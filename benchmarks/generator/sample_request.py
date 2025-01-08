import logging
import json
import sys

import pandas as pd

from typing import Tuple, Optional, List
from transformers import PreTrainedTokenizerBase


def load_sharegpt_requests(
        dataset_path: str,
        tokenizer: PreTrainedTokenizerBase,
) -> pd.DataFrame:
    # Load the dataset into a DataFrame
    with open(dataset_path, encoding='utf-8') as f:
        dataset = json.load(f)
    dataset = [
        (data["conversations"][0]["value"], data["conversations"][1]["value"])
        for data in dataset if len(data["conversations"]) >= 2
    ]
    df = pd.DataFrame(dataset, columns=["prompt", "completion"])
    logging.warn(f"...Start dataframe transformation")
    # Tokenize and calculate lengths
    df["prompt_len"] = df["prompt"].apply(lambda x: len(tokenizer(x).input_ids))
    df["completion_len"] = df["completion"].apply(lambda x: len(tokenizer(x).input_ids))
    logging.warn(f"...Complete dataframe transformation")
    return df


def load_bird_requests(
    dataset_path: str,
    tokenizer: PreTrainedTokenizerBase,
    ) -> pd.DataFrame:
    # Load the dataset into a DataFrame
    logging.warning(f"dataset_path: {dataset_path}")  # Add this line
    df_raw = pd.read_json(dataset_path, lines=True)
     # Print number of rows
    logging.warn(f"Number of rows in df_raw: {len(df_raw)}")

    # Extract prompt and completion into new DataFrame
    dataset = []
    for data in df_raw.to_dict(orient="records"):
        messages = data["messages"]
        # Convert metadata string to dict if it's a string
        metadata = json.loads(data["metadata"]) if isinstance(data["metadata"], str) else data["metadata"]
        
        if len(messages) >= 2:
            dataset.append((messages[1]["content"], metadata["gt_sql"]))
        
    df = pd.DataFrame(dataset, columns=["prompt", "completion"])
    logging.warn(f"...Start dataframe transformation")
    # Tokenize and calculate lengths
    df["prompt_len"] = df["prompt"].apply(lambda x: len(tokenizer(x).input_ids))
    df["completion_len"] = df["completion"].apply(lambda x: len(tokenizer(x).input_ids))
    logging.warning(f"DataFrame size: {len(df)}")  # Add this line
    # logging.warning(f"First row: {df.iloc[0] if len(df) > 0 else 'Empty DataFrame'}")
    logging.warn(f"...Complete dataframe transformation")
    return df


def sample_sharegpt_requests(
        dataset_path: str,
        num_requests: int,
        tokenizer: Optional[PreTrainedTokenizerBase] = None,
        fixed_output_len: Optional[int] = None,
) -> List[Tuple[str, int, int, None]]:
    # Load the dataset
    with open(dataset_path, encoding='utf-8') as f:
        dataset = json.load(f)
    dataset = [data for data in dataset if len(data["conversations"]) >= 2]
    dataset = [(data["conversations"][0]["value"], data["conversations"][1]["value"]) for data in dataset]

    filtered_dataset: List[Tuple[str, int, int]] = []
    for i in range(len(dataset)):
        if len(filtered_dataset) == num_requests:
            break
        prompt = dataset[i][0]
        if tokenizer is not None:
            prompt_token_ids = tokenizer(prompt).input_ids
            completion = dataset[i][1]
            completion_token_ids = tokenizer(completion).input_ids
            prompt_len = len(prompt_token_ids)
            output_len = len(completion_token_ids) if fixed_output_len is None else fixed_output_len
            if prompt_len < 4 or (fixed_output_len is None and output_len < 4):
                continue
            if prompt_len > 1024 or prompt_len + output_len > 2048:
                continue
            filtered_dataset.append({"prompt": prompt,
                                     "prompt_length": prompt_len,
                                     "output_length": output_len})
        else:
            filtered_dataset.append({"prompt": prompt,
                                     "prompt_length": -1,
                                     "output_length": -1})

    return filtered_dataset


