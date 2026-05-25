from datetime import datetime
from typing import Any, Dict, List, Optional

from aibrix.batch.job_entity import BatchJob, BatchJobSpec, JobEntityManager
from aibrix.batch.storage import batch_metastore
from aibrix.batch.storage.batch_metastore import (
    delete_batch_job,
    get_batch_job,
    initialize_batch_metastore,
    list_batch_jobs,
    put_batch_job,
)
from aibrix.metadata.setting import settings


class MetastoreJobCache(JobEntityManager):
    def __init__(
        self,
        storage_type=None,
        params: Optional[dict[str, Any]] = None,
    ) -> None:
        super().__init__()
        self.active_jobs: Dict[str, BatchJob] = {}
        self._storage_type = storage_type or settings.METASTORE_TYPE
        self._params = dict(params or {})
        if batch_metastore.p_metastore is None:
            initialize_batch_metastore(self._storage_type, self._params)

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        if job_id in self.active_jobs:
            return self.active_jobs[job_id]
        job = await get_batch_job(job_id)
        if job is not None and job.job_id is not None:
            self.active_jobs[job.job_id] = job
        return job

    async def list_jobs(self) -> List[BatchJob]:
        jobs = await list_batch_jobs()
        jobs.sort(key=self._created_at_sort_key, reverse=True)
        self.active_jobs = {job.job_id: job for job in jobs if job.job_id is not None}
        return jobs

    async def submit_job(self, session_id: str, job_spec: BatchJobSpec):
        job = BatchJob.new_local(spec=job_spec)
        job.session_id = session_id
        stored_job = await self._upsert_job(job, None)
        await self.job_committed(stored_job)

    async def update_job_ready(self, job: BatchJob):
        await self._update_existing_job(job)

    async def update_job_status(self, job: BatchJob):
        await self._update_existing_job(job)

    async def cancel_job(self, job: BatchJob):
        await self._update_existing_job(job)

    async def delete_job(self, job: BatchJob):
        if job.job_id is None:
            raise ValueError("job_id is required")
        existing_job = await self.get_job(job.job_id) or job
        await delete_batch_job(job.job_id)
        self.active_jobs.pop(job.job_id, None)
        await self.job_deleted(existing_job)

    async def _update_existing_job(self, job: BatchJob) -> None:
        if job.job_id is None:
            raise ValueError("job_id is required")
        old_job = await self.get_job(job.job_id)
        stored_job = await self._upsert_job(job, old_job)
        if old_job is not None:
            await self.job_updated(old_job, stored_job)

    async def _upsert_job(self, job: BatchJob, old_job: Optional[BatchJob]) -> BatchJob:
        if job.job_id is None:
            raise ValueError("job_id is required")
        stored_job = job.model_copy(deep=True)
        stored_job_id = stored_job.job_id
        stored_job.metadata.resource_version = self._next_resource_version(old_job)
        await put_batch_job(stored_job_id, stored_job)
        self.active_jobs[stored_job_id] = stored_job
        return stored_job

    def _next_resource_version(self, old_job: Optional[BatchJob]) -> str:
        if old_job is None or old_job.metadata.resource_version is None:
            return "1"
        try:
            return str(int(old_job.metadata.resource_version) + 1)
        except ValueError:
            return "1"

    def _created_at_sort_key(self, job: BatchJob) -> datetime:
        created_at = job.status.created_at
        if created_at is not None:
            return created_at
        payload = job.model_dump(mode="json", by_alias=True)
        return datetime.fromisoformat(payload["status"]["createdAt"])
