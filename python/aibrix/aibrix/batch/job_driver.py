# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import asyncio
import uuid
from typing import Optional
from urllib.parse import urljoin

import httpx

import aibrix.batch.storage as storage
from aibrix.batch.job_entity import BatchJob, BatchJobState
from aibrix.batch.job_manager import JobManager
from aibrix.logger import init_logger
from python.aibrix.aibrix.batch.job_entity.batch_job import BatchJobError, BatchJobErrorCode

logger = init_logger(__name__)


class InferenceEngineClient:
    async def inference_request(self, endpoint: str, request_data):
        """Send inference request to the LLM engine."""
        await asyncio.sleep(1)  # Simulate processing time
        return request_data


class ProxyInferenceEngineClient(InferenceEngineClient):
    def __init__(self, base_url: str):
        """
        Initiate client to inference engine.
        """
        self.base_url = base_url

    async def inference_request(self, endpoint: str, request_data):
        """Real inference request to LLM engine."""
        url = urljoin(self.base_url, endpoint)

        logger.debug("requesting inference", url=url, body=request_data)

        async with httpx.AsyncClient() as client:
            response = await client.post(url, json=request_data, timeout=30.0)
            response.raise_for_status()
            return response.json()


class JobDriver:
    def __init__(
        self,
        manager: JobManager,
        inference_client: Optional[InferenceEngineClient] = None,
    ) -> None:
        """ """
        self._job_manager = manager
        if inference_client is None:
            self._inference_client = InferenceEngineClient()
        else:
            self._inference_client = inference_client

    async def execute_job(self, job_id):
        """
        Execute complete job workflow: prepare -> execute -> finalize.
        This function executes all three steps.
        """
        job = self._job_manager.get_job(job_id)
        if job is None:
            logger.warning("Job not found", job_id=job_id)
            return

        # Check if temp file IDs exist to determine if we should skip steps 1 and 3
        has_temp_files = (
            job.status.temp_output_file_id and job.status.temp_error_file_id
        )

        if not has_temp_files:
            # Step 1: Prepare job output files
            await storage.prepare_job_ouput_files(job)

        # Step 2: Execute worker (core execution)
        await self.execute_worker(job_id)

        if not has_temp_files:
            # Step 3: Aggregate outputs
            job = self._job_manager.get_job(job_id)
            if job and job.status.state == BatchJobState.FINALIZING:
                await storage.finalize_job_output_data(job)
                logger.debug("Completed job", job_id=job_id)
                self.sync_job_status(job_id)

    async def execute_worker(self, job_id):
        """
        Execute worker logic: process requests without file preparation or finalization.
        This function only executes step 2 (the core execution loop).
        """
        # Verify job status and get minimum unfinished request id
        request_id = self._job_manager.get_job_next_request(job_id)
        if request_id == -1:
            logger.warning(
                "Job has something wrong with metadata in job manager, nothing left to execute",
                job_id=job_id,
            )
            return

        job = self._job_manager.get_job(job_id)

        if request_id == 0:
            logger.debug("Start processing job", job_id=job_id)
        else:
            logger.debug("Resuming job", job_id=job_id, request_id=request_id)

        # Step 2: Execute requests, resumable.
        line_no = request_id
        async for request in storage.read_job_next_request(job, request_id):
            logger.debug(
                "Read job request, checking completion status",
                job_id=job_id,
                line=line_no,
                next_unfinished=request_id,
            )
            # Skip completed requests
            if line_no < request_id:
                continue
            
            custom_id = request.get("custom_id")
            logger.debug("Executing job request", job_id=job_id, request_id=request_id, custom_id=custom_id)

            # Retry inference request up to 3 times
            request_output = None
            last_error = None
            for attempt in range(3):
                try:
                    request_output = await self._inference_client.inference_request(
                        job.spec.endpoint.value, request["body"]
                    )
                    break  # Success, exit retry loop
                except Exception as e:
                    last_error = e
                    logger.warning(
                        f"Inference request failed (attempt {attempt + 1}/3): {e}",
                        job_id=job_id,
                        request_id=request_id,
                    )
                    if attempt < 2:  # Don't sleep on last attempt
                        await asyncio.sleep(1 * (attempt + 1))  # Exponential backoff

            response = {
                "id": uuid.uuid4().hex[:5],
            }
            if custom_id:
                response["custom_id"] = custom_id
            if last_error is not None:
                logger.error(
                    f"All inference attempts failed after 3 retries: {last_error}",
                    job_id=job_id,
                    request_id=request_id,
                )
                response["error"] = BatchJobError(code=BatchJobErrorCode.INFERENCE_FAILED, message=str(last_error))
            else:
                response["response"] = request_output

            await storage.write_job_output_data(job, request_id, [response])
            # Request next id to avoid state becoming FINALIZING by make total > request_id
            logger.debug("Job request executed", job_id=job_id, request_id=request_id)
            job = self.sync_job_status(job_id, request_id)

            request_id = self._job_manager.get_job_next_request(job_id)
            line_no += 1

        job = self.sync_job_status(
            job_id, request_id + 1
        )  # Now that total == request_id
        logger.debug(
            "Worker completed, job state:",
            job_id=job_id,
            total=job.status.request_counts.total if job else None,
            state=job.status.state.value if job else None,
        )

    def store_output(self, output_id, request_id, result):
        """
        Write the request result back to storage.
        """
        storage.put_job_results(output_id, request_id, [result])

    def sync_job_status(self, job_id, reqeust_id=-1) -> Optional[BatchJob]:
        """
        Update job's status back to job manager.
        """
        if reqeust_id < 0:
            return self._job_manager.mark_job_done(job_id)
        else:
            return self._job_manager.mark_job_progress(job_id, [reqeust_id])
