import logging
import math
import random
import pandas as pd
import argparse
import csv

from typing import Tuple, List, Any
from transformers import PreTrainedTokenizerBase
from datetime import datetime, timedelta
from sample_request import (load_bird_requests,sample_bird_requests_len_range,sample_bird_requests_no_len_range)
from utils import (get_tokenizer, plot_workload, make_serializable, save_workload)

def generate_from_azure_csv_and_bird_prompts(file_path: str,
                            prompt_file_path: str,
                            duration_ms: int,
                            tokenizer: PreTrainedTokenizerBase,
                            interval_ms: int,
                            output_file: str = 'output/output.json',
                            to_jsonl: bool = False,
                        ) -> List[List[Any]]:
    # Load the CSV file
    df = pd.read_csv(file_path)

    # Ensure TIMESTAMP is a datetime object
    df['TIMESTAMP'] = pd.to_datetime(df['TIMESTAMP'])

    # Define the grouping time range (e.g., 1 second)
    time_range = timedelta(milliseconds=interval_ms)

    # Initialize a list to hold the grouped requests
    grouped_requests = []

    # Group requests by the time range
    df.set_index('TIMESTAMP', inplace=True)
    current_time = df.index.min()
    end_time = df.index.max()
    logging.warn(f"Start generation from time {current_time} to {end_time}")
   
    bird_df = load_bird_requests(dataset_path = prompt_file_path, tokenizer = tokenizer)
    current_index = 0  # Add this line to track position in dataset

    ts = 0
    while current_time <= end_time:
        # Select requests within the current time range
        mask = (df.index >= current_time) & (df.index < current_time + time_range)
        group = df.loc[mask]
        input_lens = []
        output_lens = []
        group_indices = []
        for idx, row in group.iterrows():
            input_lens.append(int(row['ContextTokens']))
            output_lens.append(int(row['GeneratedTokens']))
            group_indices.append(idx)

        sampled_requests, current_index = sample_bird_requests_no_len_range(
                df = bird_df,
                num_requests = len(input_lens),
                current_index = current_index  # Pass the current index
                )

        # Update trace token lengths for testing
        for i, req in enumerate(sampled_requests):
            if i < len(group_indices):
                df.at[group_indices[i], 'SampledInputTokens'] = req["Prompt Length"]
                df.at[group_indices[i], 'SampledOutputTokens'] = req["Output Length"]

        if sampled_requests:  # Only add non-empty groups
            grouped_requests.append({"Timestamp": ts, "Requests": sampled_requests})
        ts += interval_ms
        if ts > duration_ms:
            break
        # Move to the next time range
        current_time += time_range

    # Print or process grouped_requests as needed
    # Save to file
    grouped_requests = make_serializable(grouped_requests)
    save_workload(grouped_requests, output_file, use_jsonl = to_jsonl)
    
    return grouped_requests




