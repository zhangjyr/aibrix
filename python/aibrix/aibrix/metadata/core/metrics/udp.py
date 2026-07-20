# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

import socket
from typing import Iterable

from aibrix.logger import init_logger
from aibrix.metadata.core.metrics.sink import Sink, Tag, normalize_tags

logger = init_logger(__name__)


def _resolve_udp_addr(address: str) -> tuple[int, tuple]:
    host, port = address.rsplit(":", 1)
    family, _, _, _, sockaddr = socket.getaddrinfo(
        host, int(port), type=socket.SOCK_DGRAM
    )[0]
    return family, sockaddr


class _UDPSinkBase(Sink):
    def __init__(self, address: str, prefix: str = ""):
        self._family, self._sockaddr = _resolve_udp_addr(address)
        self._socket = socket.socket(self._family, socket.SOCK_DGRAM)
        self._prefix = prefix.strip(".")

    def _metric_name(self, name: str) -> str:
        if self._prefix:
            return f"{self._prefix}.{name}"
        return name

    def _send(self, payload: str) -> None:
        try:
            self._socket.sendto(payload.encode("utf-8"), self._sockaddr)
        except Exception:
            logger.exception("Failed to emit metrics payload", payload=payload)

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        self._send(
            self._format(self._metric_name(name), value, "c", normalize_tags(tags))
        )

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        self._send(
            self._format(self._metric_name(name), value, "g", normalize_tags(tags))
        )

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        self._send(
            self._format(self._metric_name(name), value, "ms", normalize_tags(tags))
        )

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self.gauge(name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self.counter(name, value, *tags)

    def close(self) -> None:
        self._socket.close()

    def _format(
        self, name: str, value: float, metric_type: str, tags: tuple[Tag, ...]
    ) -> str:
        raise NotImplementedError


class StatsdSink(_UDPSinkBase):
    def _format(
        self, name: str, value: float, metric_type: str, tags: tuple[Tag, ...]
    ) -> str:
        return f"{name}:{value}|{metric_type}"


class StatsiteSink(StatsdSink):
    pass


class DogStatsdSink(_UDPSinkBase):
    def __init__(self, address: str, prefix: str = "", global_tags: Iterable[str] = ()):
        super().__init__(address=address, prefix=prefix)
        self._global_tags = tuple(global_tags)

    def _format(
        self, name: str, value: float, metric_type: str, tags: tuple[Tag, ...]
    ) -> str:
        payload = f"{name}:{value}|{metric_type}"
        all_tags = list(self._global_tags)
        all_tags.extend(f"{tag.name}:{tag.value}" for tag in tags)
        if all_tags:
            payload = f"{payload}|#{','.join(all_tags)}"
        return payload
