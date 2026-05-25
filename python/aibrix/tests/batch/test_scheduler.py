import asyncio
import contextlib
import time
from datetime import datetime
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from aibrix.batch.job_entity import (
    BatchJobErrorCode,
    BatchJobState,
    BatchJobStatus,
)
from aibrix.batch.scheduler import BasicCongestionControl, JobScheduler
from aibrix.context import InfrastructureContext


class FakeProgressManager:
    def __init__(self, job_id_status=None):
        self.job_id_status = job_id_status or {}
        self.validated_job_ids = []
        self.jobs = {}
        self.failed_error = None

    async def get_job_status(self, job_id):
        if job_id not in self.job_id_status:
            return None
        return BatchJobStatus(
            jobID=job_id, state=self.job_id_status[job_id], createdAt=datetime.now()
        )

    async def validate_job(self, job_id, inference_client=None):
        self.validated_job_ids.append(job_id)
        if job_id not in self.job_id_status:
            raise ValueError(f"job_id {job_id} not found in job_id_status")
        self.job_id_status[job_id] = BatchJobState.IN_PROGRESS
        return True

    async def get_job(self, job_id):
        return self.jobs.get(job_id)

    async def mark_job_failed(self, job_id, error):
        self.failed_error = error
        job = self.jobs[job_id]
        job.status.state = BatchJobState.FINALIZED
        return job


def _make_scheduler(pool_size, progress_manager):
    scheduler = JobScheduler(
        InfrastructureContext(),
        progress_manager,
        None,
        pool_size,
        cc_controller=BasicCongestionControl(pool_size),
    )
    scheduler._inference_client = None
    scheduler.interval = 0
    return scheduler


@pytest.mark.asyncio
async def test_round_robin_get_job_prioritizes_existing_pool_jobs():
    progress_manager = FakeProgressManager(
        {
            "finished-job": BatchJobState.FINALIZED,
            "in-progress-job": BatchJobState.IN_PROGRESS,
            "new-job1": BatchJobState.CREATED,
            "new-job2": BatchJobState.CREATED,
            "new-job3": BatchJobState.CREATED,
        },
    )
    scheduler = _make_scheduler(3, progress_manager)
    scheduler._CC_controller._running_job_pool = [
        "finished-job",
        "in-progress-job",
        None,
    ]
    scheduler._CC_controller._running_job_idx = 0
    scheduler.append_job("new-job1", time.time() + 60)
    scheduler.append_job("new-job2", time.time() + 60)
    scheduler.append_job("new-job3", time.time() + 60)

    next_job_id = await scheduler.round_robin_get_job()

    assert next_job_id == "new-job1"
    assert scheduler._CC_controller._running_job_pool == [
        "new-job1",
        "in-progress-job",
        "new-job2",
    ]
    await progress_manager.validate_job("new-job1")
    progress_manager.job_id_status["in-progress-job"] = BatchJobState.FINALIZED

    next_job_id = await scheduler.round_robin_get_job()
    assert next_job_id == "new-job2"
    assert scheduler._CC_controller._running_job_pool == [
        "new-job1",
        "new-job3",
        "new-job2",
    ]


@pytest.mark.asyncio
async def test_round_robin_get_job_fills_only_empty_slots():
    progress_manager = FakeProgressManager(
        {
            "running-job-a": BatchJobState.FINALIZED,
            "running-job-b": BatchJobState.IN_PROGRESS,
            "new-job-1": BatchJobState.CREATED,
            "new-job-2": BatchJobState.CREATED,
        }
    )
    scheduler = _make_scheduler(3, progress_manager)
    scheduler._CC_controller._running_job_pool = [
        "running-job-a",
        "running-job-b",
        None,
    ]
    scheduler.append_job("new-job-1", time.time() + 60)
    scheduler.append_job("new-job-2", time.time() + 60)

    next_job_id = await scheduler.round_robin_get_job()

    assert next_job_id == "new-job-1"
    assert scheduler._CC_controller._running_job_pool == [
        "new-job-1",
        "running-job-b",
        "new-job-2",
    ]


@pytest.mark.asyncio
async def test_scheduler_marks_runtime_error_as_failed(monkeypatch):
    progress_manager = FakeProgressManager(
        {"job-runtime-failure": BatchJobState.IN_PROGRESS}
    )
    job_driver = SimpleNamespace(
        execute_job=AsyncMock(side_effect=RuntimeError("runtime boom"))
    )
    job = SimpleNamespace(
        job_id="job-runtime-failure",
        job_driver=job_driver,
        status=SimpleNamespace(state=BatchJobState.IN_PROGRESS),
    )
    progress_manager.jobs[job.job_id] = job
    scheduler = _make_scheduler(1, progress_manager)

    async def _one_job():
        return job.job_id

    monkeypatch.setattr(scheduler, "round_robin_get_job", _one_job)

    task = asyncio.create_task(scheduler.jobs_running_loop())
    try:
        for _ in range(20):
            if progress_manager.failed_error is not None:
                break
            await asyncio.sleep(0)
        assert progress_manager.failed_error is not None
        job_driver.execute_job.assert_awaited_once_with(job.job_id)
        assert progress_manager.failed_error.code == BatchJobErrorCode.INFERENCE_FAILED
        assert progress_manager.failed_error.message == "runtime boom"
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task