def generate_from_bird_csv(trace_pattern_df: pd.DataFrame,
                            prompt_file_path: str,
                            duration_ms: int,
                            tokenizer: PreTrainedTokenizerBase,
                            interval_ms: int,
                            output_file: str = 'output/output.json',
                            to_jsonl: bool = False,
                            error_perc: float = 0.2,
                        ) -> List[List[Any]]:
    # Load the CSV file
    df = trace_pattern_df
    # Add columns for sampled token lengths if they don't exist
    if 'SampledInputTokens' not in df.columns:
        df['SampledInputTokens'] = -1
    if 'SampledOutputTokens' not in df.columns:
        df['SampledOutputTokens'] = -1

    # Ensure TIMESTAMP is a datetime object
    df['TIMESTAMP'] = pd.to_datetime(df['TIMESTAMP'])

    # Define the grouping time range (e.g., 1 second)
    time_range = timedelta(milliseconds=interval_ms)

    # Initialize a list to hold the grouped requests
    grouped_requests = []

    # Group requests by the time range
    df.set_index('TIMESTAMP', inplace=True)
    current_time = df.index.min()
    end_time = df.index.max()
    logging.warn(f"Start generation from time {current_time} to {end_time}")
   
    bird_df = load_bird_requests(dataset_path = prompt_file_path, tokenizer = tokenizer)
    
    ts = 0
    used_indices = set()  # Track used prompts across all intervals
    while current_time <= end_time:
        # Select requests within the current time range
        mask = (df.index >= current_time) & (df.index < current_time + time_range)
        group = df.loc[mask]
        input_lens = []
        output_lens = []
        group_indices = []  # Store indices for updating the dataframe
        
        for idx, row in group.iterrows():
            input_lens.append(int(row['ContextTokens']))
            output_lens.append(int(row['GeneratedTokens']))
            group_indices.append(idx)
            
        sampled_requests = sample_bird_requests_len_range(
                df = bird_df,
                num_requests = len(input_lens),
                input_lens = input_lens, 
                output_lens = output_lens,
                initial_err_perc = error_perc,
                used_indices = used_indices  
                )
        
        # Update used indices and save token lengths
        for i, req in enumerate(sampled_requests):
            # Update used indices
            prompt = req["Prompt"]
            idx = bird_df[bird_df["prompt"] == prompt].index[0]
            used_indices.add(idx)
            
            # Save sampled token lengths back to the original dataframe
            if i < len(group_indices):
                df.at[group_indices[i], 'SampledInputTokens'] = req["Prompt Length"]
                df.at[group_indices[i], 'SampledOutputTokens'] = req["Output Length"]
        
        if sampled_requests:  # Only add non-empty groups
            grouped_requests.append({"Timestamp": ts, "Requests": sampled_requests})
        ts += interval_ms
        if ts > duration_ms:
            break
        # Move to the next time range
        current_time += time_range

    # Save the updated CSV file with sampled token lengths
    df.reset_index().to_csv('output/synthetic_bird_trace.csv', index=False)
    
    # Save workload to JSON/JSONL
    grouped_requests = make_serializable(grouped_requests)
    save_workload(grouped_requests, output_file, use_jsonl = to_jsonl)
    
    return grouped_requests



def generate_from_bird_csv_burst(trace_pattern_df: pd.DataFrame,
                            prompt_file_path: str,
                            duration_ms: int,
                            tokenizer: PreTrainedTokenizerBase,
                            interval_ms: int,
                            output_file: str = 'output/output.json',
                            to_jsonl: bool = False,
                            error_perc: float = 0.2,
                        ) -> List[List[Any]]:
    # Load the trace pattern dataframe
    df = trace_pattern_df
    # Add columns for sampled token lengths if they don't exist
    if 'SampledInputTokens' not in df.columns:
        df['SampledInputTokens'] = -1
    if 'SampledOutputTokens' not in df.columns:
        df['SampledOutputTokens'] = -1

    # Ensure TIMESTAMP is a datetime object
    df['TIMESTAMP'] = pd.to_datetime(df['TIMESTAMP'])

    # Define the grouping time range (e.g., 1 second)
    time_range = timedelta(milliseconds=interval_ms)

    # Initialize a list to hold the grouped requests
    grouped_requests = []

    # Group requests by the time range
    df.set_index('TIMESTAMP', inplace=True)
    current_time = df.index.min()
    end_time = df.index.max()
    logging.warn(f"Start generation from time {current_time} to {end_time}")
   
    bird_df = load_bird_requests(dataset_path = prompt_file_path, tokenizer = tokenizer)
    
    ts = 0
    current_index = 0  # Add this line to track position in dataset

    while current_time <= end_time:
        # Select requests within the current time range
        mask = (df.index >= current_time) & (df.index < current_time + time_range)
        group = df.loc[mask]
        input_lens = []
        output_lens = []
        group_indices = []  # Store indices for updating the dataframe

        for idx, row in group.iterrows():
            input_lens.append(int(row['ContextTokens']))
            output_lens.append(int(row['GeneratedTokens']))
            group_indices.append(idx)
            
        sampled_requests, current_index = sample_bird_requests_no_len_range(
                df = bird_df,
                num_requests = len(input_lens),
                current_index = current_index  # Pass the current index
                )
        
        # Update trace token lengths for testing
        for i, req in enumerate(sampled_requests):
            if i < len(group_indices):
                df.at[group_indices[i], 'SampledInputTokens'] = req["Prompt Length"]
                df.at[group_indices[i], 'SampledOutputTokens'] = req["Output Length"]
        
        if sampled_requests:  # Only add non-empty groups
            grouped_requests.append({"Timestamp": ts, "Requests": sampled_requests})
        ts += interval_ms
        if ts > duration_ms:
            break
        # Move to the next time range
        current_time += time_range

    trace_name  = output_file.split('/')[-1]

    # Save the updated CSV file with sampled token lengths
    df.reset_index().to_csv(f'output/synthetic_{trace_name}_trace.csv', index=False)
    
    # Save workload to JSON/JSONL
    grouped_requests = make_serializable(grouped_requests)
    save_workload(grouped_requests, output_file, use_jsonl = to_jsonl)
    
    return grouped_requests



