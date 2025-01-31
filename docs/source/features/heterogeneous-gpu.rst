.. _heterogeneous-gpu:

============================
Heterogeneous GPU Inference
============================

Heterogeneous GPU Inference is a feature that enables users to utilize different types of GPUs for deploying the same model. This feature addresses two primary challenges associated with Large Language Model (LLM) inference: (1) As the demand for large-scale model inference increases, ensuring consistent GPU availability has become a challenge, particularly within regions where identical GPU types are often unavailable due to capacity constraints. (2) Users may seek to incorporate lower-cost, lower-performance GPUs to reduce overall expenses. 

Design Overview
---------------

There are three main components in Heterogeneous GPU Inference Feature: (1) LLM Request Monitoring, (2) Heterogeneous GPU Optimizer, (3) Request Routing. The following figure shows the overall architecture. First, LLM Request Monitoring component is responsible for monitoring the past inference requests and their request patterns. Second, Heterogeneous GPU Optimizer component is responsible for selecting the optimal GPU type and the corresponding GPU count. Third, Request Routing component is responsible for routing the request to the optimal GPU.


.. figure:: ../assets/images/heterogeneous-gpu-diagram.png
  :alt: heterogeneous-gpu-diagram
  :width: 100%
  :align: center


Example
-------------

Step 1: Deploy the heterogeneous deployments. One deployment and corresponding PodAutoscaler should be deployed for each GPU type.
See `deployment/app/config/heterogeneous` for an example of heterogeneous configuration composed of two GPU types. The following codes 
deploy local heterogeneous deployments using simulator.

.. code-block:: bash

    export AIBRIX_HOME="${PWD}"       # set the project root environment variable:
    cd $AIBRIX_HOME/development/app/
    make docker-build-simulator       #build mock workload for a100
    make docker-build-simulator-a40   #build mock workload for a40
    make deploy-heterogeneous         #deploy heterogeneous a100 and a40 workload

After deployment, you will see a llama2-7b inference service with two pods running on simulated A100 and A40 GPUs:

.. code-block:: bash

    kubectl get svc
    NAME         TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)          AGE
    kubernetes   ClusterIP   10.96.0.1       <none>        443/TCP          14d
    llama2-7b    NodePort    10.107.122.88   <none>        8000:30081/TCP   48m

Incoming requests are routed through the gateway and directed to the optimal pod based on request patterns:

.. code-block:: bash

    kubectl get pods
    NAME                                       READY   STATUS        RESTARTS      AGE
    simulator-llama2-7b-a100-9bdfbb7ff-rx9r7   2/2     Running       0             46m
    simulator-llama2-7b-a40-5c9576c566-jfblm   2/2     Running       0             27s

Step 2: Activate poetry for python execution. See `python/aibrix/README.md` for details. You will need run:

.. code-block:: bash

    cd $AIBRIX_HOME/python/aibrix
    poetry install --no-root --with dev
    poetry shell

.. note::

  The GPU Optimizer runs continuously in the background, dynamically adjusting GPU allocation for each model based on workload patterns.
  Note that GPU optimizer requires offline inference performance benchmark data for each type of GPU on each specific LLM model.
  If local heterogeneous deployments is used, you can find the prepared benchmark data under `python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/result/` and skip Step 3.

Step 3: Benchmark model. For each type of GPU, using local heterogeneous deployments as an example, run:

.. code-block:: bash

    kubectl -n aibrix-system port-forward [pod_name] 8010:8000 1>/dev/null 2>&1 &
    # Wait for port-forward taking effect.
    cd $AIBRIX_HOME/python/aibrix/aibrix/gpu_optimizer
    make DP=simulator-llama2-7b-a100 benchmark # See optimizer/profiling/benchmark.sh for more options.

Step 4: Decide SLO and generate profile. Using local heterogeneous deployments as an example, run:
  
.. code-block:: bash

    kubectl -n aibrix-system port-forward svc/aibrix-redis-master 6379:6379 1>/dev/null 2>&1 &
    # Wait for port-forward taking effect.
    cd $AIBRIX_HOME/python/aibrix/aibrix/gpu_optimizer
    make DP=simulator-llama2-7b-a100 COST=1.0 gen-profile # Run python optimizer/profiling/gen_profile.py -h for SLO options.
    make DP=simulator-llama2-7b-a40 COST=0.3 gen-profile

Now that the GPU Optimizer is ready to work. You should observe that the number of workload pods changes in response to the requests sent to the gateway.

.. note::

  Requests must be routed through gateway for the GPU optimizer to work as expected.

Miscellaneous
-------------

Once the GPU optimizer finishes the scaling optimization, the output of the GPU optimizer is passed to PodAutoscaler as a metricSource via a designated HTTP endpoint for the final scaling decision.  In the above local a100 and a40 deployment files, we configure the PodAutoscaler spec (using a40 as an example).

.. code-block:: yaml

    apiVersion: autoscaling.aibrix.ai/v1alpha1
    kind: PodAutoscaler
    metadata:
      name: podautoscaler-simulator-llama2-7b-a40
      labels:
        app.kubernetes.io/name: aibrix
        app.kubernetes.io/managed-by: kustomize
        kpa.autoscaling.aibrix.ai/scale-down-delay: 0s
      namespace: default
    spec:
      scaleTargetRef:
        apiVersion: apps/v1
        kind: Deployment
        name: simulator-llama2-7b-a40 # replace with corresponding deployment name
      minReplicas: 0
      maxReplicas: 10
      metricsSources: 
        - metricSourceType: domain
          protocolType: http
          endpoint: aibrix-gpu-optimizer.aibrix-system.svc.cluster.local:8080
          path: /metrics/default/simulator-llama2-7b-a40 # replace with /metrics/default/[deployment name]
          targetMetric: "vllm:deployment_replicas"
          targetValue: "1"
      scalingStrategy: "KPA"

To avoiding scaling down to 0 workload pod when there is no workload, a new label  ``model.aibrix.ai/min_replicas`` in the deployment file is used to specify the minimum number of replicas.

.. code-block:: yaml

    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: simulator-llama2-7b-a100
      labels:
        model.aibrix.ai/name: "llama2-7b"
        model.aibrix.ai/min_replicas: "1" # min replica for gpu optimizer when no workloads.
    ... rest yaml deployments

