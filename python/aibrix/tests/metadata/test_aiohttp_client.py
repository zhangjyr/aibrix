import contextlib
from unittest.mock import AsyncMock, MagicMock, patch

import aiohttp
import httpx
import pytest

from aibrix.metadata.core.aiohttp_client import AIOHTTPClientWrapper
from aibrix.metadata.core.metrics.sink import T


@pytest.mark.asyncio
async def test_httpx_wrapper_request_records_telemetry():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "aibrix.metadata.core.aiohttp_client.AIOHTTPClientWrapper._request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        response = await wrapper.request(
            "GET",
            "https://example.com/healthz",
            telemetry_call_site="tests.metadata.test_aiohttp_client.success",
        )

    assert response.status_code == 200
    assert wrapper._telemetry_stats.started == 1
    assert wrapper._telemetry_stats.completed == 1
    assert wrapper._telemetry_stats.failed == 0
    assert wrapper._telemetry_stats.inflight == 0
    snapshot = wrapper._telemetry_stats.snapshot(reset_window=False)
    assert len(snapshot.call_sites) == 1
    assert (
        snapshot.call_sites[0].call_site == "tests.metadata.test_aiohttp_client.success"
    )
    assert snapshot.p50_latency_seconds is not None
    assert snapshot.p95_latency_seconds is not None
    assert sum(snapshot.latency_histogram) == 1

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_shares_telemetry_stats_by_purpose():
    first = AIOHTTPClientWrapper(
        client_id="test-wrapper-purpose-1",
        purpose="shared-aiohttp-purpose",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    second = AIOHTTPClientWrapper(
        client_id="test-wrapper-purpose-2",
        purpose="shared-aiohttp-purpose",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "aibrix.metadata.core.aiohttp_client.AIOHTTPClientWrapper._request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        await first.request("GET", "https://example.com/first")
        await second.request("GET", "https://example.com/second")

    assert first._telemetry_stats is second._telemetry_stats
    assert first._telemetry_stats.started == 2
    assert second._telemetry_stats.completed == 2

    await first.stop()
    await second.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_flushes_shared_purpose_final_telemetry_once():
    first = AIOHTTPClientWrapper(
        client_id="test-wrapper-purpose-final-1",
        purpose="shared-aiohttp-purpose-final",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    second = AIOHTTPClientWrapper(
        client_id="test-wrapper-purpose-final-2",
        purpose="shared-aiohttp-purpose-final",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "aibrix.metadata.core.aiohttp_client.AIOHTTPClientWrapper._request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        await first.request("GET", "https://example.com/first")
        await second.request("GET", "https://example.com/second")

    gauges = []
    with patch(
        "aibrix.metadata.core.http_client.Emitter.gauge",
        lambda name, value, *tags: gauges.append((name, value, tags)),
    ):
        await first.stop()
        await second.stop()

    summary_gauges = [
        (value, tags)
        for name, value, tags in gauges
        if name == "metadata_http_client_telemetry_summary"
    ]
    assert len(summary_gauges) == 1
    assert T("purpose", "shared-aiohttp-purpose-final") in summary_gauges[0][1]
    assert T("flush", "true") in summary_gauges[0][1]
    metric_values = {name: value for name, value, _ in gauges}
    assert metric_values["metadata_http_client_telemetry_registered_clients"] == 0.0
    assert metric_values["metadata_http_client_telemetry_window_started"] == 2.0
    assert metric_values["metadata_http_client_telemetry_window_completed"] == 2.0


@pytest.mark.asyncio
async def test_httpx_wrapper_request_records_failure():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-failure",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "aibrix.metadata.core.aiohttp_client.AIOHTTPClientWrapper._request",
        new=AsyncMock(side_effect=httpx.ConnectError("boom")),
    ):
        with pytest.raises(httpx.ConnectError):
            await wrapper.request(
                "GET",
                "https://example.com/healthz",
                telemetry_call_site="tests.metadata.test_aiohttp_client.failure",
            )

    assert wrapper._telemetry_stats.started == 1
    assert wrapper._telemetry_stats.completed == 1
    assert wrapper._telemetry_stats.failed == 1
    assert wrapper._telemetry_stats.inflight == 0
    snapshot = wrapper._telemetry_stats.snapshot(reset_window=False)
    assert len(snapshot.call_sites) == 1
    assert (
        snapshot.call_sites[0].call_site == "tests.metadata.test_aiohttp_client.failure"
    )
    assert snapshot.call_sites[0].exception_types == {"ConnectError": 1}
    assert snapshot.exception_types == {"ConnectError": 1}

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_emits_debug_telemetry_by_call_site():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-debug",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "aibrix.metadata.core.aiohttp_client.AIOHTTPClientWrapper._request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        await wrapper.request(
            "GET",
            "https://example.com/healthz",
            telemetry_call_site="tests.metadata.test_aiohttp_client.debug",
        )

    with patch("aibrix.metadata.core.http_client.logger.debug") as mock_debug:
        wrapper._emit_telemetry(final=False, elapsed=1.0)

    mock_debug.assert_called_once()
    _, kwargs = mock_debug.call_args
    assert kwargs["client_id"] == "test-wrapper-debug"
    assert kwargs["purpose"] is None
    assert kwargs["call_site"] == "tests.metadata.test_aiohttp_client.debug"
    assert kwargs["failed"] == 0
    assert kwargs["exception_types"] == {}

    await wrapper.stop()


def test_httpx_wrapper_emits_telemetry_numeric_metrics(monkeypatch):
    gauges = []

    monkeypatch.setattr(
        "aibrix.metadata.core.http_client.Emitter.gauge",
        lambda name, value, *tags: gauges.append((name, value, tags)),
    )
    wrapper = AIOHTTPClientWrapper(
        client_id="test-httpx-telemetry",
        purpose="batch_runtime_http",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    wrapper._telemetry_stats.record_start(call_site="call.site")
    wrapper._telemetry_stats.record_completion(
        call_site="call.site",
        failed=False,
        latency_seconds=0.123,
        exception_type=None,
    )

    wrapper._emit_telemetry(final=False, elapsed=1.0)

    emitted_names = {name for name, _, _ in gauges}
    assert "metadata_http_client_telemetry_completed" in emitted_names
    assert "metadata_http_client_telemetry_completed_qps" in emitted_names
    assert "metadata_http_client_telemetry_summary" in emitted_names
    completed_tags = next(
        tags
        for name, _, tags in gauges
        if name == "metadata_http_client_telemetry_completed"
    )
    assert T("log_field", "completed") in completed_tags
    summary_gauges = [
        (value, tags)
        for name, value, tags in gauges
        if name == "metadata_http_client_telemetry_summary"
    ]
    assert summary_gauges
    assert summary_gauges[0][0] == 1.0
    assert T("flush", "true") in summary_gauges[0][1]
    assert T("event", "HTTP client telemetry summary") in summary_gauges[0][1]


@pytest.mark.asyncio
async def test_httpx_wrapper_context_manager_starts_and_stops():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-context",
        telemetry_interval_seconds=0,
    )

    async with wrapper as managed:
        assert isinstance(managed.async_client, aiohttp.ClientSession)
        assert managed.is_closed is False

    assert wrapper.async_client is None
    assert wrapper.is_closed is True


@pytest.mark.asyncio
async def test_httpx_wrapper_cannot_be_reused_after_stop():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-stopped",
        telemetry_interval_seconds=0,
    )

    with patch(
        "aibrix.metadata.core.aiohttp_client.AIOHTTPClientWrapper._request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        response = await wrapper.request("GET", "https://example.com/healthz")

    assert response.status_code == 200

    await wrapper.stop()

    with pytest.raises(RuntimeError, match="cannot be reused after stop"):
        await wrapper.request("GET", "https://example.com/healthz")

    with pytest.raises(RuntimeError, match="cannot be reused after stop"):
        _ = wrapper.headers


def test_httpx_wrapper_preserves_httpx_redirect_defaults():
    default_wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-redirect-default",
        telemetry_interval_seconds=0,
    )
    assert default_wrapper._aiohttp_request_kwargs({})["allow_redirects"] is False

    follow_wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-redirect-follow",
        telemetry_interval_seconds=0,
        follow_redirects=True,
    )
    assert follow_wrapper._aiohttp_request_kwargs({})["allow_redirects"] is True
    assert (
        follow_wrapper._aiohttp_request_kwargs({"follow_redirects": False})[
            "allow_redirects"
        ]
        is False
    )


def test_httpx_wrapper_maps_httpx_timeout_to_aiohttp_timeout():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-timeout",
        telemetry_interval_seconds=0,
    )

    timeout = wrapper._aiohttp_timeout(
        httpx.Timeout(connect=10.0, read=20.0, write=30.0, pool=40.0)
    )

    assert timeout.total is None
    assert timeout.connect == 40.0
    assert timeout.sock_connect == 10.0
    assert timeout.sock_read == 20.0