def generate_bird_trace_csv(context_tokens=None, generated_tokens=None, rows_per_combination=100, time_delta_ms=100, workload_pattern=None, burst_request_rate_pattern=None):
    """
    Generate a trace file with constant time intervals between records.
    
    Args:
        context_tokens (list): List of context token values
        generated_tokens (list): List of generated token values
        rows_per_combination (int): Number of rows to generate for each token combination
        time_delta_ms (int): Time difference between records in milliseconds
        
    Returns:
        pandas.DataFrame: Generated trace data
    """
    # Initialize lists to store data
    timestamps = []
    context_values = []
    generated_values = []
    
    # Start timestamp
    current_time = datetime.now()
    
    if workload_pattern == "input_output_pattern":
        # Generate data for each combination
        for ctx in context_tokens:
            for gen in generated_tokens:
                for _ in range(rows_per_combination):
                    timestamps.append(current_time)
                    context_values.append(ctx)
                    generated_values.append(gen)
                    current_time += timedelta(milliseconds=time_delta_ms)
    elif workload_pattern == "burst_pattern":
        for request_number in burst_request_rate_pattern:
            for _ in range(rows_per_combination):
                for _ in range(request_number):
                    timestamps.append(current_time)
                    # -1 means any context or generated tokens
                    context_values.append(-1)
                    generated_values.append(-1) # A predefined large number. Should be good for current traces. Modify if needed.
                    current_time += timedelta(milliseconds= time_delta_ms/ request_number)
    else:
        raise ValueError(f"Invalid workload pattern: {workload_pattern}")

    # Create DataFrame
    df = pd.DataFrame({
        'TIMESTAMP': timestamps,
        'ContextTokens': context_values,
        'GeneratedTokens': generated_values
    })
    
    return df   

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument('--prompt-file', type=str, required=True, help='File containing prompts.')
    parser.add_argument('--num-prompts', type=int, default=100, help='Number of prompts to sample.')
    parser.add_argument('--group-interval-seconds', type=int, default=1, help='Grouping interval seconds.')
    parser.add_argument('--trace-type', type=str, required=True, default="synthetic", help='Type of trace consumed. Choose among: synthetic, internal, azure')
    parser.add_argument('--trace-file', type=str, required=False, default=None, help='File containing original trace file csv, which workload generator depends upon to convert to workload format. This is only needed for for internal/azure trace type. ')
    parser.add_argument('--model', type=str, required=False, default="Qwen/Qwen2.5-Coder-7B-Instruct", help='Target model tokenizer.')
    parser.add_argument('--output', type=str, required=False, default="output", help='Output path to the workload.')
    parser.add_argument('--interval-ms', type=int, required=False, default=1000, help='Granularity of request injection interval in milliseconds.')
    parser.add_argument('--duration-ms', type=int, default=60000, help='Duration of the trace generated.')
    parser.add_argument('--to-jsonl', dest='to_jsonl', action='store_true', help='Set output data format to .jsonl (default .json).')
    args = parser.parse_args()

    # Generate workloads and pair with prompts
    workload_dict = {}
    tokenizer = get_tokenizer(pretrained_model_name_or_path = args.model, trust_remote_code = True)
    if args.trace_type == "bird":

        # # Manually set context (input) and generated (output) tokens
        # context_tokens = [2048, 4096]
        # generated_tokens = [32, 64]
        
        context_tokens = [4096]
        generated_tokens = [32]

        # Generate trace
        trace_df = generate_bird_trace_csv(
            context_tokens=context_tokens,
            generated_tokens=generated_tokens,
            rows_per_combination=100,
            time_delta_ms=1000,
            workload_pattern="input_output_pattern"
        )
        
        # Save to CSV
        trace_df.to_csv('output/synthetic_bird_trace.csv', index=False)
        generated_workload = generate_from_bird_csv(file_path= trace_df,
                                                     prompt_file_path = args.prompt_file, 
                                                     duration_ms = args.duration_ms, 
                                                     tokenizer = tokenizer, 
                                                     interval_ms = args.interval_ms, 
                                                     error_perc = 0.2,
                                                     output_file = f"{args.output}/bird_4096_32",
                                                     to_jsonl = args.to_jsonl)
        workload_dict["bird"] = generated_workload
    elif args.trace_type == "bird_aruze_time":
        
        # Use all context and generated tokens patterns

        generated_workload = generate_from_azure_csv_and_bird_prompts(file_path=args.trace_file, 
                                                     prompt_file_path = args.prompt_file, 
                                                     duration_ms = args.duration_ms, 
                                                     tokenizer = tokenizer, 
                                                     interval_ms = args.interval_ms, 
                                                     output_file = f"{args.output}/bird_aruze_time",
                                                     to_jsonl = args.to_jsonl)

        workload_dict["bird_aruze_time"] = generated_workload
        # Plot the workloads
        plot_workload(workload_dict, interval_ms=args.interval_ms, output_file=f"plot/bird_aruze_time.pdf")
    elif args.trace_type == "bird_burst":
        # 60s 
        # custom_request_rate_pattern = [1, 2, 4, 8, 16, 32, 16, 8, 4, 2, 1]
        # burst_interval = 60
        # trace_name = "bird_burst_interval_120s"

        
        #120s
        custom_request_rate_pattern = [1, 2, 4, 8, 16, 32]
        burst_interval = 120
        trace_name = "bird_burst_interval_120s"

        # Use all context and generated tokens patterns
        # Generate trace
        trace_df = generate_bird_trace_csv(
            rows_per_combination=burst_interval,
            time_delta_ms=1000,
            workload_pattern="burst_pattern",
            burst_request_rate_pattern=custom_request_rate_pattern
        )
        
        trace_df.to_csv(f'output/synthetic_{trace_name}_trace.csv', index=False)

        generated_workload = generate_from_bird_csv_burst(trace_pattern_df=trace_df, 
                                                     prompt_file_path = args.prompt_file, 
                                                     duration_ms = args.duration_ms, 
                                                     tokenizer = tokenizer, 
                                                     interval_ms = args.interval_ms, 
                                                     error_perc = 1,
                                                     output_file = f"{args.output}/{trace_name}",
                                                     to_jsonl = args.to_jsonl)
        workload_dict[f"{trace_name}"] = generated_workload
        # Plot the workloads
        plot_workload(workload_dict, interval_ms=args.interval_ms, output_file=f"plot/{trace_name}.pdf")
