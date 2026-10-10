import json
from unittest.mock import patch

import pytest

from aibrix.metadata.core.json_profiler import JsonProfiler


def test_json_profiler_records_success_and_failure_by_shared_purpose():
    first = JsonProfiler(
        purpose="test-json-shared",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    second = JsonProfiler(
        purpose="test-json-shared",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    assert first.profile("json.loads", json.loads, '{"ok": true}') == {"ok": True}
    with pytest.raises(json.JSONDecodeError):
        second.profile("json.loads", json.loads, "{")

    assert first._stats is second._stats
    snapshot = first.snapshot(reset_window=False)
    assert snapshot.started == 2
    assert snapshot.completed == 2
    assert snapshot.failed == 1
    assert snapshot.operations[0].operation == "json.loads"
    assert snapshot.operations[0].started == 2
    assert snapshot.operations[0].failed == 1


def test_json_profiler_emits_summary_with_purpose():
    profiler = JsonProfiler(
        purpose="test-json-log",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    profiler.profile("json.loads", json.loads, '{"ok": true}')

    with patch("aibrix.metadata.core.json_profiler.logger.info") as mock_info:
        profiler.emit_telemetry(final=False, elapsed=1.0)

    mock_info.assert_called_once()
    _, kwargs = mock_info.call_args
    assert kwargs["purpose"] == "test-json-log"
    assert kwargs["started_qps"] == 1.0
    assert kwargs["completed_qps"] == 1.0
    assert kwargs["failed_qps"] == 0.0
    assert kwargs["p50_latency_seconds"] is not None


def test_json_profiler_shares_periodic_emit_clock_by_purpose():
    first = JsonProfiler(
        purpose="test-json-periodic-shared",
        telemetry_enabled=True,
        telemetry_interval_seconds=1.0,
    )
    second = JsonProfiler(
        purpose="test-json-periodic-shared",
        telemetry_enabled=True,
        telemetry_interval_seconds=1.0,
    )
    first._purpose_state.last_telemetry_emitted_at = 0.0

    with (
        patch(
            "aibrix.metadata.core.json_profiler.time.perf_counter",
            side_effect=[2.0, 2.1, 2.1, 2.2, 2.2, 2.2],
        ),
        patch("aibrix.metadata.core.json_profiler.logger.info") as mock_info,
    ):
        first.profile("json.loads", json.loads, '{"first": true}')
        second.profile("json.loads", json.loads, '{"second": true}')

    telemetry_calls = [
        call
        for call in mock_info.call_args_list
        if call.args and call.args[0] == "JSON profiler telemetry summary"
    ]
    assert len(telemetry_calls) == 1
    assert telemetry_calls[0].kwargs["purpose"] == "test-json-periodic-shared"
    assert telemetry_calls[0].kwargs["window_completed"] == 1
    assert second.snapshot(reset_window=False).window_completed == 1
