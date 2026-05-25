import copy
import os

import pytest

os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import (
    BatchJobSpec,
    BatchJobState,
    BatchJobStatusCopy,
    JobEntityManager,
    RequestCountStats,
)
from aibrix.metadata.cache.redis import RedisJobCache


class FakeRedisPipeline:
    def __init__(self, redis):
        self.redis = redis
        self.commands = []

    def get(self, key):
        self.commands.append(("get", key))
        return self

    def set(self, key, value):
        self.commands.append(("set", key, value))
        return self

    def delete(self, key):
        self.commands.append(("delete", key))
        return self

    def zadd(self, key, mapping):
        self.commands.append(("zadd", key, mapping))
        return self

    def zrem(self, key, member):
        self.commands.append(("zrem", key, member))
        return self

    def sadd(self, key, value):
        self.commands.append(("sadd", key, value))
        return self

    def execute(self):
        results = []
        for command in self.commands:
            name = command[0]
            results.append(getattr(self.redis, name)(*command[1:]))
        return results


class FakeRedis:
    def __init__(self):
        self.values = {}
        self.sorted_sets = {}
        self.sets = {}

    async def get(self, key):
        value = self.values.get(key)
        if value is None:
            return None
        return copy.deepcopy(value)

    async def set(self, key, value):
        self.values[key] = value
        return True

    async def delete(self, key):
        self.values.pop(key, None)
        self.sets.pop(key, None)
        return 1

    async def zadd(self, key, mapping):
        self.sorted_sets.setdefault(key, {})
        self.sorted_sets[key].update(mapping)
        return 1

    async def zrevrange(self, key, start, end):
        items = sorted(
            self.sorted_sets.get(key, {}).items(),
            key=lambda item: item[1],
            reverse=True,
        )
        members = [member for member, _ in items]
        if end == -1:
            selected = members[start:]
        else:
            selected = members[start : end + 1]
        return [
            member.encode("utf-8") if isinstance(member, str) else member
            for member in selected
        ]

    async def zrem(self, key, member):
        if key in self.sorted_sets:
            self.sorted_sets[key].pop(member, None)
        return 1

    def sadd(self, key, value):
        self.sets.setdefault(key, set()).add(value)
        return 1

    def smembers(self, key):
        return {
            value.encode("utf-8") if isinstance(value, str) else value
            for value in self.sets.get(key, set())
        }

    def run_pipeline(self, callback):
        pipeline = FakeRedisPipeline(self)
        callback(pipeline)
        return pipeline.execute()


@pytest.mark.asyncio
async def test_redis_job_cache_implements_job_entity_manager():
    cache = RedisJobCache(redis_client=FakeRedis())
    assert isinstance(cache, JobEntityManager)


@pytest.mark.asyncio
async def test_redis_job_cache_submit_and_list_jobs():
    cache = RedisJobCache(redis_client=FakeRedis())
    committed_jobs = []

    async def committed_handler(job):
        committed_jobs.append(job)
        return True

    cache.on_job_committed(committed_handler)

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

    assert len(committed_jobs) == 2
    assert committed_jobs[0].metadata.resource_version == "1"
    assert committed_jobs[1].metadata.resource_version == "1"

    listed_jobs = await cache.list_jobs()
    assert [job.session_id for job in listed_jobs] == ["session-2", "session-1"]
    assert (await cache.get_job(committed_jobs[0].job_id)).session_id == "session-1"


@pytest.mark.asyncio
async def test_redis_job_cache_update_and_delete_callbacks():
    cache = RedisJobCache(redis_client=FakeRedis())
    updated_jobs = []
    deleted_jobs = []

    async def committed_handler(job):
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

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)

    job = (await cache.list_jobs())[0]
    ready_job = job.model_copy(deep=True)
    ready_job.status.in_progress_at = ready_job.status.created_at
    ready_job.status.temp_output_file_id = "temp-output"
    ready_job.status.temp_error_file_id = "temp-error"

    await cache.update_job_ready(ready_job)

    persisted_ready_job = await cache.get_job(job.job_id)
    assert persisted_ready_job.status.temp_output_file_id == "temp-output"
    assert persisted_ready_job.metadata.resource_version == "2"
    assert updated_jobs[-1][0].metadata.resource_version == "1"
    assert updated_jobs[-1][1].metadata.resource_version == "2"

    finalized_job = persisted_ready_job.model_copy(deep=True)
    finalized_job.status.state = BatchJobState.FINALIZED
    await cache.update_job_status(finalized_job)

    assert (await cache.get_job(job.job_id)).status.state == BatchJobState.FINALIZED
    assert (await cache.get_job(job.job_id)).metadata.resource_version == "3"

    await cache.delete_job(await cache.get_job(job.job_id))

    assert await cache.get_job(job.job_id) is None
    assert deleted_jobs[0].job_id == job.job_id


