import os
from datetime import timedelta
from typing import Optional

import pytest

os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import (
    BatchJobSpec,
    BatchJobState,
    JobEntityManager,
)
from aibrix.metadata.cache.metastore import MetastoreJobCache
from aibrix.storage import StorageType


class FakeMetastore:
    def __init__(self) -> None:
        self.objects: dict[str, bytes] = {}

    async def put_object(self, key: str, data, **kwargs) -> bool:
        if isinstance(data, bytes):
            payload = data
        else:
            payload = str(data).encode("utf-8")
        self.objects[key] = payload
        return True

    async def get_object(self, key: str) -> bytes:
        try:
            return self.objects[key]
        except KeyError as exc:
            raise FileNotFoundError(key) from exc

    async def delete_object(self, key: str) -> None:
        self.objects.pop(key, None)

    async def list_objects(
        self,
        prefix: str = "",
        delimiter: Optional[str] = None,
        limit: Optional[int] = None,
        continuation_token: Optional[str] = None,
    ) -> tuple[list[str], Optional[str]]:
        del delimiter
        offset = int(continuation_token or "0")
        keys = sorted(key for key in self.objects if key.startswith(prefix))
        remaining_keys = keys[offset:]
        page = remaining_keys[:limit] if limit is not None else remaining_keys
        next_token = (
            str(offset + len(page))
            if limit is not None and len(remaining_keys) > len(page)
            else None
        )
        return page, next_token


@pytest.fixture
def fake_metastore(monkeypatch):
    store = FakeMetastore()
    calls = []

    def fake_initialize_batch_metastore(storage_type, params=None):
        calls.append((storage_type, dict(params or {})))
        module.batch_metastore.p_metastore = store

    from aibrix.metadata.cache import metastore as module

    monkeypatch.setattr(
        module, "initialize_batch_metastore", fake_initialize_batch_metastore
    )
    monkeypatch.setattr(module.batch_metastore, "p_metastore", None)
    return store, calls


@pytest.mark.asyncio
async def test_storage_job_cache_implements_job_entity_manager(fake_metastore):
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)
    assert isinstance(cache, JobEntityManager)


@pytest.mark.asyncio
async def test_storage_job_cache_initializes_batch_metastore(fake_metastore):
    _, calls = fake_metastore
    MetastoreJobCache(storage_type=StorageType.REDIS, params={"db": 3})

    assert calls == [(StorageType.REDIS, {"db": 3})]


@pytest.mark.asyncio
async def test_storage_job_cache_submit_update_list_and_delete(fake_metastore):
    _, _ = fake_metastore
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)
    committed_jobs = []
    updated_jobs = []
    deleted_jobs = []

    async def committed_handler(job):
        committed_jobs.append(job)
        return True

    async def updated_handler(old_job, new_job):
        updated_jobs.append((old_job, new_job))
        return True

    async def deleted_handler(job):
        deleted_jobs.append(job)
        return True

    cache.on_job_committed(committed_handler)
    cache.on_job_updated(updated_handler)
    cache.on_job_deleted(deleted_handler)

    older_spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    newer_spec = BatchJobSpec.from_strings(
        input_file_id="input-2",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )

    await cache.submit_job("session-1", older_spec)
    await cache.submit_job("session-2", newer_spec)

    first_job = committed_jobs[0]
    second_job = committed_jobs[1]
    first_job.status.created_at = first_job.status.created_at - timedelta(seconds=1)
    await cache.update_job_status(first_job)

    listed_jobs = await cache.list_jobs()

    assert len(committed_jobs) == 2
    assert [job.session_id for job in listed_jobs] == ["session-2", "session-1"]
    assert (await cache.get_job(first_job.job_id)).session_id == "session-1"

    ready_job = (await cache.get_job(first_job.job_id)).model_copy(deep=True)
    ready_job.status.temp_output_file_id = "temp-output"
    await cache.update_job_ready(ready_job)

    finalized_job = (await cache.get_job(first_job.job_id)).model_copy(deep=True)
    finalized_job.status.state = BatchJobState.FINALIZED
    await cache.update_job_status(finalized_job)

    persisted_job = await cache.get_job(first_job.job_id)
    assert persisted_job.status.temp_output_file_id == "temp-output"
    assert persisted_job.status.state == BatchJobState.FINALIZED
    assert persisted_job.metadata.resource_version == "4"
    assert updated_jobs[-1][0].metadata.resource_version == "3"
    assert updated_jobs[-1][1].metadata.resource_version == "4"

    await cache.delete_job(second_job)

    assert await cache.get_job(second_job.job_id) is None
    assert deleted_jobs[0].job_id == second_job.job_id
