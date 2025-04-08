import argparse
import logging
import time
import asyncio
import openai
import json
import io
import traceback
import threading
from queue import Queue


from typing import List
from utils import (load_workload, prepare_prompt, update_response)

thread_pool_size = 8
QUEUE_SIZE = thread_pool_size * 2
logging.basicConfig(level=logging.INFO)
task_queue = Queue(maxsize=QUEUE_SIZE)
session_history = {}
lock = threading.Lock()

def worker(thread_idx, client, model, send_request_func, output_file):
    """Worker function to run an asyncio event loop in a separate thread."""
    asyncio.set_event_loop(asyncio.new_event_loop())
    loop = asyncio.get_event_loop()
    while True:
        task = task_queue.get()
        if task is None:  # Stop signal
            logging.warn(f"Worker {thread_idx} exit.")
            break
        else:
            loop.run_until_complete(send_request_func(client, model, *task))
        task_queue.task_done()


def start_worker_threads(thread_idx, client, model, send_request_func, output_file):
    """Start multiple threads, each running an event loop for handling tasks."""
    thread = threading.Thread(target=worker, args=(thread_idx, client, model, send_request_func, output_file), daemon=True)
    thread.start()
    return thread



async def send_request_streaming(client: openai.AsyncOpenAI,
                             model: str,
                             prompt: str,
                             output_file: str,
                             request_id: int,
                             session_id: str,
                             target_time: int,
                             ):
    start_time = asyncio.get_event_loop().time()
    first_response_time = None
    target_pod = ""
    target_request_id = ""
    try:
        cur_time = time.time()
        logging.warning(f"send_request_streaming: Prepare to launch task after {target_time - cur_time}")
        response_stream = await client.chat.completions.create(
            model=model,
            messages=prompt,
            temperature=0,
            max_tokens=2048,
            stream=True,
            stream_options={"include_usage": True},
        )
        if hasattr(response_stream, 'response') and hasattr(response_stream.response, 'headers'):
            target_pod = response_stream.response.headers.get('target-pod')
            target_request_id = response_stream.response.headers.get('request-id')

        text_chunks = []
        prompt_tokens = 0
        output_tokens = 0
        total_tokens = 0

        try:
            async for chunk in response_stream:
                if chunk.choices:
                    if chunk.choices[0].delta.content is not None:
                        if not first_response_time:
                            first_response_time = asyncio.get_event_loop().time()
                        output_text = chunk.choices[0].delta.content
                        text_chunks.append(output_text)
                if hasattr(chunk, 'usage') and chunk.usage is not None:
                    # For OpenAI, we expect to get complete usage stats, not partial ones to accumulate
                    # So we can safely overwrite previous values if they exist
                    if chunk.usage.prompt_tokens is not None:
                        prompt_tokens = chunk.usage.prompt_tokens
                    if chunk.usage.completion_tokens is not None:
                        output_tokens = chunk.usage.completion_tokens
                    if chunk.usage.total_tokens is not None:
                        total_tokens = chunk.usage.total_tokens
        except Exception as stream_error:
            # Handle errors during streaming
            logging.error(f"Request {request_id}: Stream interrupted: {type(stream_error).__name__}: {str(stream_error)}")

        response_text = "".join(text_chunks)
        response_time = asyncio.get_event_loop().time()
        latency = response_time - start_time
        throughput = output_tokens / latency if output_tokens > 0 else 0
        ttft = first_response_time - start_time if first_response_time else None
        tpot = (response_time - first_response_time) / output_tokens if first_response_time and output_tokens > 0 else None

        update_response(response = response_text, lock = lock, session_id = session_id, history = session_history)
        
        result = {
            "request_id": request_id,
            "status": "success",
            "input": prompt,
            "output": response_text,
            "prompt_tokens": prompt_tokens,
            "output_tokens": output_tokens,
            "total_tokens": total_tokens,
            "latency": latency,
            "throughput": throughput,
            "start_time": start_time,
            "end_time": response_time,
            "ttft": ttft,
            "tpot": tpot,
            "target_pod": target_pod,
            "target_request_id": target_request_id,
            "session_id": session_id,
        }

        # Write result to JSONL file
        logging.info(f"Request {request_id}: Completed successfully. Tokens: {total_tokens}, Latency: {latency:.2f}s")
        output_file.write(json.dumps(result) + "\n")
        output_file.flush()  # Ensure data is written immediately to the file
        return result

    except Exception as e:
        error_time = asyncio.get_event_loop().time()
        # Determine error type based on exception class
        error_type = type(e).__name__
        error_result = {
            "request_id": request_id,
            "status": "error",
            "error_type": error_type,
            "error_message": str(e),
            "error_traceback": traceback.format_exc(),
            "input": prompt,
            "latency": error_time - start_time,
            "start_time": start_time,
            "end_time": error_time,
            "target_pod": target_pod,
            "target_request_id": target_request_id,
            "session_id": session_id,
        }
        logging.error(f"Request {request_id}: Error ({error_type}): {str(e)}")
        output_file.write(json.dumps(error_result) + "\n")
        output_file.flush()
        return error_result

