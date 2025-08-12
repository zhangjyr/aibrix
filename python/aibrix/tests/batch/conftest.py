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

import os
import threading
from pathlib import Path

import boto3
import kopf
import pytest
import yaml
from kubernetes import client, config

from aibrix.logger import init_logger
from aibrix.metadata.cache.job import JobCache

logger = init_logger(__name__)

# Use a threading.Event to signal when the operator is ready
OPERATOR_READY = threading.Event()


def run_operator_in_thread(stop_flag: threading.Event):
    """The target function for the operator thread."""
    # The 'ready_flag' is a special kopf argument that gets set
    # when the operator has started and is ready to handle events.
    kopf.run(
        standalone=True,
        ready_flag=OPERATOR_READY,
        namespace="default",  # Monitor default namespace for tests
        stop_flag=stop_flag,
    )


@pytest.fixture(scope="session")
def k8s_config():
    """Initialize Kubernetes client."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

@pytest.fixture
def kopf_operator(scope="function"):
    """
    A session-scoped fixture to run the kopf operator in a background thread.
    This ensures JobCache handlers are properly triggered during tests.
    """
    from aibrix.metadata.core import KopfOperatorWrapper
    operator = KopfOperatorWrapper(
        namespace="default",
        startup_timeout=30,
        shutdown_timeout=10,
    )
    try:
        # Start the kopf operator in a daemon thread
        print("--- Starting kopf operator in background thread ---")
        operator.start()
        print("--- Kopf operator is ready, yielding to tests ---")
        yield  # Tests run here

    finally:
        print("\n--- Kopf operator test session finished ---")
        operator.stop()


@pytest.fixture(scope="session")
def test_namespace():
    """Use default namespace for testing."""
    return "default"


@pytest.fixture(scope="function")
def job_cache(kopf_operator):
    """
    Function-scoped fixture that provides a JobCache instance.
    The kopf_operator fixture ensures the operator is running.
    """
    return JobCache()


@pytest.fixture(scope="session")
def s3_config_available():
    """Check if S3 configuration is available locally."""
    try:
        # Check for AWS credentials
        session = boto3.Session()
        credentials = session.get_credentials()

        if not credentials:
            pytest.skip("No AWS credentials found")

        # Check for required environment variables or default credentials
        access_key = credentials.access_key
        secret_key = credentials.secret_key

        if not access_key or not secret_key:
            pytest.skip("AWS credentials incomplete")

        # Test S3 access
        s3_client = session.client("s3")
        s3_client.list_buckets()

        return {
            "access_key": access_key,
            "secret_key": secret_key,
            "region": session.region_name or "us-west-2",
        }

    except Exception as e:
        pytest.skip(f"S3 configuration not available: {e}")


@pytest.fixture(scope="session")
def redis_config_available():
    """Check if S3 configuration is available locally."""
    # Check for AWS credentials
    if os.getenv("REDIS_HOST") is None:
        pytest.skip("Redis configuration not available")


@pytest.fixture(scope="session")
def test_s3_bucket(s3_config_available):
    """Get or create test S3 bucket."""
    bucket_name = os.getenv("AIBRIX_TEST_S3_BUCKET")

    session = boto3.Session()
    s3_client = session.client("s3")

    try:
        # Try to access the bucket
        s3_client.head_bucket(Bucket=bucket_name)
        logger.info(f"Using existing S3 bucket: {bucket_name}")
    except s3_client.exceptions.NoSuchBucket:
        pytest.skip(
            f"Test bucket {bucket_name} does not exist. Set TEST_S3_BUCKET env var or create bucket."
        )
    except Exception as e:
        pytest.skip(f"Cannot access S3 bucket {bucket_name}: {e}")

    return bucket_name


@pytest.fixture(scope="session")
def s3_credentials_secret(
    k8s_config, test_namespace, s3_config_available, test_s3_bucket
):
    """Create K8s secret with S3 credentials from YAML template."""
    import base64

    # Load secret template from YAML
    secret_template_path = Path(__file__).parent / "testdata" / "s3_secret.yaml"
    with open(secret_template_path, "r") as f:
        secret_template = yaml.safe_load(f)

    core_v1 = client.CoreV1Api()
    secret_name = secret_template["metadata"]["name"]

    # Populate secret data with actual values (K8s expects base64 encoded values)
    secret_template["data"] = {
        "access-key-id": base64.b64encode(
            s3_config_available["access_key"].encode()
        ).decode(),
        "secret-access-key": base64.b64encode(
            s3_config_available["secret_key"].encode()
        ).decode(),
        "region": base64.b64encode(s3_config_available["region"].encode()).decode(),
        "bucket-name": base64.b64encode(test_s3_bucket.encode()).decode(),
    }

    # Update namespace
    secret_template["metadata"]["namespace"] = test_namespace

    # Create K8s Secret object
    secret = client.V1Secret(
        metadata=client.V1ObjectMeta(name=secret_name, namespace=test_namespace),
        data=secret_template["data"],
        type=secret_template["type"],
    )

    try:
        # Delete existing secret if it exists
        try:
            core_v1.delete_namespaced_secret(name=secret_name, namespace=test_namespace)
        except client.ApiException as e:
            if e.status != 404:
                raise

        # Create the secret
        core_v1.create_namespaced_secret(namespace=test_namespace, body=secret)
        logger.info(f"Created K8s secret: {secret_name}")

        yield secret_name

    finally:
        # Cleanup: delete the secret
        try:
            core_v1.delete_namespaced_secret(name=secret_name, namespace=test_namespace)
            logger.info(f"Deleted K8s secret: {secret_name}")
        except client.ApiException as e:
            if e.status != 404:
                logger.warning(f"Failed to cleanup secret {secret_name}: {e}")
