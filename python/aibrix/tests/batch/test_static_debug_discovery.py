from urllib.parse import urljoin

import pytest

from aibrix.batch.client import DispatchEngine, InferenceRequest
from aibrix.batch.client.sources.static_debug import (
    StaticDebugDiscoveryFactory,
    StaticModelDiscovery,
)


@pytest.mark.asyncio
async def test_static_model_discovery_expands_single_local_endpoint_to_replicas():
    discovery = StaticModelDiscovery(
        ["http://127.0.0.1:18080"],
        replica_count=3,
    )

    snapshot = await discovery.discover_model_endpoints("stress-runtime")

    urls = [endpoint.base_url for endpoint in snapshot.endpoints]
    assert urls == [
        "http://127.0.0.1:18080#pod-0",
        "http://127.0.0.1:18080#pod-1",
        "http://127.0.0.1:18080#pod-2",
    ]
    assert [urljoin(url, "/v1/chat/completions") for url in urls] == [
        "http://127.0.0.1:18080/v1/chat/completions",
        "http://127.0.0.1:18080/v1/chat/completions",
        "http://127.0.0.1:18080/v1/chat/completions",
    ]


@pytest.mark.asyncio
async def test_static_debug_discovery_factory_creates_job_scoped_replica_urls():
    factory = StaticDebugDiscoveryFactory(
        "http://127.0.0.1:18080",
        replica_count=2,
    )

    first = factory.for_job("job-a")
    second = factory.for_job("job-b")

    first_snapshot = await first._discovery.discover_model_endpoints("stress-runtime")
    second_snapshot = await second._discovery.discover_model_endpoints("stress-runtime")

    assert [endpoint.base_url for endpoint in first_snapshot.endpoints] == [
        "http://127.0.0.1:18080#job-job-a-pod-0",
        "http://127.0.0.1:18080#job-job-a-pod-1",
    ]
    assert [endpoint.base_url for endpoint in second_snapshot.endpoints] == [
        "http://127.0.0.1:18080#job-job-b-pod-0",
        "http://127.0.0.1:18080#job-job-b-pod-1",
    ]


@pytest.mark.asyncio
async def test_static_debug_discovery_factory_overrides_capacity_per_job():
    factory = StaticDebugDiscoveryFactory(
        "http://127.0.0.1:18080",
        replica_count=3,
        capacity=24,
    )

    source = factory.for_job("job-a")

    channels = await source.channels()
    capacity = await source.capacity()
    assert len(channels) == 3
    assert capacity.count == 24


@pytest.mark.asyncio
async def test_dispatch_engine_round_robins_across_static_debug_channels(monkeypatch):
    sent_by_channel: list[str] = []

    class _FakeHttpChannel:
        def __init__(self, base_url: str, *, timeout: float = 30.0) -> None:
            del timeout
            self._id = base_url

        @property
        def id(self) -> str:
            return self._id

        async def send(self, request: InferenceRequest):
            sent_by_channel.append(self._id)
            return {"channel": self._id, "ref": request.ref}

        async def aclose(self) -> None:
            return None

    monkeypatch.setattr(
        "aibrix.batch.client.sources.discovery.HttpChannel",
        _FakeHttpChannel,
    )
    factory = StaticDebugDiscoveryFactory(
        "http://127.0.0.1:18080",
        replica_count=3,
        capacity=9,
    )
    source = factory.for_job("job-a")
    engine = DispatchEngine(source)
    results = []

    async def feed():
        for index in range(9):
            yield InferenceRequest(path="/v1/chat/completions", payload={}, ref=index)

    async def on_result(request, response, error):
        results.append((request.ref, response, error))

    await engine.run(feed(), on_result, adaptive_concurrency=False)

    assert [error for _, _, error in results] == [None] * 9
    assert sent_by_channel == [
        "http://127.0.0.1:18080#job-job-a-pod-0",
        "http://127.0.0.1:18080#job-job-a-pod-1",
        "http://127.0.0.1:18080#job-job-a-pod-2",
        "http://127.0.0.1:18080#job-job-a-pod-0",
        "http://127.0.0.1:18080#job-job-a-pod-1",
        "http://127.0.0.1:18080#job-job-a-pod-2",
        "http://127.0.0.1:18080#job-job-a-pod-0",
        "http://127.0.0.1:18080#job-job-a-pod-1",
        "http://127.0.0.1:18080#job-job-a-pod-2",
    ]