@pytest.mark.asyncio
async def test_httpx_wrapper_preserves_httpx_default_timeout():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-default-timeout",
        telemetry_interval_seconds=0,
    )

    try:
        wrapper.start()
        assert wrapper.async_client is not None
        assert wrapper.async_client.timeout.total is None
        assert wrapper.async_client.timeout.connect == 5.0
        assert wrapper.async_client.timeout.sock_connect == 5.0
        assert wrapper.async_client.timeout.sock_read == 5.0
    finally:
        await wrapper.stop()


def test_httpx_wrapper_request_timeout_none_disables_timeout():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-request-timeout-none",
        telemetry_interval_seconds=0,
    )

    request_timeout = wrapper._aiohttp_request_kwargs({"timeout": None})["timeout"]

    assert request_timeout.total is None
    assert request_timeout.connect is None
    assert request_timeout.sock_connect is None
    assert request_timeout.sock_read is None


@pytest.mark.asyncio
async def test_httpx_wrapper_stop_uses_actual_elapsed_time_for_final_telemetry():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-final-elapsed",
        telemetry_enabled=True,
        telemetry_interval_seconds=60,
    )

    wrapper.start()
    assert wrapper.async_client is not None

    wrapper._telemetry_stats.record_start(
        call_site="tests.metadata.test_aiohttp_client.final_elapsed"
    )
    wrapper._telemetry_stats.record_completion(
        failed=False,
        latency_seconds=0.1,
        call_site="tests.metadata.test_aiohttp_client.final_elapsed",
    )
    wrapper._last_telemetry_emitted_at = 100.0

    gauges = []
    with (
        patch.object(wrapper.async_client, "close", new=AsyncMock()),
        patch(
            "aibrix.metadata.core.http_client.asyncio.get_running_loop",
            return_value=MagicMock(time=MagicMock(return_value=100.25)),
        ),
        patch(
            "aibrix.metadata.core.http_client.Emitter.gauge",
            lambda name, value, *tags: gauges.append((name, value, tags)),
        ),
    ):
        await wrapper.stop()

    summary_gauge = next(
        tags
        for name, _, tags in gauges
        if name == "metadata_http_client_telemetry_summary"
    )
    assert T("final", "true") in summary_gauge
    assert T("purpose", "") in summary_gauge
    metric_values = {name: value for name, value, _ in gauges}
    assert metric_values["metadata_http_client_telemetry_started_qps"] == 4.0
    assert metric_values["metadata_http_client_telemetry_completed_qps"] == 4.0
    assert metric_values["metadata_http_client_telemetry_failed_qps"] == 0.0


@pytest.mark.asyncio
async def test_httpx_wrapper_stream_returns_aiter_bytes_compatible_response():
    wrapper = AIOHTTPClientWrapper(
        client_id="test-wrapper-stream",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    response = httpx.Response(200, content=b"abcdef")

    @contextlib.asynccontextmanager
    async def fake_stream(*args, **kwargs):
        del args, kwargs
        yield response

    with patch(
        "aibrix.metadata.core.aiohttp_client.AIOHTTPClientWrapper._stream",
        fake_stream,
    ):
        async with wrapper.stream(
            "GET",
            "https://example.com/stream",
            telemetry_call_site="tests.metadata.test_aiohttp_client.stream",
        ) as streamed:
            chunks = [chunk async for chunk in streamed.aiter_bytes(chunk_size=2)]

    assert chunks == [b"ab", b"cd", b"ef"]
    assert wrapper._telemetry_stats.started == 1
    assert wrapper._telemetry_stats.completed == 1
    assert wrapper._telemetry_stats.failed == 0

    await wrapper.stop()
