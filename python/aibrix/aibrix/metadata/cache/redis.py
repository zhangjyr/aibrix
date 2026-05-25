import asyncio
import json
import time
from datetime import datetime
from typing import AbstractSet, Any, Awaitable, Dict, List, Optional, Protocol

import redis.asyncio as redis

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatusCopy,
    JobEntityManager,
    aggregate_batch_job_status,
    merge_batch_job_status_copies,
)


class RedisJobCachePipeline(Protocol):
    def get(self, key: str) -> Any: ...

    def set(self, key: str, value: str) -> Any: ...

    def delete(self, key: str) -> Any: ...

    def zadd(self, key: str, mapping: dict[str, float]) -> Any: ...

    def zrem(self, key: str, value: str) -> Any: ...

    def sadd(self, key: str, value: str) -> Any: ...


class RedisJobCacheClient(Protocol):
    def get(self, key: str) -> bytes | str | None | Awaitable[bytes | str | None]: ...

    def zrevrange(
        self, key: str, start: int, end: int
    ) -> list[bytes | str] | Awaitable[list[bytes | str]]: ...

    def smembers(
        self, key: str
    ) -> (
        AbstractSet[bytes | str]
        | list[bytes | str]
        | Awaitable[AbstractSet[bytes | str] | list[bytes | str]]
    ): ...

    def delete(self, key: str) -> Any | Awaitable[Any]: ...

    def zrem(self, key: str, value: str) -> Any | Awaitable[Any]: ...

    def set(self, key: str, value: str) -> Any | Awaitable[Any]: ...

    def zadd(self, key: str, mapping: dict[str, float]) -> Any | Awaitable[Any]: ...

    def run_pipeline(self, callback: Any) -> list[Any] | Awaitable[list[Any]]: ...


