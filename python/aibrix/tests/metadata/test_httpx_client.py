from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

from aibrix.metadata.core import HTTPXClientWrapper


@pytest.mark.asyncio
async def test_httpx_wrapper_request_records_telemetry():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        response = await wrapper.request(
            "GET",
            "https://example.com/healthz",
            telemetry_call_site="tests.metadata.test_httpx_client.success",
        )

    assert response.status_code == 200
    assert wrapper._telemetry_stats.started == 1
    assert wrapper._telemetry_stats.completed == 1
    assert wrapper._telemetry_stats.failed == 0
    assert wrapper._telemetry_stats.inflight == 0
    snapshot = wrapper._telemetry_stats.snapshot(reset_window=False)
    assert len(snapshot.call_sites) == 1
    assert (
        snapshot.call_sites[0].call_site == "tests.metadata.test_httpx_client.success"
    )
    assert snapshot.p50_latency_seconds is not None
    assert snapshot.p95_latency_seconds is not None
    assert sum(snapshot.latency_histogram) == 1

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_shares_telemetry_stats_by_purpose():
    first = HTTPXClientWrapper(
        client_id="test-wrapper-purpose-1",
        purpose="shared-purpose",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    second = HTTPXClientWrapper(
        client_id="test-wrapper-purpose-2",
        purpose="shared-purpose",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
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
    first = HTTPXClientWrapper(
        client_id="test-wrapper-purpose-final-1",
        purpose="shared-purpose-final",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    second = HTTPXClientWrapper(
        client_id="test-wrapper-purpose-final-2",
        purpose="shared-purpose-final",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        await first.request("GET", "https://example.com/first")
        await second.request("GET", "https://example.com/second")

    with patch("aibrix.metadata.core.httpx_client.logger.info") as mock_info:
        await first.stop()
        await second.stop()

    telemetry_calls = [
        call
        for call in mock_info.call_args_list
        if call.args and call.args[0] == "HTTPX client telemetry summary"
    ]
    assert len(telemetry_calls) == 1
    assert telemetry_calls[0].kwargs["purpose"] == "shared-purpose-final"
    assert telemetry_calls[0].kwargs["registered_clients"] == 0
    assert telemetry_calls[0].kwargs["window_started"] == 2
    assert telemetry_calls[0].kwargs["window_completed"] == 2


@pytest.mark.asyncio
async def test_httpx_wrapper_request_records_failure():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-failure",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(side_effect=httpx.ConnectError("boom")),
    ):
        with pytest.raises(httpx.ConnectError):
            await wrapper.request(
                "GET",
                "https://example.com/healthz",
                telemetry_call_site="tests.metadata.test_httpx_client.failure",
            )

    assert wrapper._telemetry_stats.started == 1
    assert wrapper._telemetry_stats.completed == 1
    assert wrapper._telemetry_stats.failed == 1
    assert wrapper._telemetry_stats.inflight == 0
    snapshot = wrapper._telemetry_stats.snapshot(reset_window=False)
    assert len(snapshot.call_sites) == 1
    assert (
        snapshot.call_sites[0].call_site == "tests.metadata.test_httpx_client.failure"
    )
    assert snapshot.call_sites[0].exception_types == {"ConnectError": 1}
    assert snapshot.exception_types == {"ConnectError": 1}

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_emits_debug_telemetry_by_call_site():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-debug",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        await wrapper.request(
            "GET",
            "https://example.com/healthz",
            telemetry_call_site="tests.metadata.test_httpx_client.debug",
        )

    with patch("aibrix.metadata.core.httpx_client.logger.debug") as mock_debug:
        wrapper._emit_telemetry(final=False, elapsed=1.0)

    mock_debug.assert_called_once()
    _, kwargs = mock_debug.call_args
    assert kwargs["client_id"] == "test-wrapper-debug"
    assert kwargs["purpose"] is None
    assert kwargs["call_site"] == "tests.metadata.test_httpx_client.debug"
    assert kwargs["failed"] == 0
    assert kwargs["exception_types"] == {}

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_context_manager_starts_and_stops():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-context",
        telemetry_interval_seconds=0,
    )

    async with wrapper as managed:
        assert managed.async_client is not None
        assert managed.is_closed is False

    assert wrapper.async_client is None
    assert wrapper.is_closed is True


@pytest.mark.asyncio
async def test_httpx_wrapper_cannot_be_reused_after_stop():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-stopped",
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        response = await wrapper.request("GET", "https://example.com/healthz")

    assert response.status_code == 200

    await wrapper.stop()

    with pytest.raises(RuntimeError, match="cannot be reused after stop"):
        await wrapper.request("GET", "https://example.com/healthz")

    with pytest.raises(RuntimeError, match="cannot be reused after stop"):
        _ = wrapper.headers


@pytest.mark.asyncio
async def test_httpx_wrapper_stop_uses_actual_elapsed_time_for_final_telemetry():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-final-elapsed",
        telemetry_enabled=True,
        telemetry_interval_seconds=60,
    )

    wrapper.start()
    assert wrapper.async_client is not None

    wrapper._telemetry_stats.record_start(
        call_site="tests.metadata.test_httpx_client.final_elapsed"
    )
    wrapper._telemetry_stats.record_completion(
        failed=False,
        latency_seconds=0.1,
        call_site="tests.metadata.test_httpx_client.final_elapsed",
    )
    wrapper._last_telemetry_emitted_at = 100.0

    with (
        patch.object(wrapper.async_client, "aclose", new=AsyncMock()),
        patch(
            "aibrix.metadata.core.httpx_client.asyncio.get_running_loop",
            return_value=MagicMock(time=MagicMock(return_value=100.25)),
        ),
        patch("aibrix.metadata.core.httpx_client.logger.info") as mock_info,
    ):
        await wrapper.stop()

    telemetry_call = next(
        call
        for call in mock_info.call_args_list
        if call.args and call.args[0] == "HTTPX client telemetry summary"
    )
    assert telemetry_call.kwargs["final"] is True
    assert telemetry_call.kwargs["purpose"] is None
    assert telemetry_call.kwargs["started_qps"] == 4.0
    assert telemetry_call.kwargs["completed_qps"] == 4.0
    assert telemetry_call.kwargs["failed_qps"] == 0.0
