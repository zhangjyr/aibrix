# Copyright 2026 The Aibrix Team.
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
"""Static discovery helpers for local debug and stress tests."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional, Sequence
from urllib.parse import urldefrag

from aibrix.batch.client.source import CapacitySignal
from aibrix.batch.client.sources.discovery import DiscoveryEndpointSource


@dataclass(frozen=True, slots=True)
class StaticDiscoveredEndpoint:
    base_url: str


@dataclass(frozen=True, slots=True)
class StaticDiscoverySnapshot:
    version: int
    endpoints: Sequence[StaticDiscoveredEndpoint]


class StaticModelDiscovery:
    """Deterministic discovery adapter for local/stress runtime endpoints."""

    def __init__(
        self,
        base_urls: Sequence[str],
        *,
        version: int = 1,
        replica_count: int = 1,
        job_id: str | None = None,
    ) -> None:
        urls = _expand_replica_urls(
            base_urls,
            replica_count=replica_count,
            job_id=job_id,
        )
        self._snapshot = StaticDiscoverySnapshot(
            version=version,
            endpoints=tuple(StaticDiscoveredEndpoint(url) for url in urls),
        )

    async def discover_model_endpoints(
        self, served_model_name: str, service_id: Optional[str] = None
    ) -> StaticDiscoverySnapshot:
        del served_model_name, service_id
        return self._snapshot


class StaticDebugDiscoveryFactory:
    """Creates one discovery endpoint source per job for local stress tests."""

    def __init__(
        self,
        base_url: str,
        *,
        replica_count: int = 1,
        capacity: int = 1,
        timeout: float = 30.0,
    ) -> None:
        self._base_url = base_url
        self._replica_count = max(int(replica_count), 1)
        self._capacity = max(int(capacity), 1)
        self._timeout = timeout
        self._sources: list[DiscoveryEndpointSource] = []

    def for_job(self, job_id: str) -> DiscoveryEndpointSource:
        source = StaticDebugEndpointSource(
            StaticModelDiscovery(
                [self._base_url],
                replica_count=self._replica_count,
                job_id=job_id,
            ),
            served_model_name="stress-runtime",
            capacity=self._capacity,
            timeout=self._timeout,
        )
        self._sources.append(source)
        return source

    async def aclose(self) -> None:
        for source in self._sources:
            await source.aclose()
        self._sources = []


class StaticDebugEndpointSource(DiscoveryEndpointSource):
    """Discovery source with local stress capacity decoupled from URL count."""

    def __init__(self, *args, capacity: int = 1, **kwargs) -> None:
        self._capacity = max(int(capacity), 1)
        super().__init__(
            *args,
            max_connections_per_endpoint=self._capacity,
            **kwargs,
        )

    @property
    def configured_capacity(self) -> int:
        return self._capacity

    async def capacity(self) -> CapacitySignal:
        signal = await super().capacity()
        return CapacitySignal(count=self._capacity, version=signal.version)

    async def wait_capacity_change(self, previous: CapacitySignal) -> CapacitySignal:
        source_previous = CapacitySignal(
            count=len(self._channels),
            version=previous.version,
        )
        signal = await super().wait_capacity_change(source_previous)
        return CapacitySignal(count=self._capacity, version=signal.version)


def _expand_replica_urls(
    base_urls: Sequence[str], *, replica_count: int, job_id: str | None = None
) -> tuple[str, ...]:
    replica_count = max(int(replica_count), 1)
    if len(base_urls) != 1:
        return tuple(base_urls)

    base_url, _ = urldefrag(base_urls[0])
    prefix = f"job-{job_id}-pod" if job_id is not None else "pod"
    return tuple(f"{base_url}#{prefix}-{index}" for index in range(replica_count))