def sample_sharegpt_requests_len_range(
        df: pd.DataFrame,
        num_requests: int,
        input_lens: List[int],
        output_lens: List[int],
        initial_err_perc: Optional[float] = 0.5,
        err_step: float = 0.05
) -> List[Tuple[str, int, int, None]]:
    filtered_results = []

    # Relaxation mechanism
    for i in range(num_requests):
        input_len = input_lens[i]
        output_len = output_lens[i]
        err_perc = initial_err_perc
        while err_perc >= 0:
            input_range = range(0, sys.maxsize)
            output_range = range(0, sys.maxsize)
            if input_len is not None:
                input_range = (int(input_len * err_perc), int(input_len * (1 + err_perc)))
            if output_len is not None:
                output_range = (int(output_len * err_perc), int(output_len * (1 + err_perc)))

            filtered = df[
                (df["prompt_len"] >= input_range[0]) &
                (df["prompt_len"] <= input_range[1]) &
                (df["completion_len"] >= output_range[0]) &
                (df["completion_len"] <= output_range[1])
                ]

            if not filtered.empty:
                # Select the first match or random sample
                sample = filtered.iloc[0]  # Or filtered.sample(1) for random
                filtered_results.append({"prompt": sample["prompt"],
                                         "prompt_length": sample["prompt_len"],
                                         "output_length": sample["completion_len"]})
                break  # Stop relaxing for this request once a match is found

            # Reduce err_perc for next iteration
            logging.warn(f"Relax err_perc {err_perc} by {err_step}")
            err_perc -= err_step

        if err_perc < 0:
            raise Exception(f"No match found for request {i + 1} even after relaxing err_perc to 0")

    return filtered_results

def sample_bird_requests_len_range(
    df: pd.DataFrame,
    num_requests: int,
    input_lens: List[int],
    output_lens: List[int],
    initial_err_perc: Optional[float] = 0.5,
    used_indices: Optional[set] = None
) -> List[Tuple[str, int, int, None]]:
    """
    Sample bird requests based on input and output length ranges, 
    with deduplication logic to ensure no duplicate rows are selected, if possible. There will be duplicates if availabe rows are fewer than num_requests. 
    """
    filtered_results = []
    used_indices = used_indices if used_indices is not None else set()  # Use provided set or create new

    # Relaxation mechanism
    for i in range(num_requests):
        input_len = input_lens[i]
        output_len = output_lens[i]
        err_perc = initial_err_perc

        while err_perc >= 0:
            input_range = (int(input_len * (1 - err_perc)), int(input_len * (1 + err_perc)))
            output_range = (int(output_len * (1 - err_perc)), int(output_len * (1 + err_perc)))

            # Filter for matching rows within the specified ranges
            filtered = df[
                (df["prompt_len"] >= input_range[0]) & 
                (df["prompt_len"] <= input_range[1]) &
                (df["completion_len"] >= output_range[0]) & 
                (df["completion_len"] <= output_range[1])
            ]


            if len(filtered) == 0:
                logging.warn(f"input_len: {input_len}, output_len: {output_len}, 0 prompts found under err_perc {err_perc}")
                break

            # Exclude already used rows
            unused_filtered = filtered.loc[~filtered.index.isin(used_indices)]

            if len(unused_filtered) > 0:
                # Use first unused row
                idx = unused_filtered.index[0]
                sample = unused_filtered.loc[idx]
                used_indices.add(idx)
            else:
                # If no unused rows, reuse an existing row
                logging.warn(f"input_len: {input_len}, output_len: {output_len}, Reusing an existing row as no unused rows found within error threshold {err_perc}")
                idx = filtered.sample(n=1).index[0]  # Randomly select one row
                sample = filtered.loc[idx]

            filtered_results.append({
                    "Prompt": sample["prompt"], 
                    "Prompt Length": sample["prompt_len"], 
                    "Output Length": sample["completion_len"]
                })
            break  # Found a match, move to next request

        if err_perc < 0:
            logging.error(f"No match found for request {i + 1} even after relaxing err_perc to 0")
            raise Exception(f"No match found for request {i + 1} even after relaxing err_perc to 0")

    return filtered_results

def sample_bird_requests_no_len_range(
    df: pd.DataFrame,
    num_requests: int,
    current_index: int,
    used_indices: Optional[set] = None
) -> List[Tuple[str, int, int, None]]:
    """
    Sample bird requests based on input and output length ranges, 
    with deduplication logic to ensure no duplicate rows are selected, if possible. There will be duplicates if availabe rows are fewer than num_requests. 
    """
    filtered_results = []
    used_indices = used_indices if used_indices is not None else set()  # Use provided set or create new



    total_rows = len(df)

    for i in range(num_requests):
        if current_index >= total_rows:
            # Shuffle the DataFrame when we've used all rows
            df = df.sample(frac=1).reset_index(drop=True)
            current_index = 0
            logging.warn("Reached end of dataset, shuffling and starting over")

        sample = df.iloc[current_index]
        filtered_results.append({
            "Prompt": sample["prompt"],
            "Prompt Length": sample["prompt_len"],
            "Output Length": sample["completion_len"]
        })
        current_index += 1

    return filtered_results, current_index
