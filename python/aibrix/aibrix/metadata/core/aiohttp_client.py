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
import contextlib
from typing import Any, AsyncIterator, Optional

import aiohttp
import httpx

from aibrix.metadata.core.http_client import HTTPClient

_HTTPX_DEFAULT_TIMEOUT = httpx.Timeout(5.0)


class _AIOHTTPStreamResponse:
    def __init__(
        self,
        response: aiohttp.ClientResponse,
        *,
        method: str,
        url: str,
    ) -> None:
        self._response = response
        self._request = httpx.Request(method, url)
        self.status_code = response.status
        self.headers = response.headers

    async def aiter_bytes(self, chunk_size: int = 65536) -> AsyncIterator[bytes]:
        async for chunk in self._response.content.iter_chunked(chunk_size):
            yield chunk

    def raise_for_status(self) -> None:
        if 400 <= self.status_code:
            raise httpx.HTTPStatusError(
                f"HTTP {self.status_code}",
                request=self._request,
                response=httpx.Response(
                    self.status_code,
                    headers=dict(self.headers),
                    request=self._request,
                ),
            )


class AIOHTTPClientWrapper(HTTPClient):
    async_client: Optional[aiohttp.ClientSession] = None
    client_label = "aiohttp-client"
    instantiated_log_message = "aiohttp.ClientSession instantiated."
    closed_log_message = "aiohttp.ClientSession closed"

    def __init__(self, **client_kwargs: Any) -> None:
        self._limits = client_kwargs.pop("limits", None)
        self._default_timeout = client_kwargs.pop("timeout", _HTTPX_DEFAULT_TIMEOUT)
        self._limit_per_host = client_kwargs.pop("aiohttp_limit_per_host", 0)
        self._follow_redirects = bool(client_kwargs.pop("follow_redirects", False))
        super().__init__(**client_kwargs)

    @property
    def is_closed(self) -> bool:
        return self.async_client.closed if self.async_client is not None else True

    def _create_async_client(self) -> aiohttp.ClientSession:
        return aiohttp.ClientSession(
            connector=aiohttp.TCPConnector(
                limit=self._max_connections(),
                limit_per_host=self._limit_per_host,
                keepalive_timeout=self._keepalive_expiry(),
            ),
            timeout=self._aiohttp_timeout(self._default_timeout),
            **self._aiohttp_client_kwargs(),
        )

    async def _close_async_client(self) -> None:
        assert self.async_client is not None
        await self.async_client.close()

    async def _request(
        self,
        method: str,
        url: str,
        **kwargs: Any,
    ) -> httpx.Response:
        assert self.async_client is not None
        request = httpx.Request(method, url)
        try:
            async with self.async_client.request(
                method,
                url,
                **self._aiohttp_request_kwargs(kwargs),
            ) as response:
                content = await response.read()
                return httpx.Response(
                    response.status,
                    headers=dict(response.headers),
                    content=content,
                    request=request,
                )
        except (asyncio.TimeoutError, aiohttp.ServerTimeoutError) as exc:
            raise httpx.TimeoutException(str(exc), request=request) from exc
        except aiohttp.ClientError as exc:
            raise httpx.TransportError(str(exc), request=request) from exc

    @contextlib.asynccontextmanager
    async def _stream(
        self,
        method: str,
        url: str,
        **kwargs: Any,
    ):
        assert self.async_client is not None
        request = httpx.Request(method, url)
        response: aiohttp.ClientResponse | None = None
        try:
            response = await self.async_client.request(
                method,
                url,
                **self._aiohttp_request_kwargs(kwargs),
            )
            yield _AIOHTTPStreamResponse(response, method=method, url=url)
        except (asyncio.TimeoutError, aiohttp.ServerTimeoutError) as exc:
            raise httpx.TimeoutException(str(exc), request=request) from exc
        except aiohttp.ClientError as exc:
            raise httpx.TransportError(str(exc), request=request) from exc
        finally:
            if response is not None:
                response.release()

    def _aiohttp_client_kwargs(self) -> dict[str, Any]:
        return dict(self._client_kwargs)

    def _aiohttp_request_kwargs(self, kwargs: dict[str, Any]) -> dict[str, Any]:
        request_kwargs = dict(kwargs)
        if "timeout" in request_kwargs:
            timeout = request_kwargs.pop("timeout")
            request_kwargs["timeout"] = self._aiohttp_timeout(timeout)
        follow_redirects = request_kwargs.pop(
            "follow_redirects", self._follow_redirects
        )
        request_kwargs["allow_redirects"] = follow_redirects
        content = request_kwargs.pop("content", None)
        if content is not None and "data" not in request_kwargs:
            request_kwargs["data"] = content
        return request_kwargs

    def _aiohttp_timeout(self, timeout: Any) -> aiohttp.ClientTimeout:
        if timeout is None:
            return aiohttp.ClientTimeout(total=None)
        if isinstance(timeout, httpx.Timeout):
            return aiohttp.ClientTimeout(
                total=None,
                connect=timeout.pool,
                sock_connect=timeout.connect,
                sock_read=timeout.read,
            )
        return aiohttp.ClientTimeout(total=float(timeout))

    def _max_connections(self) -> int:
        if isinstance(self._limits, httpx.Limits):
            return self._limits.max_connections or 0
        return 100

    def _keepalive_expiry(self) -> float:
        if isinstance(self._limits, httpx.Limits):
            return self._limits.keepalive_expiry or 3.0
        return 3.0
