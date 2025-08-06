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

from types import SimpleNamespace
from typing import Any

import kopf
import pytest

from aibrix.batch.job_entity import (
    BatchJobEndpoint,
    BatchJobState,
    CompletionWindow,
    k8s_job_to_batch_job,
)


class MockK8sJob:
    """Mock Kubernetes Job object for testing."""

    def __init__(
        self,
        metadata=None,
        annotations=None,
        status=None,
        api_version="batch/v1",
        kind="Job",
    ):
        self.metadata = metadata or {}
        self.status = status or {}
        self.spec = {"template": {"metadata": {"annotations": annotations or {}}}}
        self.api_version = api_version
        self.kind = kind


def _get_job_created_obj():
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creationTimestamp": "2024-01-01T12:00:00Z",
        },
        "spec": {
            "template": {
                "metadata": {
                    "annotations": {
                        "batch.job.aibrix.ai/input-file-id": "file-123",
                        "batch.job.aibrix.ai/endpoint": "/v1/embeddings",
                    },
                },
            },
        },
        "status": {
            "startTime": "2025-08-05T05:26:13Z",
            "active": 1,
            "terminating": 0,
            "uncountedTerminatedPods": {},
            "ready": 0,
        },
    }


def _get_job_in_progress_obj():
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creationTimestamp": "2024-01-01T12:00:00Z",
        },
        "spec": {
            "template": {
                "metadata": {
                    "annotations": {
                        "batch.job.aibrix.ai/input-file-id": "file-123",
                        "batch.job.aibrix.ai/endpoint": "/v1/embeddings",
                        "batch.job.aibrix.ai/output-file-id": "output-123",
                        "batch.job.aibrix.ai/error-file-id": "error-123",
                        "batch.job.aibrix.ai/temp-output-file-id": "temp-output-123",
                        "batch.job.aibrix.ai/temp-error-file-id": "temp-error-123",
                    },
                },
            },
        },
        "status": {
            "conditions": None,
            "startTime": "2025-08-05T05:26:13Z",
            "active": 1,
            "terminating": 0,
            "uncountedTerminatedPods": {},
            "ready": 0,
        },
    }


def _get_job_succees_obj():
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creationTimestamp": "2024-01-01T12:00:00Z",
        },
        "spec": {
            "template": {
                "metadata": {
                    "annotations": {
                        "batch.job.aibrix.ai/input-file-id": "file-123",
                        "batch.job.aibrix.ai/endpoint": "/v1/embeddings",
                        "batch.job.aibrix.ai/output-file-id": "output-123",
                        "batch.job.aibrix.ai/error-file-id": "error-123",
                        "batch.job.aibrix.ai/temp-output-file-id": "temp-output-123",
                        "batch.job.aibrix.ai/temp-error-file-id": "temp-error-123",
                    },
                },
            },
        },
        "status": {
            "conditions": [
                {
                    "type": "SuccessCriteriaMet",
                    "status": "True",
                    "lastProbeTime": "2025-08-05T05:26:25Z",
                    "lastTransitionTime": "2025-08-05T05:26:25Z",
                    "reason": "CompletionsReached",
                    "message": "Reached expected number of succeeded pods",
                },
                {
                    "type": "Complete",
                    "status": "True",
                    "lastProbeTime": "2025-08-05T05:26:25Z",
                    "lastTransitionTime": "2025-08-05T05:26:25Z",
                    "reason": "CompletionsReached",
                    "message": "Reached expected number of succeeded pods",
                },
            ],
            "startTime": "2025-08-05T05:26:13Z",
            "completionTime": "2025-08-05T05:26:25Z",
            "succeeded": 1,
            "terminating": 0,
            "uncountedTerminatedPods": {},
            "ready": 0,
        },
    }


def _get_job_expired_obj():
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creationTimestamp": "2024-01-01T12:00:00Z",
        },
        "spec": {
            "template": {
                "metadata": {
                    "annotations": {
                        "batch.job.aibrix.ai/input-file-id": "file-123",
                        "batch.job.aibrix.ai/endpoint": "/v1/embeddings",
                        "batch.job.aibrix.ai/output-file-id": "output-123",
                        "batch.job.aibrix.ai/error-file-id": "error-123",
                        "batch.job.aibrix.ai/temp-output-file-id": "temp-output-123",
                        "batch.job.aibrix.ai/temp-error-file-id": "temp-error-123",
                    },
                },
            },
        },
        "status": {
            "conditions": [
                {
                    "type": "Failed",
                    "status": "True",
                    "lastProbeTime": "2025-08-05T05:26:25Z",
                    "lastTransitionTime": "2025-08-05T05:26:25Z",
                    "reason": "DeadlineExceeded",
                    "message": "Job was active longer than specified deadline",
                },
            ],
            "startTime": "2025-08-05T05:26:13Z",
            "completionTime": "2025-08-05T05:26:25Z",
            "succeeded": 1,
            "terminating": 1,
            "uncountedTerminatedPods": {},
            "ready": 0,
        },
    }


