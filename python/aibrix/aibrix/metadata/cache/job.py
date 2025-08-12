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
from pathlib import Path
from typing import Any, Callable, Coroutine, Dict, List, Optional

import kopf
import yaml
from kubernetes import client
from kubernetes.client.rest import ApiException

import aibrix.batch.storage.batch_metastore as metastore
import aibrix.batch.storage.batch_storage as storage
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    JobAnnotationKey,
    JobEntityManager,
    k8s_job_to_batch_job,
)
from aibrix.logger import init_logger
from aibrix.storage import StorageType

from .utils import merge_yaml_object

# If you installed kopf[uvloop], kopf will likely set this up.
# Otherwise, you can explicitly set it:
# import uvloop
# asyncio.set_event_loop_policy(uvloop.EventLoopPolicy())


# Global logger for standalone functions
logger = init_logger(__name__)

# Global JobCache instance for kopf handlers
_global_job_cache: Optional["JobCache"] = None

def set_global_job_cache(job_cache: "JobCache") -> None:
    """Set the global job cache instance for kopf handlers."""
    global _global_job_cache
    _global_job_cache = job_cache


def get_global_job_cache() -> Optional["JobCache"]:
    """Get the global job cache instance."""
    return _global_job_cache


class JobCache(JobEntityManager):
    """Kubernetes-based job cache implementing JobEntityManager interface.

    This class uses kopf to watch Kubernetes Job resources and maintains
    an in-memory cache of BatchJob objects. It implements the JobEntityManager
    interface to provide standardized job management capabilities.
    """

    def __init__(self) -> None:
        """Initialize the job cache."""
        # Cache of BatchJob objects keyed by batch ID (K8s UID)
        self.active_jobs: Dict[str, BatchJob] = {}

        # Register this instance as the global job cache for kopf handlers
        set_global_job_cache(self)

        # Callback handlers for job lifecycle events
        self._job_committed_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_updated_handler: Optional[
            Callable[[BatchJob, BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_deleted_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = None

        # Load Kubernetes Job template once at initialization
        template_path = Path(__file__).parent.parent / "setting"
        try:
            path = template_path / "k8s_job_template.yaml"
            with open(path, "r") as f:
                self.job_template = yaml.safe_load(f)
            logger.info(
                "Kubernetes Job template loaded successfully",
                template_path=str(path),
            )  # type: ignore[call-arg]

            self.storage_patches = {}
            path = template_path / "k8s_job_s3_patch.yaml"
            with open(path, "r") as f:
                self.storage_patches[StorageType.S3.value] = yaml.safe_load(f)
            logger.info(
                "Kubernetes Job storage patch loaded successfully",
                template_path=str(path),
                storage=StorageType.S3,
            )  # type: ignore[call-arg]

            path = template_path / "k8s_job_tos_patch.yaml"
            with open(path, "r") as f:
                self.storage_patches[StorageType.TOS.value] = yaml.safe_load(f)
            logger.info(
                "Kubernetes Job storage patch loaded successfully",
                template_path=str(path),
                storage=StorageType.TOS,
            )  # type: ignore[call-arg]

            path = template_path / "k8s_job_redis_patch.yaml"
            with open(path, "r") as f:
                self.storage_patches[StorageType.REDIS.value] = yaml.safe_load(f)
            logger.info(
                "Kubernetes Job storage patch loaded successfully",
                template_path=str(path),
                storage=StorageType.REDIS,
            )  # type: ignore[call-arg]
        except FileNotFoundError:
            logger.error(
                "Kubernetes Job template not found",
                template_path=str(path),
                operation="__init__",
            )  # type: ignore[call-arg]
            raise RuntimeError(f"Job template not found at {template_path}")
        except yaml.YAMLError as e:
            logger.error(
                "Failed to parse Kubernetes Job template",
                error=str(e),
                template_path=str(path),
                operation="__init__",
            )  # type: ignore[call-arg]
            raise RuntimeError(f"Invalid YAML in job template: {e}")

        self.batch_v1_api = client.BatchV1Api()

    def is_scheduler_enabled(self) -> bool:
        """Check if JobEntityManager has own scheduler enabled."""
        return True

    # Implementation of JobEntityManager abstract methods
    def get_job(self, job_id: str) -> Optional[BatchJob]:
        """Get cached job detail by batch id.

        Args:
            job_id: Batch id (Kubernetes UID).

        Returns:
            BatchJob: Job detail.

        Raises:
            KeyError: If job with given job_id is not found.
        """
        if job_id not in self.active_jobs:
            return None
        return self.active_jobs[job_id]

    def list_jobs(self) -> List[BatchJob]:
        """List unarchived jobs that cached locally.

        Returns:
            List[BatchJob]: List of jobs.
        """
        return list(self.active_jobs.values())

    async def submit_job(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        job_name: Optional[str] = None,
        parallelism: int = 1,
        prepared_job: Optional[BatchJob] = None,
    ) -> None:
        """Submit job by creating a Kubernetes Job.

        Args:
            job_spec: BatchJobSpec to submit to Kubernetes.
            job_name: Optional job name, will generate one if not provided.
            prepared_job: Optional BatchJob with file IDs to add to pod annotations.

        Raises:
            RuntimeError: If Kubernetes client is not available.
            ApiException: If Kubernetes API call fails.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        try:
            # Convert BatchJobSpec to Kubernetes Job manifest
            k8s_job = self._batch_job_spec_to_k8s_job(
                session_id, job_spec, job_name, parallelism, prepared_job
            )

            # Get namespace from k8s_job, use default if not specified
            namespace = k8s_job["metadata"].get("namespace") or "default"
            created_job = await asyncio.to_thread(
                self.batch_v1_api.create_namespaced_job,
                namespace=namespace,
                body=k8s_job,
            )

            logger.info(  # type: ignore[call-arg]
                "Job submitted to Kubernetes",
                job_name=created_job.metadata.name,
                namespace=namespace,
                k8s_uid=created_job.metadata.uid,
                input_file_id=job_spec.input_file_id,
                endpoint=job_spec.endpoint,
            )  # type: ignore[call-arg]

        except ApiException as e:
            logger.error(  # type: ignore[call-arg]
                "Failed to submit job to Kubernetes",
                input_file_id=job_spec.input_file_id,
                endpoint=job_spec.endpoint,
                error=str(e),
                status_code=e.status,
                reason=e.reason,
            )  # type: ignore[call-arg]
            raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error submitting job",
                input_file_id=job_spec.input_file_id,
                endpoint=job_spec.endpoint,
                error=str(e),
                operation="submit_job",
            )  # type: ignore[call-arg]
            raise

    async def update_job_ready(self, job: BatchJob):
        """Update job by marking it ready info in the persist store.
        The job suspend flag will be removed to start the execution.

        Args:
            job (BatchJob): Job to update.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        try:
            # Convert BatchJobSpec to Kubernetes Job manifest
            patch_body = self._batch_job_to_k8s_job_patch(job)

            # Get namespace from k8s_job, use default if not specified
            namespace = job.metadata.namespace or "default"
            await asyncio.to_thread(
                self.batch_v1_api.patch_namespaced_job,
                name=job.metadata.name,
                namespace=namespace,
                body=patch_body,
            )

            logger.info(  # type: ignore[call-arg]
                "Job patched to Kubernetes",
                job_name=job.metadata.name,
                namespace=namespace,
                patch_body=patch_body,
            )  # type: ignore[call-arg]

        except ApiException as e:
            logger.error(  # type: ignore[call-arg]
                "Failed to patch job to Kubernetes",
                job_name=job.metadata.name,
                namespace=namespace,
                patch_body=patch_body,
                error=str(e),
                status_code=e.status,
                reason=e.reason,
            )  # type: ignore[call-arg]
            raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error patch job",
                job_name=job.metadata.name,
                namespace=namespace,
                patch_body=patch_body,
                error=str(e),
                operation="submit_job",
            )  # type: ignore[call-arg]
            raise

    async def cancel_job(self, job_id: str) -> None:
        """Cancel job by deleting the Kubernetes Job.

        Args:
            job_id: Job ID (batch ID) to cancel.

        Raises:
            RuntimeError: If Kubernetes client is not available.
            KeyError: If job is not found in cache.
            ApiException: If Kubernetes API call fails.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        # Get job from cache to find namespace and name
        job = self.get_job(job_id)
        if not job:
            logger.error("Job not found for cancellation", job_id=job_id)  # type: ignore[call-arg]
            raise KeyError(f"Job with ID '{job_id}' not found")

        namespace = job.metadata.namespace or "default"
        job_name = job.metadata.name
        try:
            # Delete the Kubernetes Job
            await asyncio.to_thread(
                self.batch_v1_api.delete_namespaced_job,
                name=job_name,
                namespace=namespace,
                propagation_policy="Foreground",  # Delete pods too
            )

            logger.info(  # type: ignore[call-arg]
                "Job deletion requested in Kubernetes",
                job=job_name,
                namespace=namespace,
                job_id=job_id,
            )
        except ApiException as e:
            if e.status == 404:
                logger.warning(  # type: ignore[call-arg]
                    "Job not found in Kubernetes for cancellation",
                    job=job_name,
                    namespace=namespace,
                    job_id=job_id,
                )
            else:
                logger.error(  # type: ignore[call-arg]
                    "Failed to cancel job in Kubernetes",
                    job=job_name,
                    namespace=namespace,
                    job_id=job_id,
                    error=str(e),
                    status_code=e.status,
                    reason=e.reason,
                )
                raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error canceling job",
                job=job_name,
                namespace=namespace,
                job_id=job_id,
                error=str(e),
                operation="cancel_job",
            )
            raise

    async def delete_job(self, job: BatchJob) -> None:
        """Cancel job by deleting the Kubernetes Job.

        Args:
            job_id: Job ID (batch ID) to cancel.

        Raises:
            RuntimeError: If Kubernetes client is not available.
            KeyError: If job is not found in cache.
            ApiException: If Kubernetes API call fails.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        namespace = job.metadata.namespace or "default"
        job_name = job.metadata.name
        try:
            # Delete the Kubernetes Job
            await asyncio.to_thread(
                self.batch_v1_api.delete_namespaced_job,
                name=job_name,
                namespace=namespace,
                propagation_policy="Foreground",  # Delete pods too
            )

            logger.info(  # type: ignore[call-arg]
                "Job deletion requested in Kubernetes",
                job_id=job.job_id,
                job=job_name,
                namespace=namespace,
            )
        except ApiException as e:
            if e.status == 404:
                logger.warning(  # type: ignore[call-arg]
                    "Job not found in Kubernetes for deletion",
                    job=job_name,
                    namespace=namespace,
                )
            else:
                logger.error(  # type: ignore[call-arg]
                    "Failed to delete job in Kubernetes",
                    job_id=job.job_id,
                    job=job_name,
                    namespace=namespace,
                    error=str(e),
                    status_code=e.status,
                    reason=e.reason,
                )
                raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error deleting job",
                job_id=job.job_id,
                job=job_name,
                namespace=namespace,
                error=str(e),
                operation="delete_job",
            )
            raise

    def _batch_job_to_k8s_job_patch(self, job: BatchJob) -> Dict[str, Any]:
        """Convert BatchJob to Kubernetes Job patch manifest. Only annotations will be patched.

        Args:
            job_spec: BatchJob to convert.

        Returns:
            patch body object.
        """
        # Use pre-loaded template (deep copy to avoid modifying the original)
        patch_body = {
            "spec": {
                "template": {
                    "metadata": {
                        "annotations": {
                            JobAnnotationKey.OUTPUT_FILE_ID: job.status.output_file_id,
                            JobAnnotationKey.TEMP_OUTPUT_FILE_ID: job.status.temp_output_file_id,
                            JobAnnotationKey.ERROR_FILE_ID: job.status.error_file_id,
                            JobAnnotationKey.TEMP_ERROR_FILE_ID: job.status.temp_error_file_id,
                        },
                    },
                },
                "suspend": False,
            },
        }
        return patch_body

    def _batch_job_spec_to_k8s_job(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        job_name: Optional[str] = None,
        parallelism: int = 1,
        prepared_job: Optional[BatchJob] = None,
    ) -> Dict[str, Any]:
        """Convert BatchJobSpec to Kubernetes Job manifest using pre-loaded template.

        Args:
            job_spec: BatchJobSpec to convert.
            job_name: Optional job name, will generate one if not provided.
            prepared_job: Optional BatchJob with file IDs to add to pod annotations.

        Returns:
            Kubernetes V1Job object.
        """
        # Generate unique job name
        if job_name is None:
            job_name = f"batch-{uuid.uuid4().hex[:8]}"

        # Create pod annotations from job spec
        pod_annotations: Dict[str, str] = {
            JobAnnotationKey.SESSION_ID.value: session_id,
            JobAnnotationKey.INPUT_FILE_ID.value: job_spec.input_file_id,
            JobAnnotationKey.ENDPOINT.value: job_spec.endpoint,
        }

        # Add batch metadata as pod annotations
        if job_spec.metadata:
            for key, value in job_spec.metadata.items():
                pod_annotations[f"{JobAnnotationKey.METADATA_PREFIX.value}{key}"] = (
                    value
                )

        # Add file IDs from prepared job if provided
        suspend = True
        if prepared_job and prepared_job.status:
            if prepared_job.status.output_file_id:
                pod_annotations[JobAnnotationKey.OUTPUT_FILE_ID.value] = (
                    prepared_job.status.output_file_id
                )
            else:
                suspend = False

            if prepared_job.status.temp_output_file_id:
                pod_annotations[JobAnnotationKey.TEMP_OUTPUT_FILE_ID.value] = (
                    prepared_job.status.temp_output_file_id
                )
            else:
                suspend = False

            if prepared_job.status.error_file_id:
                pod_annotations[JobAnnotationKey.ERROR_FILE_ID.value] = (
                    prepared_job.status.error_file_id
                )
            else:
                suspend = False

            if prepared_job.status.temp_error_file_id:
                pod_annotations[JobAnnotationKey.TEMP_ERROR_FILE_ID.value] = (
                    prepared_job.status.temp_error_file_id
                )
            else:
                suspend = False

        job_patch = {
            "metadata": {
                "name": job_name,
                # Minimal job-level annotations - most metadata moved to pod
                "annotations": {
                    "batch.job.aibrix.ai/managed-by": "aibrix",
                },
            },
            "spec": {
                "template": {
                    "metadata": {
                        "annotations": pod_annotations,
                    },
                },
                "activeDeadlineSeconds": job_spec.completion_window,
                "suspend": suspend,
                "parallelism": parallelism,
                "completions": parallelism,
            },
        }
        # Use pre-loaded template (deep copy to avoid modifying the original)
        job_template = merge_yaml_object(self.job_template, job_patch)

        # Merge storage env
        if (
            storage_patch := self.storage_patches.get(storage.get_storage_type().value)
        ) is not None:
            job_template = merge_yaml_object(job_template, storage_patch, False)
        else:
            logger.warning(
                "No storage patch found", storage_type=storage.get_storage_type()
            )  # type:ignore[call-arg]

        # Merge metastore env
        if (
            metastore_patch := self.storage_patches.get(
                metastore.get_metastore_type().value
            )
        ) is not None:
            job_template = merge_yaml_object(job_template, metastore_patch, False)
        else:
            logger.warning(
                "No metastore patch found", storage_type=metastore.get_metastore_type()
            )  # type:ignore[call-arg]

        return job_template

logger.info("kopf job handlers imported")

# Standalone kopf handlers that work with the global JobCache instance
@kopf.on.create("batch", "v1", "jobs")  # type: ignore[arg-type]
async def job_created_handler(body: Any, **kwargs: Any) -> None:
    """Handle Kubernetes Job creation events."""
    job_cache = get_global_job_cache()
    if not job_cache:
        logger.warning("No global job cache available for job creation event")
        return

    try:
        # Transform K8s Job to BatchJob
        batch_job = k8s_job_to_batch_job(body)
        job_id = batch_job.status.job_id if batch_job.status else body.metadata.uid

        logger.info(
            "Job created",
            job_id=job_id,
            name=batch_job.metadata.name,
            namespace=batch_job.metadata.namespace,
            state=batch_job.status.state.value,
        )  # type: ignore[call-arg]

        # Invoke callback if registered
        try:
            if await job_cache.job_committed(batch_job):
                # Store in cache
                job_cache.active_jobs[job_id] = batch_job
            else:
                await job_cache.delete_job(batch_job)
        except Exception as e:
            logger.error(
                "Error in job committed handler",
                error=str(e),
                handler="job_committed",
            )  # type: ignore[call-arg]
    except ValueError as ve:
        # For jobs without proper annotations, store basic info for backward compatibility
        job_id = body.metadata.uid
        logger.warning(
            "Failed to process job creation",
            job_id=job_id,
            reason=str(ve),
        )  # type: ignore[call-arg]
    except Exception as e:
        logger.error(
            "Failed to process job creation", error=str(e), operation="job_created"
        )  # type: ignore[call-arg]


@kopf.on.update("batch", "v1", "jobs")  # type: ignore[arg-type]
async def job_updated_handler(body: Any, **kwargs: Any) -> None:
    """Handle Kubernetes Job update events."""
    job_cache = get_global_job_cache()
    if not job_cache:
        logger.warning("No global job cache available for job update event")
        return

    try:
        # Transform new K8s Job to BatchJob
        new_batch_job = k8s_job_to_batch_job(body)
        job_id = (
            new_batch_job.status.job_id if new_batch_job.status else body.metadata.uid
        )

        # Get old job from cache
        old_batch_job = job_cache.active_jobs.get(job_id)
        if old_batch_job is None:
            logger.warning("Job updating ignored due to job not found", job_id=job_id)  # type: ignore[call-arg]
            return

        logger.info(
            "Job updated",
            job_id=job_id,
            name=new_batch_job.metadata.name,
            namespace=new_batch_job.metadata.namespace,
            old_state=old_batch_job.status.state.value if old_batch_job else "unknown",
            new_state=new_batch_job.status.state.value,
        )  # type: ignore[call-arg]

        # Invoke callback if registered and we have both old and new jobs
        try:
            if await job_cache.job_updated(old_batch_job, new_batch_job):
                # Update cache
                job_cache.active_jobs[job_id] = new_batch_job
        except Exception as uhe:
            logger.error(
                "Error in job updated handler",
                error=str(uhe),
                handler="job_updated",
            )  # type: ignore[call-arg]
    except Exception as e:
        logger.error(
            "Failed to process job update", error=str(e), operation="job_updated"
        )  # type: ignore[call-arg]


@kopf.on.field("batch", "v1", "jobs", field="status.conditions")  # type: ignore[arg-type]
async def job_completion_handler(body: Any, **kwargs: Any) -> None:
    """
    This handler triggers ONLY when the 'status.conditions' field of a Job changes.
    """
    if not body:  # The conditions field might be None initially
        return

    await job_updated_handler(body, **kwargs)  # type: ignore[call-arg, misc, arg-type]


# Set optional = True to prevent kopf add the finalizer.
@kopf.on.delete("batch", "v1", "jobs", optional=True)  # type: ignore[arg-type]
async def job_deleted_handler(body: Any, **kwargs: Any) -> None:
    """Handle Kubernetes Job deletion events."""
    job_cache = get_global_job_cache()
    if not job_cache:
        logger.warning("No global job cache available for job deletion event")
        return

    job_id = body.metadata.uid
    job_name = body.metadata.name
    namespace = body.metadata.namespace

    # Get job from cache before deletion
    deleted_job = job_cache.active_jobs.get(job_id)
    if deleted_job is None:
        logger.info(
            "Job deleted event ignore, no job found",
            job_id=job_id,
            name=job_name,
            namespace=namespace,
        )  # type: ignore[call-arg]
        return

    logger.info(
        "Job deleted",
        job_id=job_id,
        job=deleted_job.metadata.name,
        namespace=deleted_job.metadata.namespace,
        state=deleted_job.status.state.value,
    )  # type: ignore[call-arg]

    # Invoke callback if registered
    try:
        if await job_cache.job_deleted(deleted_job):
            del job_cache.active_jobs[job_id]
    except Exception as e:
        logger.error(
            "Error in job deleted handler",
            error=str(e),
            handler="job_deleted",
        )  # type: ignore[call-arg]