class RedisJobCache(JobEntityManager):
    def __init__(
        self,
        host: str = "localhost",
        port: int = 6379,
        db: int = 0,
        password: Optional[str] = None,
        key_prefix: str = "batch_jobs",
        redis_client: Optional[redis.Redis] = None,
    ) -> None:
        super().__init__()
        self.active_jobs: Dict[str, BatchJob] = {}
        self._client = redis_client or self._build_client(
            host=host,
            port=port,
            db=db,
            password=password,
        )
        self._key_prefix = key_prefix
        self._metastore_compatible_keys = key_prefix == ""
        self._index_key = f"{key_prefix}:index" if key_prefix else "timestamps:all"
        self._job_prefix = f"{key_prefix}:batchjob" if key_prefix else "batchjob"
        self._status_copy_prefix = (
            f"{key_prefix}:batchstatus_copies" if key_prefix else "batchstatus_copies"
        )

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        if job_id in self.active_jobs:
            aggregated = self.active_jobs[job_id].copy(
                aggregate_batch_job_status(self.active_jobs[job_id].status)
            )
            self.active_jobs[job_id] = aggregated
            return aggregated
        payload = await self._maybe_await(self._client.get(self._job_key(job_id)))
        if payload is None:
            return None
        job = self._deserialize_job(payload)
        job = await self._hydrate_job(job)
        if job.job_id is not None:
            self.active_jobs[job.job_id] = job
        return job

    async def list_jobs(self) -> List[BatchJob]:
        if self._metastore_compatible_keys:
            decoded_job_ids = await self._list_job_ids_from_metastore_keys()
        else:
            job_ids = await self._maybe_await(
                self._client.zrevrange(self._index_key, 0, -1)
            )
            decoded_job_ids = [self._decode(raw_job_id) for raw_job_id in job_ids]
        payloads = await self._pipeline_get(
            [self._job_key(job_id) for job_id in decoded_job_ids]
        )
        jobs: list[Optional[BatchJob]] = []
        jobs_to_hydrate: list[BatchJob] = []
        jobs_to_hydrate_indices: list[int] = []
        for payload in payloads:
            if payload is None:
                continue
            job = self._deserialize_job(payload)
            if self._should_load_status_copies(job):
                jobs_to_hydrate_indices.append(len(jobs))
                jobs.append(None)
                jobs_to_hydrate.append(job)
            else:
                jobs.append(job)
        if jobs_to_hydrate:
            hydrated_jobs = await asyncio.gather(
                *(self._hydrate_job(job) for job in jobs_to_hydrate)
            )
            for index, hydrated_job in zip(jobs_to_hydrate_indices, hydrated_jobs):
                jobs[index] = hydrated_job
        listed_jobs = [job for job in jobs if job is not None]
        if self._metastore_compatible_keys:
            listed_jobs.sort(key=self._created_at_sort_key, reverse=True)
        self.active_jobs = {
            job.job_id: job for job in listed_jobs if job.job_id is not None
        }
        return listed_jobs

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
        worker_ids = await self._list_status_copy_worker_ids(job.job_id)
        await self._run_pipeline(
            lambda pipeline: self._delete_job_pipeline(pipeline, job.job_id, worker_ids)
        )
        self.active_jobs.pop(job.job_id, None)
        await self.job_deleted(existing_job)

    def _build_client(
        self,
        host: str,
        port: int,
        db: int,
        password: Optional[str],
    ) -> redis.Redis:
        return redis.Redis(
            host=host,
            port=port,
            db=db,
            password=password,
            decode_responses=False,
        )

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
        if old_job is not None:
            stored_job.status = merge_batch_job_status_copies(
                old_job.status, stored_job.status
            )
        else:
            stored_job.status = aggregate_batch_job_status(stored_job.status)
        stored_job_id = stored_job.job_id
        if stored_job_id is None:
            raise ValueError("job_id is required")
        stored_job.metadata.resource_version = self._next_resource_version(old_job)
        status_copies = stored_job.status.status_copies or {}
        main_job = stored_job.copy(stored_job.status.model_copy(deep=True))
        main_job.status.status_copies = None
        payload = main_job.model_dump_json(by_alias=True)
        await self._run_pipeline(
            lambda pipeline: self._upsert_job_pipeline(
                pipeline, stored_job_id, payload, stored_job, status_copies
            )
        )
        self.active_jobs[stored_job_id] = stored_job
        return stored_job

    def _job_key(self, job_id: str) -> str:
        return f"{self._job_prefix}:{job_id}"

    def _status_copy_index_key(self, job_id: str) -> str:
        return f"{self._status_copy_prefix}:{job_id}:index"

    def _status_copy_key(self, job_id: str, worker_id: str) -> str:
        return f"{self._status_copy_prefix}:{job_id}:{worker_id}"

    def _deserialize_job(self, payload: Any) -> BatchJob:
        if isinstance(payload, bytes):
            payload = payload.decode("utf-8")
        job = BatchJob.model_validate(json.loads(payload))
        return job.copy(aggregate_batch_job_status(job.status))

    async def _hydrate_job(self, job: BatchJob) -> BatchJob:
        if job.job_id is None or not self._should_load_status_copies(job):
            return job
        status_copies = await self._load_status_copies(job.job_id)
        if not status_copies:
            return job
        job.status.status_copies = status_copies
        job.status = aggregate_batch_job_status(job.status, False)
        return job

    async def _load_status_copies(self, job_id: str) -> dict[str, BatchJobStatusCopy]:
        worker_ids = await self._list_status_copy_worker_ids(job_id)
        if not worker_ids:
            return {}
        payloads = await self._pipeline_get(
            [self._status_copy_key(job_id, worker_id) for worker_id in worker_ids]
        )
        status_copies: dict[str, BatchJobStatusCopy] = {}
        for worker_id, payload in zip(worker_ids, payloads):
            if payload is None:
                continue
            if isinstance(payload, bytes):
                payload = payload.decode("utf-8")
            status_copies[worker_id] = BatchJobStatusCopy.model_validate_json(payload)
        return status_copies

    async def _list_status_copy_worker_ids(self, job_id: str) -> list[str]:
        if self._metastore_compatible_keys:
            prefix = f"{self._status_copy_prefix}:{job_id}:"
            keys = await self._list_keys_with_prefix(prefix)
            return [key[len(prefix) :] for key in keys]
        worker_ids = await self._maybe_await(
            self._client.smembers(self._status_copy_index_key(job_id))
        )
        return [self._decode(worker_id) for worker_id in worker_ids]

    async def _list_job_ids_from_metastore_keys(self) -> list[str]:
        prefix = f"{self._job_prefix}:"
        job_keys = await self._list_keys_with_prefix(prefix)
        return [job_key[len(prefix) :] for job_key in job_keys]

    async def _list_keys_with_prefix(self, prefix: str) -> list[str]:
        keys = await self._maybe_await(self._client.zrevrange("timestamps:all", 0, -1))
        return [
            decoded_key
            for raw_key in keys
            if (decoded_key := self._decode(raw_key)).startswith(prefix)
        ]

    async def _pipeline_get(self, keys: list[str]) -> list[Any]:
        if not keys:
            return []
        return await self._run_pipeline(
            lambda pipeline: [pipeline.get(key) for key in keys]
        )

    async def _run_pipeline(self, callback: Any) -> list[Any]:
        return await self._maybe_await(self._client.run_pipeline(callback))

    def _upsert_job_pipeline(
        self,
        pipeline: RedisJobCachePipeline,
        job_id: str,
        payload: str,
        stored_job: BatchJob,
        status_copies: dict[str, BatchJobStatusCopy],
    ) -> None:
        pipeline.set(self._job_key(job_id), payload)
        if self._metastore_compatible_keys:
            pipeline.zadd("timestamps:all", {self._job_key(job_id): time.time()})
        else:
            pipeline.zadd(self._index_key, {job_id: self._created_at_score(stored_job)})
        for worker_id, status_copy in status_copies.items():
            if not status_copy.updated:
                continue
            pipeline.set(
                self._status_copy_key(job_id, worker_id),
                status_copy.model_dump_json(by_alias=True, exclude_none=True),
            )
            if self._metastore_compatible_keys:
                pipeline.zadd(
                    "timestamps:all",
                    {self._status_copy_key(job_id, worker_id): time.time()},
                )
            else:
                pipeline.sadd(self._status_copy_index_key(job_id), worker_id)

    def _delete_job_pipeline(
        self, pipeline: RedisJobCachePipeline, job_id: str, worker_ids: list[str]
    ) -> None:
        pipeline.delete(self._job_key(job_id))
        if self._metastore_compatible_keys:
            pipeline.zrem("timestamps:all", self._job_key(job_id))
        else:
            pipeline.zrem(self._index_key, job_id)
            pipeline.delete(self._status_copy_index_key(job_id))
        for worker_id in worker_ids:
            if self._metastore_compatible_keys:
                pipeline.zrem(
                    "timestamps:all", self._status_copy_key(job_id, worker_id)
                )
            pipeline.delete(self._status_copy_key(job_id, worker_id))

    def _should_load_status_copies(self, job: BatchJob) -> bool:
        return job.status.state not in {
            BatchJobState.CREATED,
            BatchJobState.VALIDATING,
            BatchJobState.FINALIZED,
        }

    def _decode(self, value: bytes | str) -> str:
        if isinstance(value, bytes):
            return value.decode("utf-8")
        return value

    def _created_at_sort_key(self, job: BatchJob) -> datetime:
        created_at = job.status.created_at
        if created_at is not None:
            return created_at
        payload = job.model_dump(mode="json", by_alias=True)
        return datetime.fromisoformat(payload["status"]["createdAt"])

    def _next_resource_version(self, old_job: Optional[BatchJob]) -> str:
        if old_job is None or old_job.metadata.resource_version is None:
            return "1"
        try:
            return str(int(old_job.metadata.resource_version) + 1)
        except ValueError:
            return "1"

    def _created_at_score(self, job: BatchJob) -> float:
        created_at = job.status.created_at
        if created_at is not None:
            return created_at.timestamp()
        payload = job.model_dump(mode="json", by_alias=True)
        return datetime.fromisoformat(payload["status"]["createdAt"]).timestamp()