def dict_to_obj(d: dict) -> Any:
    """Recursively converts a dictionary to a multi-level object."""
    # Convert nested dictionaries recursively
    for key, value in d.items():
        if isinstance(value, dict) and key != "annotations":
            d[key] = dict_to_obj(value)

    # Convert the top-level dictionary to a SimpleNamespace object
    return SimpleNamespace(**d)


def test_k8s_job_to_batch_job_created():
    """Test successful transformation of Kubernetes job to BatchJob."""
    # Create mock Kubernetes job with required annotations
    k8s_job = MockK8sJob(
        metadata={
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creation_timestamp": "2024-01-01T12:00:00Z",
        },
        annotations={
            "batch.job.aibrix.ai/input-file-id": "file-123",
            "batch.job.aibrix.ai/endpoint": "/v1/chat/completions",
            "batch.job.aibrix.ai/metadata.priority": "high",
            "batch.job.aibrix.ai/metadata.customer": "test-customer",
        },
        status={"active": 1},
    )

    # Transform to BatchJob
    k8s_job.spec["suspend"] = True  # Test suspend will not affect state
    k8s_job.spec["activeDeadlineSeconds"] = (
        CompletionWindow.TWENTY_FOUR_HOURS.expires_at()
    )
    batch_job = k8s_job_to_batch_job(k8s_job)

    # Verify transformation
    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"

    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.metadata.namespace == "default"
    assert batch_job.metadata.uid == "test-uid-123"

    assert batch_job.spec.input_file_id == "file-123"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert (
        batch_job.spec.completion_window
        == CompletionWindow.TWENTY_FOUR_HOURS.expires_at()
    )
    assert batch_job.spec.metadata == {"priority": "high", "customer": "test-customer"}

    assert batch_job.status.job_id == "test-uid-123"
    assert batch_job.status.state == BatchJobState.CREATED


def test_k8s_job_to_batch_job_missing_required_annotation():
    """Test transformation fails when required annotation is missing."""
    k8s_job = MockK8sJob(
        metadata={
            "name": "test-batch-job",
        },
        annotations={
            # Missing required input-file-id annotation
            "batch.job.aibrix.ai/endpoint": "/v1/chat/completions"
        },
    )

    with pytest.raises(
        ValueError, match="Required annotation.*input-file-id.*not found"
    ):
        k8s_job_to_batch_job(k8s_job)


def test_k8s_job_to_batch_job_invalid_endpoint():
    """Test transformation fails with invalid endpoint."""
    k8s_job = MockK8sJob(
        annotations={
            "batch.job.aibrix.ai/input-file-id": "file-123",
            "batch.job.aibrix.ai/endpoint": "/invalid/endpoint",
        }
    )

    # We don't check validity of k8s job obj
    batch_job = k8s_job_to_batch_job(k8s_job)
    assert batch_job.spec.endpoint == "/invalid/endpoint"


def test_k8s_job_dict_access():
    """Test transformer works with dict access."""
    batch_job = k8s_job_to_batch_job(_get_job_created_obj())

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.CREATED

    batch_job = k8s_job_to_batch_job(_get_job_in_progress_obj())

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.IN_PROGRESS

    batch_job = k8s_job_to_batch_job(_get_job_succees_obj())

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.FINALIZING

    batch_job = k8s_job_to_batch_job(_get_job_expired_obj())
    assert batch_job.status.state == BatchJobState.EXPIRED


def test_k8s_job_obj_access():
    """Test transformer works with object-style access."""
    obj = dict_to_obj(_get_job_created_obj())
    batch_job = k8s_job_to_batch_job(obj)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.CREATED

    obj = dict_to_obj(_get_job_in_progress_obj())
    batch_job = k8s_job_to_batch_job(obj)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.IN_PROGRESS

    obj = dict_to_obj(_get_job_succees_obj())
    batch_job = k8s_job_to_batch_job(obj)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.FINALIZING

    obj = dict_to_obj(_get_job_expired_obj())
    batch_job = k8s_job_to_batch_job(obj)
    assert batch_job.status.state == BatchJobState.EXPIRED


def test_k8s_job_kopf_access():
    """Test transformer works with dict-style access (e.g., from kopf)."""

    kopf_body = kopf.Body(_get_job_created_obj())
    batch_job = k8s_job_to_batch_job(kopf_body)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.CREATED

    kopf_body = kopf.Body(_get_job_in_progress_obj())
    batch_job = k8s_job_to_batch_job(kopf_body)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.IN_PROGRESS

    kopf_body = kopf.Body(_get_job_succees_obj())
    batch_job = k8s_job_to_batch_job(kopf_body)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.FINALIZING

    kopf_body = kopf.Body(_get_job_expired_obj())
    batch_job = k8s_job_to_batch_job(kopf_body)
    assert batch_job.status.state == BatchJobState.EXPIRED


