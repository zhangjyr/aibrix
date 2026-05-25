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
import json
from typing import Any

import pytest
from fastapi.testclient import TestClient
from kubernetes import client as k8s_client

import aibrix.batch.constant as batch_constant
from aibrix import envs
from aibrix.batch.job_driver import EchoInferenceEngineClient
from aibrix.metadata.cache.redis import RedisJobCache
from aibrix.storage import StorageType
from tests.batch.conftest import (
    MockMetadataStore,
    build_batch_request,
    create_test_app,
    e2e_batch_request_kwargs,
    select_e2e_backends,
    upload_batch_input_file,
)


def verify_batch_output_content(output_content: str, expected_requests: int) -> bool:
    lines = output_content.strip().split("\n")

    if len(lines) != expected_requests:
        print(f"Expected {expected_requests} output lines, got {len(lines)}")
        return False

    for i, line in enumerate(lines):
        try:
            output = json.loads(line)

            for field in ["id", "custom_id", "response"]:
                if field not in output:
                    print(f"Missing required field '{field}' in response {i + 1}")
                    return False

            expected_custom_id = f"request-{i + 1}"
            if output["custom_id"] != expected_custom_id:
                print(
                    f"Expected custom_id '{expected_custom_id}', got '{output['custom_id']}'"
                )
                return False

            response = output["response"]
            for field in ["status_code", "request_id", "body"]:
                if field not in response:
                    print(
                        f"Missing required field 'response.{field}' in response {i + 1}"
                    )
                    return False

            body = response["body"]
            if "model" not in body:
                print(
                    f"Missing required field 'response.body.model' in response {i + 1}"
                )
                return False

        except json.JSONDecodeError as e:
            print(f"Invalid JSON in output line {i + 1}: {e}")
            return False

    return True


def pytest_generate_tests(metafunc):
    select_e2e_backends(
        metafunc,
        ["redis_job_using_deployment", "k8s_job", "redis_job"],
    )


def backend_batch_request(
    test_backend: str,
    input_file_id: str,
    endpoint: str = "/v1/chat/completions",
) -> dict[str, Any]:
    return build_batch_request(
        input_file_id,
        endpoint,
        **e2e_batch_request_kwargs(test_backend),
    )


def backend_request_count(test_backend: str) -> int:
    return 10 if test_backend == "k8s_job" else 3


def backend_max_polls(test_backend: str) -> int:
    return 120 if test_backend in {"k8s_job", "redis_job_using_deployment"} else 20


async def wait_for_completed_batch(
    client: TestClient,
    batch_id: str,
    *,
    max_polls: int,
    poll_interval: float = 1.0,
    inspect_status=None,
) -> dict[str, Any]:
    for _ in range(max_polls):
        status_response = client.get(f"/v1/batches/{batch_id}")
        assert status_response.status_code == 200, status_response.text
        status_result = status_response.json()
        if inspect_status is not None:
            inspect_status(status_result)
        if status_result["status"] == "completed":
            return status_result
        if status_result["status"] in {"failed", "cancelled", "expired"}:
            raise AssertionError(
                f"Batch job {status_result['status']}: {status_result}"
            )
        await asyncio.sleep(poll_interval)
    raise AssertionError(f"Batch job {batch_id} did not complete within polling budget")


def assert_completed_batch(
    status_result: dict[str, Any], expected_requests: int
) -> str:
    assert status_result["status"] == "completed"
    output_file_id = status_result["output_file_id"]
    assert output_file_id is not None
    request_counts = status_result.get("request_counts")
    assert request_counts is not None
    assert request_counts["total"] == expected_requests
    assert request_counts["completed"] == expected_requests
    assert request_counts["failed"] == 0
    return output_file_id


async def download_and_verify_output(
    client: TestClient, output_file_id: str, expected_requests: int
) -> str:
    output_response = client.get(f"/v1/files/{output_file_id}/content")
    assert output_response.status_code == 200, output_response.text
    output_content = output_response.content.decode("utf-8")
    assert output_content
    assert verify_batch_output_content(output_content, expected_requests)
    return output_content


class SlowEchoInferenceEngineClient(EchoInferenceEngineClient):
    async def inference_request(self, endpoint: str, request_data):
        await asyncio.sleep(0.5)
        return request_data