@pytest.mark.asyncio
async def test_redis_job_cache_persists_status_copies_separately():
    redis = FakeRedis()
    cache = RedisJobCache(redis_client=redis)

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)

    job = (await cache.list_jobs())[0]
    job.status.state = BatchJobState.IN_PROGRESS
    job.status.request_counts.total = 10
    job.status.request_counts.launched = 0
    job.status.request_counts.completed = 0
    job.status.status_copies = {
        "worker-1": BatchJobStatusCopy(
            state=BatchJobState.IN_PROGRESS,
            requestCounts=RequestCountStats(
                total=10, launched=2, completed=1, failed=0
            ),
            updated=True,
        ),
        "worker-2": BatchJobStatusCopy(
            state=BatchJobState.IN_PROGRESS,
            requestCounts=RequestCountStats(
                total=10, launched=3, completed=2, failed=1
            ),
            updated=True,
        ),
    }

    await cache.update_job_status(job)
    cache.active_jobs.clear()

    fetched = await cache.get_job(job.job_id)
    assert fetched.status.request_counts.total == 10
    assert fetched.status.request_counts.launched == 5
    assert fetched.status.request_counts.completed == 3
    assert fetched.status.request_counts.failed == 1
    assert set(fetched.status.status_copies) == {"worker-1", "worker-2"}
    assert f"batch_jobs:batchstatus_copies:{job.job_id}:worker-1" in redis.values
    assert f"batch_jobs:batchstatus_copies:{job.job_id}:worker-2" in redis.values

    await cache.delete_job(fetched)

    assert f"batch_jobs:batchstatus_copies:{job.job_id}:worker-1" not in redis.values
    assert f"batch_jobs:batchstatus_copies:{job.job_id}:worker-2" not in redis.values


@pytest.mark.asyncio
async def test_redis_job_cache_empty_prefix_interworks_with_batch_metastore_keys():
    redis = FakeRedis()
    cache = RedisJobCache(redis_client=redis, key_prefix="")

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)

    job = (await cache.list_jobs())[0]
    job.status.state = BatchJobState.IN_PROGRESS
    job.status.request_counts.total = 10
    job.status.request_counts.launched = 0
    job.status.request_counts.completed = 0
    job.status.status_copies = {
        "worker-1": BatchJobStatusCopy(
            state=BatchJobState.IN_PROGRESS,
            requestCounts=RequestCountStats(
                total=10, launched=2, completed=1, failed=0
            ),
            updated=True,
        )
    }

    await cache.update_job_status(job)
    cache.active_jobs.clear()

    assert f"batchjob:{job.job_id}" in redis.values
    assert f"batchstatus_copies:{job.job_id}:worker-1" in redis.values
    assert redis.sorted_sets["timestamps:all"][f"batchjob:{job.job_id}"] > 0
    assert (
        redis.sorted_sets["timestamps:all"][f"batchstatus_copies:{job.job_id}:worker-1"]
        > 0
    )

    fetched = await cache.get_job(job.job_id)
    assert fetched.status.request_counts.launched == 2
    assert fetched.status.request_counts.completed == 1
    assert set(fetched.status.status_copies) == {"worker-1"}

    listed_jobs = await cache.list_jobs()
    assert [listed.job_id for listed in listed_jobs] == [job.job_id]

    await cache.delete_job(fetched)

    assert f"batchjob:{job.job_id}" not in redis.values
    assert f"batchstatus_copies:{job.job_id}:worker-1" not in redis.values
    assert f"batchjob:{job.job_id}" not in redis.sorted_sets["timestamps:all"]
    assert (
        f"batchstatus_copies:{job.job_id}:worker-1"
        not in redis.sorted_sets["timestamps:all"]
    )