def test_k8s_job_s3_integration_case():
    """Test transformer with real S3 integration job object structure."""
    k8s_job = {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "s3-batch-job",
            "namespace": "default",
            "uid": "b08af167-8d56-41e9-92b6-efebe4a859ab",
            "creationTimestamp": "2025-07-28T21:13:41Z",
            "resourceVersion": "767483",
            "generation": 1,
            "labels": {
                "app": "aibrix-batch",
                "component": "batch-processor",
            },
        },
        "spec": {
            "activeDeadlineSeconds": 3600,
            "backoffLimit": 3,
            "completions": 1,
            "parallelism": 1,
            "selector": {
                "matchLabels": {
                    "batch.kubernetes.io/controller-uid": "b08af167-8d56-41e9-92b6-efebe4a859ab"
                }
            },
            "template": {
                "metadata": {
                    "labels": {
                        "app": "aibrix-batch",
                        "component": "batch-processor",
                        "batch.kubernetes.io/controller-uid": "b08af167-8d56-41e9-92b6-efebe4a859ab",
                        "batch.kubernetes.io/job-name": "s3-batch-job",
                        "controller-uid": "b08af167-8d56-41e9-92b6-efebe4a859ab",
                        "job-name": "s3-batch-job",
                    },
                    "annotations": {
                        "batch.job.aibrix.ai/endpoint": "/v1/chat/completions",
                        "batch.job.aibrix.ai/input-file-id": "s3-test-input-db5ada19.jsonl",
                    },
                },
                "spec": {
                    "restartPolicy": "OnFailure",
                    "serviceAccountName": "job-reader-sa",
                    "automountServiceAccountToken": True,
                    "containers": [
                        {
                            "name": "batch-worker",
                            "image": "aibrix/runtime:nightly",
                            "command": ["aibrix_batch_worker"],
                            "env": [
                                {
                                    "name": "JOB_NAME",
                                    "valueFrom": {
                                        "fieldRef": {
                                            "apiVersion": "v1",
                                            "fieldPath": "metadata.labels['job-name']",
                                        }
                                    },
                                },
                                {
                                    "name": "JOB_NAMESPACE",
                                    "valueFrom": {
                                        "fieldRef": {
                                            "apiVersion": "v1",
                                            "fieldPath": "metadata.namespace",
                                        }
                                    },
                                },
                                {
                                    "name": "STORAGE_AWS_REGION",
                                    "value": "us-west-1",
                                },
                                {
                                    "name": "STORAGE_AWS_BUCKET",
                                    "value": "tianium.aibrix",
                                },
                                {
                                    "name": "REDIS_HOST",
                                    "value": "aibrix-redis-master.aibrix-system.svc.cluster.local",
                                },
                            ],
                        },
                        {
                            "name": "llm-engine",
                            "image": "aibrix/vllm-mock:nightly",
                            "ports": [{"containerPort": 8000, "protocol": "TCP"}],
                            "readinessProbe": {
                                "httpGet": {
                                    "path": "/ready",
                                    "port": 8000,
                                    "scheme": "HTTP",
                                },
                                "periodSeconds": 5,
                                "timeoutSeconds": 1,
                                "successThreshold": 1,
                                "failureThreshold": 3,
                            },
                            "env": [
                                {
                                    "name": "MY_POD_IP",
                                    "valueFrom": {
                                        "fieldRef": {
                                            "apiVersion": "v1",
                                            "fieldPath": "status.podIP",
                                        }
                                    },
                                }
                            ],
                        },
                    ],
                },
            },
        },
        "status": {
            "active": 1,
            "ready": 0,
            "startTime": "2025-07-28T21:13:41Z",
            "terminating": 0,
        },
    }

    batch_job = k8s_job_to_batch_job(k8s_job)

    # Verify transformation results
    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"

    assert batch_job.metadata.name == "s3-batch-job"
    assert batch_job.metadata.namespace == "default"
    assert batch_job.metadata.uid == "b08af167-8d56-41e9-92b6-efebe4a859ab"
    assert batch_job.metadata.resource_version == "767483"
    assert batch_job.metadata.generation == 1

    assert batch_job.spec.input_file_id == "s3-test-input-db5ada19.jsonl"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.spec.completion_window == 3600

    assert batch_job.status.job_id == "b08af167-8d56-41e9-92b6-efebe4a859ab"
    assert batch_job.status.state == BatchJobState.CREATED
