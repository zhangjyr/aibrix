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

import threading

import kopf
import pytest
from kubernetes import config

from aibrix.metadata.cache.job import JobCache

# Use a threading.Event to signal when the operator is ready
OPERATOR_READY = threading.Event()


def run_operator_in_thread():
    """The target function for the operator thread."""
    # The 'ready_flag' is a special kopf argument that gets set
    # when the operator has started and is ready to handle events.
    kopf.run(
        standalone=True,
        ready_flag=OPERATOR_READY,
        namespace="default",  # Monitor default namespace for tests
    )


@pytest.fixture(scope="session")
def kopf_operator():
    """
    A session-scoped fixture to run the kopf operator in a background thread.
    This ensures JobCache handlers are properly triggered during tests.
    """
    try:
        # Make sure we have Kubernetes config loaded
        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()

        # Start the kopf operator in a daemon thread
        print("--- Starting kopf operator in background thread ---")
        operator_thread = threading.Thread(target=run_operator_in_thread)
        operator_thread.daemon = True
        operator_thread.start()

        # Wait for the operator to be ready
        if not OPERATOR_READY.wait(timeout=30):
            pytest.fail("Kopf operator did not start in time")

        print("--- Kopf operator is ready, yielding to tests ---")
        yield  # Tests run here

    finally:
        print("\n--- Kopf operator test session finished ---")


@pytest.fixture(scope="function")
def job_cache(kopf_operator):
    """
    Function-scoped fixture that provides a JobCache instance.
    The kopf_operator fixture ensures the operator is running.
    """
    return JobCache()
