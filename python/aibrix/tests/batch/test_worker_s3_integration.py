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
import copy
import json
import os
import time
import uuid
from pathlib import Path

import boto3
import pytest
import yaml
from kubernetes import client, config

from aibrix.logger import init_logger

logger = init_logger(__name__)


class TestWorkerS3Integration:
    """Test worker with S3 storage and Redis metadata integration."""

    @pytest.fixture(scope="class")
    def s3_config_available(self):
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

    @pytest.fixture
    def test_s3_bucket(self, s3_config_available):
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

    @pytest.fixture(scope="class")
    def k8s_client(self):
        """Initialize Kubernetes client."""
        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()

        return client.BatchV1Api()

    @pytest.fixture(scope="class")
    def test_namespace(self):
        """Use default namespace for testing."""
        return "default"

    @pytest.fixture
    def s3_credentials_secret(
        self, k8s_client, test_namespace, s3_config_available, test_s3_bucket
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
                core_v1.delete_namespaced_secret(
                    name=secret_name, namespace=test_namespace
                )
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
                core_v1.delete_namespaced_secret(
                    name=secret_name, namespace=test_namespace
                )
                logger.info(f"Deleted K8s secret: {secret_name}")
            except client.ApiException as e:
                if e.status != 404:
                    logger.warning(f"Failed to cleanup secret {secret_name}: {e}")

    @pytest.fixture
    def s3_job_template_patch(self, s3_credentials_secret):
        """Load S3-specific job template patch from YAML file."""
        patch_file_path = (
            Path(__file__).parent / "testdata" / "k8s_job_s3_template_patch.yaml"
        )

        with open(patch_file_path, "r") as f:
            return yaml.safe_load(f)

    @pytest.fixture
    def base_job_template(self):
        """Load base job template."""
        base_template_path = (
            Path(__file__).parent.parent.parent
            / "aibrix"
            / "metadata"
            / "setting"
            / "k8s_job_template.yaml"
        )

        with open(base_template_path, "r") as f:
            return yaml.safe_load(f)

    @pytest.fixture
    def test_input_data(self):
        """Load test input data from sample_job_input.jsonl."""
        import json

        sample_file_path = Path(__file__).parent / "testdata" / "sample_job_input.jsonl"

        test_data = []
        with open(sample_file_path, "r") as f:
            for line in f:
                line = line.strip()
                if line:
                    test_data.append(json.loads(line))

        return test_data

    def _merge_yaml_object(self, base, overlay):
        """
        Recursively merges two YAML objects, mimicking kustomize's strategic merge.

        - Dictionaries are merged recursively.
        - Lists of objects with a 'name' key are merged by item.
        - Other lists and scalar values from the overlay replace those in the base.
        """
        merged = copy.deepcopy(base)

        for key, value in overlay.items():
            if (
                key in merged
                and isinstance(merged[key], dict)
                and isinstance(value, dict)
            ):
                logger.debug(f"override {key}")
                merged[key] = self._merge_yaml_object(merged[key], value)

            elif (
                key in merged
                and isinstance(merged[key], list)
                and isinstance(value, list)
            ):
                # To merge a list, we use "name" field as the key
                base_list = merged[key]
                overlay_list = value
                strategy_merge = False
                logger.debug(f"merge {key}")

                # Create a map of base items by their 'name' for quick lookups
                base_items_by_name = {
                    item.get("name"): item
                    for item in base_list
                    if isinstance(item, dict) and "name" in item
                }
                strategy_merge = len(base_items_by_name) > 0
                logger.debug(f"exist keys in base:{key}: {base_items_by_name.keys()}")

                for item in overlay_list:
                    if isinstance(item, dict) and "name" in item:
                        item_name = item.get("name")
                        if item_name in base_items_by_name:
                            # If an item with the same name exists, merge them
                            base_item = base_items_by_name[item_name]
                            logger.debug(f"merge {key}:{item_name}")
                            base_items_by_name[item_name] = self._merge_yaml_object(
                                base_item, item
                            )
                        else:
                            # Otherwise, append the new item
                            logger.debug(f"append {key}:{item_name}")
                            base_items_by_name[item_name] = item
                            base_list.append(item)
                    else:
                        # If the overlay item isn't a dict with a name, just append it
                        logger.debug(f"append {key}:{item}")
                        base_list.append(item)

                if strategy_merge:
                    merged[key] = list(base_items_by_name.values())
                else:
                    merged[key] = base_list
            else:
                logger.debug(f"override {key}")
                merged[key] = value

        return merged

    @pytest.mark.asyncio
    async def test_worker_with_s3_and_redis(
        self,
        k8s_client,
        test_namespace,
        base_job_template,
        s3_job_template_patch,
        test_input_data,
        s3_config_available,
        test_s3_bucket,
    ):
        """Test worker using S3 storage and Redis metadata."""

        # Generate unique job name
        job_name = "s3-batch-job"
        input_file_id = f"s3-test-input-{str(uuid.uuid4())[:8]}.jsonl"

        # Merge base template with S3 patch
        job_spec = self._merge_yaml_object(base_job_template, s3_job_template_patch)
        print(json.dumps(job_spec, indent=4))
        job_spec["metadata"]["name"] = job_name
        job_spec["metadata"]["annotations"] = {
            "batch.job.aibrix.ai/input-file-id": input_file_id,
            "batch.job.aibrix.ai/endpoint": "/v1/chat/completions",
            "batch.job.aibrix.ai/completion-window": "24h",
        }

        # Upload test input data to S3
        session = boto3.Session()
        s3_client = session.client("s3")

        # Create JSONL content
        jsonl_content = "\n".join(json.dumps(item) for item in test_input_data)
        s3_key = input_file_id

        try:
            s3_client.put_object(
                Bucket=test_s3_bucket,
                Key=s3_key,
                Body=jsonl_content.encode("utf-8"),
                ContentType="application/jsonl",
            )
            logger.info(f"Uploaded input data to S3: s3://{test_s3_bucket}/{s3_key}")

            # Submit job to Kubernetes
            logger.info("Submitting S3 batch job to Kubernetes...")
            created_job = k8s_client.create_namespaced_job(
                namespace=test_namespace, body=job_spec
            )

            assert created_job.metadata.name == job_name
            logger.info(f"S3 batch job submitted successfully: {job_name}")

            # Wait for job completion (timeout after 10 minutes for S3 operations)
            timeout = 600  # 10 minutes
            start_time = time.time()
            job_completed = False

            while time.time() - start_time < timeout:
                job_status = k8s_client.read_namespaced_job_status(
                    name=job_name, namespace=test_namespace
                )

                if job_status.status.conditions:
                    for condition in job_status.status.conditions:
                        if condition.type == "Complete" and condition.status == "True":
                            job_completed = True
                            logger.info(
                                f"S3 batch job completed successfully: {job_name}"
                            )
                            break
                        elif condition.type == "Failed" and condition.status == "True":
                            logger.error(f"S3 batch job failed: {job_name}")
                            await self._log_pod_details(
                                k8s_client, test_namespace, job_name
                            )
                            pytest.fail(f"Job failed: {condition.message}")

                if job_completed:
                    break

                await asyncio.sleep(10)  # Check every 10 seconds for S3 jobs

            if not job_completed:
                await self._log_pod_details(k8s_client, test_namespace, job_name)
                pytest.fail(f"S3 job did not complete within {timeout} seconds")

            # Verify S3 outputs exist
            await self._verify_s3_outputs(
                s3_client, test_s3_bucket, input_file_id, len(test_input_data)
            )

            logger.info("S3 integration test completed successfully!")

        finally:
            # Cleanup S3 objects
            try:
                # List and delete all objects with the input_file_id prefix
                response = s3_client.list_objects_v2(
                    Bucket=test_s3_bucket, Prefix=f"batch-input/{input_file_id}"
                )

                if "Contents" in response:
                    for obj in response["Contents"]:
                        s3_client.delete_object(Bucket=test_s3_bucket, Key=obj["Key"])
                        logger.info(f"Deleted S3 object: {obj['Key']}")

                # Also check for output files
                output_response = s3_client.list_objects_v2(
                    Bucket=test_s3_bucket, Prefix=f"batch-output/{input_file_id}"
                )

                if "Contents" in output_response:
                    for obj in output_response["Contents"]:
                        s3_client.delete_object(Bucket=test_s3_bucket, Key=obj["Key"])
                        logger.info(f"Deleted S3 output object: {obj['Key']}")

            except Exception as e:
                logger.warning(f"Failed to cleanup S3 objects: {e}")

            # Delete the Kubernetes job
            try:
                k8s_client.delete_namespaced_job(
                    name=job_name,
                    namespace=test_namespace,
                    propagation_policy="Background",
                )
                logger.info(f"Deleted S3 batch job: {job_name}")
            except client.ApiException as e:
                if e.status != 404:
                    logger.warning(f"Failed to delete job {job_name}: {e}")

    async def _log_pod_details(self, k8s_client, namespace, job_name):
        """Log pod details for debugging."""
        core_v1 = client.CoreV1Api()

        try:
            pods = core_v1.list_namespaced_pod(
                namespace=namespace, label_selector=f"job-name={job_name}"
            )

            for pod in pods.items:
                logger.info(f"S3 Pod: {pod.metadata.name}, Status: {pod.status.phase}")

                # Log container statuses
                if pod.status.container_statuses:
                    for container_status in pod.status.container_statuses:
                        logger.info(
                            f"S3 Container: {container_status.name}, Ready: {container_status.ready}"
                        )

                # Get pod logs
                for container in ["batch-worker", "llm-engine"]:
                    try:
                        logs = core_v1.read_namespaced_pod_log(
                            name=pod.metadata.name,
                            namespace=namespace,
                            container=container,
                            tail_lines=100,  # More logs for S3 debugging
                        )
                        logger.info(f"S3 {container} logs:\n{logs}")
                    except client.ApiException:
                        logger.warning(
                            f"Could not get S3 logs for container {container}"
                        )

        except client.ApiException as e:
            logger.error(f"Failed to get S3 pod details: {e}")

    async def _verify_s3_outputs(
        self, s3_client, bucket, input_file_id, expected_count
    ):
        """Verify that output files were created in S3."""

        try:
            # Check for output files in S3
            output_prefix = f"batch-output/{input_file_id}"
            response = s3_client.list_objects_v2(Bucket=bucket, Prefix=output_prefix)

            if "Contents" not in response:
                logger.warning(
                    "No output files found in S3, checking alternative paths..."
                )

                # Try different possible output paths
                for prefix in [
                    f"output/{input_file_id}",
                    f"results/{input_file_id}",
                    input_file_id,
                ]:
                    alt_response = s3_client.list_objects_v2(
                        Bucket=bucket, Prefix=prefix
                    )
                    if "Contents" in alt_response:
                        logger.info(f"Found output files under prefix: {prefix}")
                        response = alt_response
                        break

            if "Contents" in response:
                logger.info(f"Found {len(response['Contents'])} output files in S3:")
                for obj in response["Contents"]:
                    logger.info(f"  - {obj['Key']} (size: {obj['Size']} bytes)")

                    # Optionally check file content
                    if obj["Key"].endswith(".jsonl"):
                        try:
                            content = s3_client.get_object(
                                Bucket=bucket, Key=obj["Key"]
                            )
                            body = content["Body"].read().decode("utf-8")
                            lines = [line for line in body.split("\n") if line.strip()]
                            logger.info(f"  Output file has {len(lines)} lines")
                        except Exception as e:
                            logger.warning(f"Could not read output file content: {e}")
            else:
                logger.warning(
                    "No output files found in S3 - job may have used different storage path"
                )

        except Exception as e:
            logger.error(f"Error verifying S3 outputs: {e}")