async def benchmark_streaming(api_key: str,
                              endpoint: str,
                              max_retries: int,
                              timeout: float,
                              routing_strategy: str,
                              load_struct: List,
                              output_file: io.TextIOWrapper,
                              model: str,
                              ):
    request_id = 0
    base_time = time.time()
    num_requests = 0
    threads = []
    for thread_idx in range(0, thread_pool_size):
        client = create_client(api_key, endpoint, max_retries, timeout, routing_strategy)
        threads.append(start_worker_threads(thread_idx, client, model, send_request_streaming, output_file))
    for requests_dict in load_struct:
        ts = int(requests_dict["timestamp"])
        requests = requests_dict["requests"]
        target_time = base_time + ts / 1000.0
        formatted_prompts = [prepare_prompt(prompt = request["prompt"], lock = lock, session_id = request.get("session_id", None), history = session_history) for request in requests]
        for i in range(len(requests)):
            session_id = requests[i].get("session_id", None)
            task_queue.put((formatted_prompts[i], output_file, request_id, session_id, target_time))
            request_id += 1
        num_requests += len(requests)
    task_queue.join()
    # Stop all worker threads
    logging.warn("Producer completed ...")
    for _ in threads:
        task_queue.put(None)

    for thread in threads:
        thread.join()
        logging.warn(f"Worker thread {thread} completed ...")
    logging.warning(f"All {num_requests} requests completed for deployment.")

# Asynchronous request handler
async def send_request_batch(client: openai.AsyncOpenAI,
                             model: str,
                             prompt: str,
                             output_file: str,
                             request_id: int,
                             session_id: str, 
                             target_time: int,
                             ):
    start_time = asyncio.get_event_loop().time()
    target_pod = ""
    try:
        cur_time = time.time()
        logging.warning(f"send_request_batch: Prepare to launch task after {target_time - cur_time}")
        if target_time > cur_time:
            await asyncio.sleep(target_time - cur_time)
        response = await client.chat.completions.create(
            model=model,
            messages=prompt,
            temperature=0,
            max_tokens=2048
        )
        if hasattr(response, 'response') and hasattr(response.response, 'headers'):
            target_pod = response.response.headers.get('target-pod')

        response_time = asyncio.get_event_loop().time()
        latency = response_time - start_time
        prompt_tokens = response.usage.prompt_tokens
        output_tokens = response.usage.completion_tokens
        total_tokens = response.usage.total_tokens
        throughput = output_tokens / latency
        output_text = response.choices[0].message.content

        update_response(response = output_text, lock = lock, session_id = session_id, history = session_history)
        
        result = {
            "request_id": request_id,
            "status": "success",
            "input": prompt,
            "output": output_text,
            "prompt_tokens": prompt_tokens,
            "output_tokens": output_tokens,
            "total_tokens": total_tokens,
            "latency": latency,
            "throughput": throughput,
            "start_time": start_time,
            "end_time": response_time,
            "ttft": "Unknown",
            "tpot": "Unknown",
            "target_pod": target_pod,
            "session_id": session_id,
        }
        logging.info(result)
        # Write result to JSONL file
        output_file.write(json.dumps(result) + "\n")
        output_file.flush()  # Ensure data is written immediately to the file
        return result

    except Exception as e:
        error_time = asyncio.get_event_loop().time()
        error_type = type(e).__name__
        error_result = {
            "request_id": request_id,
            "status": "error",
            "error_type": error_type,
            "error_message": str(e),
            "error_traceback": traceback.format_exc(),
            "input": prompt,
            "latency": error_time - start_time,
            "start_time": start_time,
            "end_time": error_time,
            "target_pod": target_pod,
            "session_id": session_id,
        }
        logging.error(f"Request {request_id}: Error ({error_type}): {str(e)}")
        output_file.write(json.dumps(error_result) + "\n")
        output_file.flush()
        return error_result


