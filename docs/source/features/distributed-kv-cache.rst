.. _distributed-kv-cache:

====================
Distributed KV Cache
====================

.. warning::
    Currently, distributed KV cache only supports FlashAttention.

The rising demand for large language models has intensified the need for efficient memory management and caching to optimize inference performance and reduce costs. In multi-round use cases like chatbots and agent-based systems, overlapping token sequences lead to redundant computations during the prefill phase, wasting resources and limiting throughput.

Many inference engines, such as `vLLM <https://github.com/vllm-project/vllm>`_, use built-in KV caching to mitigate this issue, leveraging idle HBM and DRAM. However, single-node KV caches face key limitations: constrained memory capacity, engine-specific storage that prevents sharing across instances, and difficulty supporting scenarios like KV migration and prefill-decode disaggregation.

AIBrix addresses these challenges with a distributed KV cache, enabling high-capacity, cross-engine KV reuse while optimizing network and memory efficiency. Our solution employs a scan-resistant eviction policy to persist hot KV tensors selectively, ensuring that network and memory usage is optimized by minimizing unnecessary data transfers, asynchronous metadata updates to reduce overhead, and cache-engine colocation for faster data transfer via shared memory.

.. figure:: ../assets/images/aibrix-dist-kv-cache-arch-overview.png
  :alt: distributed-kv-cache-arch-overview
  :width: 100%
  :align: center

Example
-------

.. note::
    We use a customized version of `vineyard <https://v6d.io/>`_ as the backend for distributed KV cache and an internal version of vLLM integrated with distributed KV cache support to showcase the usage. We are working with the vLLM community to upstream the distributed KV cache API and plugin.


After deployment, we can see all the components by using ``kubectl get pods -n aibrix-system`` command:

.. code-block:: RST

    NAME                                        READY   STATUS    RESTARTS   AGE
    deepseek-coder-7b-kvcache-596965997-p86cx   0/1     Pending   0          2m
    deepseek-coder-7b-kvcache-etcd-0            1/1     Running   0          2m

.. note::
    ``deepseek-coder-7b-kvcache-596965997-p86cx`` is pending and waiting for inference engine to be deployed, this is normal.

After all components are created, we can use the following yaml to deploy the inference service:

.. literalinclude:: ../../../samples/kvcache/deployment.yaml
   :language: yaml

.. note::
    * ``metadata.name`` MUST match with ``kvcache.orchestration.aibrix.ai/pod-affinity-workload`` in the kv cache deployment
    * We need to include the Unix domain socket used by the distributed KV cache as a volume to the inference service pod (i.e., ``kvcache-socket`` in the example above)

.. note::
    ``VINEYARD_CACHE_CPU_MEM_LIMIT_GB`` needs to choose a proper value based on the pod memory resource requirement. For instance, if the pod memory resource requirement is ``P`` GB and the estimated memory consumption of the inference engine is ``E`` GB, we can set ``VINEYARD_CACHE_CPU_MEM_LIMIT_GB`` to ``P / tensor-parallel-size - E``.

Now let's use ``kubectl get pods`` command to ensure the inference service is running:

.. code-block:: RST

    NAME                                          READY   STATUS    RESTARTS   AGE
    deepseek-coder-7b-instruct-6b885ffd8b-2kfjv   2/2     Running   0          4m


After launching AIBrix's deployment, we can use the following yaml to deploy a distributed KV cache cluster:

.. literalinclude:: ../../../samples/kvcache/kvcache.yaml
   :language: yaml

.. note::

    1. ``kvcache.orchestration.aibrix.ai/pod-affinity-workload`` MUST match with ``metadata.name`` of the inference service deployment below
    2. ``kvcache.orchestration.aibrix.ai/node-affinity-gpu-type`` is unnecessary unless you deploy the model across different GPUs.


Run ``kubectl get pods -o wide`` to verify all pods are running.