@pytest.mark.asyncio
async def test_openai_batch_api_success_workflow(e2e_test_app, test_backend):
    app = e2e_test_app
    expected_requests = backend_request_count(test_backend)
    endpoint = "/v1/chat/completions"

    infrastructure_context = getattr(app.state.batch_driver, "_context", None)
    apps_v1_api = None
    core_v1_api = None
    if test_backend == "redis_job_using_deployment":
        assert isinstance(app.state.batch_driver._job_entity_manager, RedisJobCache)
        assert infrastructure_context is not None
        apps_v1_api = infrastructure_context.apps_v1_api
        core_v1_api = infrastructure_context.core_v1_api
        assert apps_v1_api is not None
        assert core_v1_api is not None

    with TestClient(app) as client:
        input_file_id = upload_batch_input_file(
            client,
            num_requests=expected_requests,
            endpoint=endpoint,
            filename=f"{test_backend}-input.jsonl",
        )
        create_response = client.post(
            "/v1/batches",
            json=backend_batch_request(test_backend, input_file_id, endpoint),
        )
        assert create_response.status_code == 200, create_response.text
        batch_result = create_response.json()
        assert batch_result["input_file_id"] == input_file_id
        assert batch_result["endpoint"] == endpoint
        batch_id = batch_result["id"]
        deployment_name = f"batch-{batch_id[:12]}-engine"
        service_name = f"batch-{batch_id[:12]}-svc"
        saw_deployment = False
        saw_ready_deployment = False
        saw_service = False

        def inspect_status(_status_result: dict[str, Any]) -> None:
            nonlocal saw_deployment, saw_ready_deployment, saw_service
            if test_backend != "redis_job_using_deployment":
                return
            try:
                deployment = apps_v1_api.read_namespaced_deployment_status(
                    name=deployment_name,
                    namespace="default",
                )
                saw_deployment = True
                if (deployment.status.available_replicas or 0) >= 1:
                    saw_ready_deployment = True
            except k8s_client.ApiException as ex:
                if ex.status != 404:
                    raise
            try:
                service = core_v1_api.read_namespaced_service(
                    name=service_name,
                    namespace="default",
                )
                if service.metadata.name == service_name:
                    saw_service = True
            except k8s_client.ApiException as ex:
                if ex.status != 404:
                    raise

        try:
            completed_batch = await wait_for_completed_batch(
                client,
                batch_id,
                max_polls=backend_max_polls(test_backend),
                inspect_status=inspect_status,
            )
            output_file_id = assert_completed_batch(completed_batch, expected_requests)
            output_content = await download_and_verify_output(
                client, output_file_id, expected_requests
            )

            if test_backend in {"k8s_job", "redis_job_using_deployment"}:
                assert isinstance(completed_batch["in_progress_at"], int)
                assert isinstance(completed_batch["finalizing_at"], int)
                assert isinstance(completed_batch["completed_at"], int)

            if test_backend == "redis_job_using_deployment":
                assert saw_deployment
                assert saw_ready_deployment
                assert saw_service
                first_line = json.loads(output_content.splitlines()[0])
                assert first_line["response"]["body"]["model"] == service_name
        finally:
            if test_backend == "redis_job_using_deployment":
                try:
                    core_v1_api.delete_namespaced_service(
                        name=service_name,
                        namespace="default",
                    )
                except k8s_client.ApiException as ex:
                    if ex.status != 404:
                        raise
                try:
                    apps_v1_api.delete_namespaced_deployment(
                        name=deployment_name,
                        namespace="default",
                    )
                except k8s_client.ApiException as ex:
                    if ex.status != 404:
                        raise
            await app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "endpoint",
    [
        "/v1/chat/completions",
        "/v1/completions",
        "/v1/embeddings",
        "/v1/rerank",
    ],
    ids=["chat_completions", "completions", "embeddings", "rerank"],
)
async def test_openai_batch_api_multi_endpoint(
    e2e_test_app, test_backend, endpoint: str
):
    if test_backend not in {"local_metastore_job", "redis_job"}:
        pytest.skip("Multi-endpoint coverage only applies to echo-backed workflows")

    app = e2e_test_app
    expected_requests = 3

    with TestClient(app) as client:
        input_file_id = upload_batch_input_file(
            client,
            num_requests=expected_requests,
            endpoint=endpoint,
            filename=f"{test_backend}-{endpoint.split('/')[-1]}.jsonl",
        )
        create_response = client.post(
            "/v1/batches",
            json=backend_batch_request(test_backend, input_file_id, endpoint),
        )
        assert create_response.status_code == 200, create_response.text
        batch_id = create_response.json()["id"]

        try:
            completed_batch = await wait_for_completed_batch(
                client,
                batch_id,
                max_polls=backend_max_polls(test_backend),
            )
            output_file_id = assert_completed_batch(completed_batch, expected_requests)
            await download_and_verify_output(client, output_file_id, expected_requests)
        finally:
            await app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_openai_batch_api_respects_default_job_pool_size(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("AIBRIX_BATCH_JOB_POOL_SIZE", "3")
    monkeypatch.setattr(envs, "BATCH_JOB_POOL_SIZE", 3)
    monkeypatch.setattr(batch_constant, "DEFAULT_JOB_POOL_SIZE", 3)

    app = create_test_app(
        enable_metastore_job=True,
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
        dry_run=False,
        inference_client=SlowEchoInferenceEngineClient(),
    )
    app.state.metadata_store = MockMetadataStore()
    app.state.redis_client = None

    scheduler = app.state.batch_driver._scheduler
    assert scheduler is not None
    assert scheduler._current_pool_size == 3
    assert scheduler._CC_controller._job_pool_size == 3

    batch_ids: list[str] = []
    with TestClient(app) as client:
        try:
            for index in range(4):
                input_file_id = upload_batch_input_file(
                    client,
                    num_requests=2,
                    endpoint="/v1/chat/completions",
                    filename=f"pool-size-{index}.jsonl",
                )
                create_response = client.post(
                    "/v1/batches",
                    json=backend_batch_request(
                        "local_metastore_job",
                        input_file_id,
                        "/v1/chat/completions",
                    ),
                )
                assert create_response.status_code == 200, create_response.text
                batch_ids.append(create_response.json()["id"])

            observed_running_pool = 0
            for _ in range(40):
                running_pool_size = len(
                    [
                        job_id
                        for job_id in scheduler._CC_controller._running_job_pool
                        if job_id
                    ]
                )
                observed_running_pool = max(observed_running_pool, running_pool_size)
                if observed_running_pool >= 3:
                    break
                await asyncio.sleep(0.2)

            assert observed_running_pool >= 3

            for batch_id in batch_ids:
                completed_batch = await wait_for_completed_batch(
                    client,
                    batch_id,
                    max_polls=40,
                    poll_interval=0.5,
                )
                output_file_id = assert_completed_batch(completed_batch, 2)
                await download_and_verify_output(client, output_file_id, 2)
        finally:
            for batch_id in batch_ids:
                await app.state.batch_driver.clear_job(batch_id)