async def benchmark_batch(api_key: str,
                          endpoint: str,
                          max_retries: int,
                          timeout: float,
                          routing_strategy: str,
                          load_struct: List,
                          output_file: io.TextIOWrapper,
                          model: str,
                          ):
    request_id = 0
    base_time = time.time()
    num_requests = 0
    threads = []
    
    for thread_idx in range(0, thread_pool_size):
        client = create_client(api_key, endpoint, max_retries, timeout, routing_strategy)
        threads.append(start_worker_threads(thread_idx, client, model, send_request_batch, output_file))
    for requests_dict in load_struct:
        ts = int(requests_dict["timestamp"])
        requests = requests_dict["requests"]
        target_time = base_time + ts / 1000.0
        formatted_prompts = [prepare_prompt(prompt = request["prompt"], lock = lock, session_id = request.get("session_id", None), history = session_history) for request in requests]
        for i in range(len(requests)):
            session_id = requests[i].get("session_id", None)
            task_queue.put((formatted_prompts[i], output_file, request_id, session_id, target_time))
            request_id += 1
        num_requests += len(requests)
    task_queue.join()
    # Stop all worker threads
    for _ in threads:
        task_queue.put(None)

    for thread in threads:
        thread.join()
        logging.warn(f"Worker thread {thread} completed ...")
    logging.warning(f"All {num_requests} requests completed for deployment.")

def create_client(api_key: str,
                  endpoint: str,
                  max_retries: int,
                  timeout: float,
                  routing_strategy: str,
                  ):
    if args.api_key is None:
        client = openai.AsyncOpenAI(
            base_url=args.endpoint + "/v1",
            max_retries=max_retries,
            timeout=timeout,
        )
    else:
        client = openai.AsyncOpenAI(
            api_key=args.api_key,
            base_url=args.endpoint + "/v1",
            max_retries=max_retries,
            timeout=timeout,
        )
    if routing_strategy is not None:
        client = client.with_options(
            default_headers={"routing-strategy": routing_strategy}
        )
    return client

def main(args):
    logging.info(f"Starting benchmark on endpoint {args.endpoint}")
    with open(args.output_file_path, 'w', encoding='utf-8') as output_file:
        load_struct = load_workload(args.workload_path)
        if not args.streaming:
            logging.info("Using batch client")
            start_time = time.time()
            asyncio.run(benchmark_batch(
                api_key = args.api_key,
                endpoint = args.endpoint,
                max_retries = 0,
                timeout = 60.0,
                routing_strategy = args.routing_strategy,
                load_struct=load_struct,
                output_file=output_file,
                model=args.model,
            ))
            end_time = time.time()
            logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")
        else:
            logging.info("Using streaming client")
            start_time = time.time()
            asyncio.run(benchmark_streaming(
                api_key = args.api_key,
                endpoint = args.endpoint,
                max_retries = 0,
                timeout = 60.0,
                routing_strategy = args.routing_strategy,
                load_struct=load_struct,
                output_file=output_file,
                model=args.model,
            ))
            end_time = time.time()
            logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument("--workload-path", type=str, default=None, help="File path to the workload file.")
    parser.add_argument("--model", type=str, default=None, help="Default target model (if workload does not contains target model).")
    parser.add_argument('--endpoint', type=str, required=True)
    parser.add_argument("--api-key", type=str, default=None, help="API key to the service. ")
    parser.add_argument('--output-file-path', type=str, default="output.jsonl")
    parser.add_argument("--streaming", action="store_true", help="Use streaming client.")
    parser.add_argument("--routing-strategy", type=str, required=False, default="random", help="Routing strategy to use.")

    args = parser.parse_args()
    main(args)