.. code-block:: RST

    NAME                                            READY   STATUS              RESTARTS   AGE     IP               NODE                                           NOMINATED NODE   READINESS GATES
    deepseek-coder-7b-instruct-85664648c7-xgp9h     1/1     Running             0          2m41s   192.168.59.224   ip-192-168-41-184.us-west-2.compute.internal   <none>           <none>
    deepseek-coder-7b-kvcache-7d5896cd89-dcfzt      1/1     Running             0          2m31s   192.168.37.154   ip-192-168-41-184.us-west-2.compute.internal   <none>           <none>
    deepseek-coder-7b-kvcache-etcd-0                1/1     Running             0          2m31s   192.168.19.197   ip-192-168-3-183.us-west-2.compute.internal    <none>           <none>


Once the inference service is running, let's set up port forwarding so that we can test the service from local:

* Run ``kubectl get svc -n envoy-gateway-system`` to get the name of the Envoy Gateway service.

.. code-block:: RST

    NAME                                     TYPE           CLUSTER-IP       EXTERNAL-IP                                       PORT(S)                                   AGE
    envoy-aibrix-system-aibrix-eg-903790dc   LoadBalancer   172.19.190.6     10.0.1.4,2406:d440:105:cf01:6f1b:7f4d:12da:c5a5   80:30904/TCP                              3d

* Run ``kubectl -n envoy-gateway-system port-forward svc/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &`` to set up port forwarding

.. code-block:: RST

    Forwarding from 127.0.0.1:8888 -> 10080
    Forwarding from [::1]:8888 -> 10080

Now, let's test the service:

.. code-block:: shell

    curl -v "http://localhost:8888/v1/chat/completions" \
      -H "Content-Type: application/json" \
      -H "Authorization: XXXXXXXXXXXXXXXXXXXXXXXX" \
      -d '{
         "model": "deepseek-coder-7b-instruct",
         "messages": [{"role": "user", "content": "Created container vllm-openai"}],
         "temperature": 0.7
       }'

and its output would be:

.. code-block:: RST

    *   Trying [::1]:8888...
    * Connected to localhost (::1) port 8888
    > POST /v1/chat/completions HTTP/1.1
    > Host: localhost:8888
    > User-Agent: curl/8.4.0
    > Accept: */*
    > Content-Type: application/json
    Handling connection for 8888
    > Authorization: XXXXXXXXXXXXXXXXXXXXXXXX
    > Content-Length: 174
    >
    < HTTP/1.1 200 OK
    < date: Thu, 30 Jan 2025 23:50:08 GMT
    < server: uvicorn
    < content-type: application/json
    < x-went-into-resp-headers: true
    < transfer-encoding: chunked
    <
    * Connection #0 to host localhost left intact
    {
      "id": "chat-60f0247aa9294f8abb61e8f24c1503c2",
      "object": "chat.completion",
      "created": 1738281009,
      "model": "deepseek-coder-7b-instruct",
      "choices": [
        {
          "index": 0,
          "message": {
            "role": "assistant",
            "content": "It seems like you're trying to create a container with the name \"vllm-openai\". However, your question is missing some context. Could you please provide more details? Are you using Docker, Kubernetes, or another container orchestration tool? Or are you asking how to create a container for a specific application or service? The details will help me provide a more accurate answer.",
            "tool_calls": []
          },
          "logprobs": null,
          "finish_reason": "stop",
          "stop_reason": null
        }
      ],
      "usage": {
        "prompt_tokens": 76,
        "total_tokens": 161,
        "completion_tokens": 85
      },
      "prompt_logprobs": null
    }

Distribute KV cache metrics can be viewed in the AIBrix Engine Dashboard. The following is an example of the dashboard panels for the distributed KV cache:

.. figure:: ../assets/images/aibrix-dist-kv-cache-dashboard.png
  :alt: distributed-kv-cache-dashboard
  :width: 100%
  :align: center
