import argparse
import csv
import json
import sys
from pathlib import Path

import pytest

SCRIPT_DIR = Path(__file__).resolve().parents[2] / "scripts" / "stress_test"
sys.path.insert(0, str(SCRIPT_DIR))

from extract_stress_logs import (  # noqa: E402
    extract_scenario_logs,
    write_batch_finalize_csv,
    write_dispatch_telemetry_csv,
    write_httpx_telemetry_csv,
    write_json_profiler_csv,
    write_mds_metrics_csv,
)
from stress_test import (  # noqa: E402
    _load_default_config,
    apply_ramp_latency_source,
    batch_job_latency_run_summary,
    build_go_command,
    build_mds_command,
    default_ramp_total_jobs,
    is_ramp_latency_stable,
    is_ramp_level_stable,
    next_ramp_concurrency,
    ramp_latency_increase_ratio,
    ramp_level_stop_reason,
    ramp_level_total_jobs,
    ramp_max_concurrency,
    ramp_record,
    ramp_total_jobs_limit,
    scenario_env,
    should_stop_ramp,
)


def test_load_default_config_accepts_python_config(tmp_path: Path, monkeypatch):
    parser = argparse.ArgumentParser()
    parser.add_argument("--ramp-jobs-per-concurrency", type=int, default=4)
    parser.add_argument("--total-jobs", type=int, default=40)
    parser.add_argument("--runtime-fixed-latency-ms", default="100ms")
    config_path = tmp_path / "config.py"
    config_path.write_text(
        "\n".join(
            [
                "CONFIG = {",
                "    # Inline comments are valid in Python configs.",
                '    "ramp-jobs-per-concurrency": 1,',
                '    "total-jobs": 0,',
                '    "runtime-fixed-latency-ms": "30s",',
                "}",
            ]
        ),
        encoding="utf-8",
    )
    monkeypatch.chdir(tmp_path)

    _load_default_config(parser, [])
    args = parser.parse_args([])

    assert args.ramp_jobs_per_concurrency == 1
    assert args.total_jobs == 0
    assert args.runtime_fixed_latency_ms == "30s"


def test_scenario_env_sets_scheduler_job_pool_size_to_100(tmp_path: Path):
    args = argparse.Namespace(batch_runtime_http_client="httpx_client")
    env = scenario_env({}, "D", tmp_path, args)

    assert env["AIBRIX_BATCH_JOB_POOL_SIZE"] == "100"
    assert env["AIBRIX_BATCH_RUNTIME_HTTP_CLIENT"] == "httpx_client"


def test_build_mds_command_preserves_runtime_capacity():
    command = build_mds_command(
        sys.executable,
        host="127.0.0.1",
        port=8090,
        runtime_base_url="http://127.0.0.1:18080",
        runtime_capacity=120,
        runtime_replicas=1,
        runtime_timeout_seconds=30.0,
    )

    capacity_flag_index = command.index("--batch-runtime-capacity")
    assert command[capacity_flag_index + 1] == "120"


def test_build_mds_command_uses_discovery_runtime_source():
    command = build_mds_command(
        sys.executable,
        host="127.0.0.1",
        port=8090,
        runtime_base_url="http://127.0.0.1:18080",
        runtime_capacity=16,
        runtime_replicas=3,
        runtime_timeout_seconds=45.0,
    )

    source_flag_index = command.index("--batch-runtime-source")
    assert command[source_flag_index + 1] == "discovery"
    replicas_flag_index = command.index("--batch-runtime-replicas")
    assert command[replicas_flag_index + 1] == "3"
    timeout_flag_index = command.index("--batch-runtime-timeout-seconds")
    assert command[timeout_flag_index + 1] == "45.0"


def test_build_go_command_accepts_runtime_fixed_latency():
    command = build_go_command(
        scenario="A",
        mds_base_url="http://127.0.0.1:8090",
        runtime_port=18080,
        runtime_fixed_latency="30s",
        duration="5m",
        poll_timeout="15m",
        concurrency=40,
        total_jobs=50000,
        requests_per_job=1000,
        job_adaptive_concurrency=False,
        output_dir=Path("/tmp/out"),
        scrape_metrics_url=None,
    )

    assert "--runtime-fixed-latency-ms=30s" in command
    assert "./python/aibrix/scripts/stress_test/driver_server" in command
    assert "--poll-timeout=15m" in command


def test_default_ramp_total_jobs_scales_with_concurrency():
    assert default_ramp_total_jobs(concurrency=1) == 4
    assert default_ramp_total_jobs(concurrency=8) == 32
    assert default_ramp_total_jobs(concurrency=8, jobs_per_concurrency=8) == 64


def test_ramp_total_jobs_zero_disables_cap():
    args = argparse.Namespace(total_jobs=0)

    assert ramp_total_jobs_limit(args) is None


def test_ramp_level_total_jobs_uses_factor_when_uncapped():
    args = argparse.Namespace(
        total_jobs=0,
        ramp_jobs_per_concurrency=1,
    )

    assert ramp_level_total_jobs(8, args) == 8


def test_ramp_level_total_jobs_caps_positive_total_jobs():
    args = argparse.Namespace(
        total_jobs=4,
        ramp_jobs_per_concurrency=2,
    )

    assert ramp_level_total_jobs(8, args) == 4
    assert ramp_max_concurrency(args) == 4


def test_next_ramp_concurrency_doubles_without_total_jobs_bound_by_default():
    assert (
        next_ramp_concurrency(
            current=1,
            growth_factor=2.0,
            max_concurrency=None,
        )
        == 2
    )
    assert (
        next_ramp_concurrency(
            current=64,
            growth_factor=2.0,
            max_concurrency=None,
        )
        == 128
    )


def test_next_ramp_concurrency_respects_explicit_max_concurrency():
    assert (
        next_ramp_concurrency(
            current=32,
            growth_factor=2.0,
            max_concurrency=40,
        )
        == 40
    )
    assert (
        next_ramp_concurrency(
            current=40,
            growth_factor=2.0,
            max_concurrency=40,
        )
        is None
    )


def test_ramp_stability_uses_mean_latency_relative_delta():
    assert is_ramp_latency_stable([1000.0, 1080.0], threshold=0.10)
    assert not is_ramp_latency_stable([1000.0, 1200.0], threshold=0.10)
    assert not is_ramp_latency_stable([1000.0], threshold=0.10)


def test_ramp_level_accepts_single_run_when_min_runs_is_one():
    assert is_ramp_level_stable([1000.0], min_runs=1, threshold=0.10)
    assert not is_ramp_level_stable([], min_runs=1, threshold=0.10)
    assert not is_ramp_level_stable([1000.0], min_runs=2, threshold=0.10)


def test_ramp_stop_uses_50_percent_mean_latency_increase():
    assert not should_stop_ramp(
        baseline_latency_mean_ms=1000.0,
        current_latency_mean_ms=1499.0,
        threshold=0.50,
    )
    assert should_stop_ramp(
        baseline_latency_mean_ms=1000.0,
        current_latency_mean_ms=1501.0,
        threshold=0.50,
    )


def test_ramp_level_stop_reason_reports_unstable_threshold_exceeded():
    assert (
        ramp_level_stop_reason(
            baseline_latency_mean_ms=1000.0,
            current_latency_mean_ms=1700.0,
            stable=False,
            threshold=0.50,
        )
        == "latency_threshold_exceeded_unstable"
    )
    assert (
        ramp_level_stop_reason(
            baseline_latency_mean_ms=1000.0,
            current_latency_mean_ms=1400.0,
            stable=False,
            threshold=0.50,
        )
        == "latency_not_stable"
    )


def test_ramp_latency_increase_ratio_reports_increase_not_multiplier():
    assert (
        ramp_latency_increase_ratio(
            baseline_latency_mean_ms=1000.0,
            current_latency_mean_ms=1500.0,
        )
        == 0.5
    )


def test_ramp_record_sets_baseline_fields_on_baseline_run():
    record = ramp_record(
        scenario="A",
        concurrency=1,
        run_index=2,
        requests_per_job=1000,
        summary={
            "total_jobs": 4,
            "total_requests": 4000,
            "success_count": 4,
            "failure_count": 0,
            "latency_mean_ms": 2000.0,
        },
        baseline_latency_mean_ms=2000.0,
        stable=True,
        stop_reason="",
        artifact_dir=Path("/tmp/ramp/A"),
    )

    assert record["baseline_latency_mean_ms"] == 2000.0
    assert record["latency_increase_ratio"] == 0.0


def test_batch_job_latency_summary_reads_execution_phase_per_job(tmp_path: Path):
    scenario_dir = tmp_path
    (scenario_dir / "results.json").write_text(
        json.dumps(
            [
                {"job_id": "job-1"},
                {"job_id": "job-2"},
                {"job_id": "job-3"},
            ]
        ),
        encoding="utf-8",
    )
    with (scenario_dir / "batch_job_latency.csv").open(
        "w", newline="", encoding="utf-8"
    ) as f:
        writer = csv.DictWriter(
            f,
            fieldnames=[
                "job_id",
                "phase_duration_ms",
            ],
        )
        writer.writeheader()
        writer.writerow(
            {"job_id": "job-1", "phase_duration_ms": '{"task_execution": 100}'}
        )
        writer.writerow(
            {"job_id": "job-1", "phase_duration_ms": '{"task_execution": 50}'}
        )
        writer.writerow(
            {"job_id": "job-2", "phase_duration_ms": '{"task_execution": 250}'}
        )
        writer.writerow(
            {"job_id": "job-3", "phase_duration_ms": '{"runtime_ready": 30}'}
        )
        writer.writerow(
            {"job_id": "job-out-of-run", "phase_duration_ms": '{"task_execution": 1}'}
        )

    summary = batch_job_latency_run_summary(scenario_dir)

    assert summary == {
        "execution_phase": "task_execution",
        "execution_phase_mean_ms": 200.0,
        "execution_phase_p50_ms": 150.0,
        "execution_phase_p95_ms": 150.0,
        "execution_phase_sample_count": 2,
        "execution_phase_missing_count": 1,
    }


def test_batch_job_latency_summary_requires_results_job_ids(tmp_path: Path):
    (tmp_path / "batch_job_latency.csv").write_text(
        'job_id,phase_duration_ms\njob-1,"{""task_execution"": 1}"\n',
        encoding="utf-8",
    )

    with pytest.raises(RuntimeError, match="No job IDs found"):
        batch_job_latency_run_summary(tmp_path)


def test_batch_job_latency_summary_fails_without_valid_samples(tmp_path: Path):
    (tmp_path / "results.json").write_text(
        json.dumps([{"job_id": "job-1"}]), encoding="utf-8"
    )
    (tmp_path / "batch_job_latency.csv").write_text(
        'job_id,phase_duration_ms\njob-1,"{""runtime_ready"": 1}"\n',
        encoding="utf-8",
    )

    with pytest.raises(RuntimeError, match="No valid execution-phase samples"):
        batch_job_latency_run_summary(tmp_path)


def test_apply_ramp_latency_source_uses_execution_phase(tmp_path: Path):
    (tmp_path / "results.json").write_text(
        json.dumps([{"job_id": "job-1"}]), encoding="utf-8"
    )
    (tmp_path / "batch_job_latency.csv").write_text(
        'job_id,phase_duration_ms\njob-1,"{""task_execution"": 250}"\n',
        encoding="utf-8",
    )

    summary = apply_ramp_latency_source(
        {"latency_mean_ms": 1000.0},
        scenario_dir=tmp_path,
        latency_source="execution-phase",
    )

    assert summary["threshold_latency_source"] == "execution-phase"
    assert summary["threshold_latency_mean_ms"] == 250.0
    assert summary["execution_phase_sample_count"] == 1


def test_apply_ramp_latency_source_keeps_whole_job(tmp_path: Path):
    summary = apply_ramp_latency_source(
        {"latency_mean_ms": 1000.0},
        scenario_dir=tmp_path,
        latency_source="whole-job",
    )

    assert summary["threshold_latency_source"] == "whole-job"
    assert summary["threshold_latency_mean_ms"] == 1000.0


def test_write_mds_metrics_csv_extracts_metric_emission_snapshots(tmp_path: Path):
    log_path = tmp_path / "mds.log"
    csv_path = tmp_path / "mds.csv"
    log_path.write_text(
        "\n".join(
            [
                "plain startup line",
                (
                    "2026-08-27 14:00:00,000 - sink.py:134 - log_snapshot - INFO - "
                    '{"metric_emit_count": 10, "metric_emit_total_ms": 1.5, '
                    '"metric_emit_avg_us": 150.0, "metric_emit_p95_us": 200.0, '
                    '"process_rss_mb": 128.5, "gc_counts": [1, 2, 3], '
                    '"active_metric_backend": "PrometheusSink", '
                    '"event": "Metric emission overhead snapshot", '
                    '"logger": "aibrix.metadata.core.metrics.sink", "level": "info", '
                    '"timestamp": "2026-08-27 21:00:00 UTC"}'
                ),
            ]
        ),
        encoding="utf-8",
    )

    rows = write_mds_metrics_csv(log_path, csv_path)

    assert rows == 1
    with csv_path.open(newline="", encoding="utf-8") as csv_file:
        records = list(csv.DictReader(csv_file))

    assert records == [
        {
            "log_time": "2026-08-27 14:00:00,000",
            "timestamp": "2026-08-27 21:00:00 UTC",
            "active_metric_backend": "PrometheusSink",
            "metric_emit_count": "10",
            "metric_emit_total_ms": "1.5",
            "metric_emit_avg_us": "150.0",
            "metric_emit_p95_us": "200.0",
            "process_rss_mb": "128.5",
            "gc_gen0_count": "1",
            "gc_gen1_count": "2",
            "gc_gen2_count": "3",
        }
    ]


def test_write_telemetry_csv_extracts_httpx_and_dispatch_events(tmp_path: Path):
    log_path = tmp_path / "mds.log"
    httpx_csv_path = tmp_path / "httpx.csv"
    dispatch_csv_path = tmp_path / "dispatch.csv"
    log_path.write_text(
        "\n".join(
            [
                (
                    "2026-10-09 22:30:37,000 - httpx_client.py:1 - INFO - "
                    '{"event": "HTTPX client telemetry summary", '
                    '"timestamp": "2026-10-09 22:30:37 UTC", '
                    '"purpose": "batch_runtime_http", '
                    '"client_id": "http-channel-a", "registered_clients": 10, '
                    '"started": 100, "completed": 90, "failed": 1, '
                    '"inflight": 9, "max_inflight": 20, '
                    '"started_qps": 10.0, "completed_qps": 9.0, '
                    '"failed_qps": 0.1, "p50_latency_seconds": 0.2, '
                    '"p95_latency_seconds": 0.5, "final": false, '
                    '"is_closed": false, "exception_types": {"PoolTimeout": 1}}'
                ),
                (
                    "2026-10-09 22:30:38,000 - base.py:1 - INFO - "
                    '{"event": "Batch dispatch telemetry", '
                    '"timestamp": "2026-10-09 22:30:38 UTC", '
                    '"job_id": "job-a", "concurrency_limit": 100, '
                    '"started": 1000, "completed": 1000, "failed": 0, '
                    '"inflight": 0, "max_inflight": 100, '
                    '"started_qps": 200.0, "completed_qps": 200.0, '
                    '"failed_qps": 0.0, "avg_latency_seconds": 2.1, '
                    '"p95_latency_seconds": 3.6, '
                    '"avg_queue_wait_seconds": 0.4, '
                    '"p95_queue_wait_seconds": 0.8, '
                    '"avg_qps_wait_seconds": 0.1, '
                    '"p95_qps_wait_seconds": 0.2, '
                    '"avg_send_latency_seconds": 1.7, '
                    '"p95_send_latency_seconds": 3.0, '
                    '"final": true}'
                ),
            ]
        ),
        encoding="utf-8",
    )

    assert write_httpx_telemetry_csv(log_path, httpx_csv_path) == 1
    assert write_dispatch_telemetry_csv(log_path, dispatch_csv_path) == 1

    with httpx_csv_path.open(newline="", encoding="utf-8") as csv_file:
        httpx_rows = list(csv.DictReader(csv_file))
    with dispatch_csv_path.open(newline="", encoding="utf-8") as csv_file:
        dispatch_rows = list(csv.DictReader(csv_file))

    assert httpx_rows[0]["purpose"] == "batch_runtime_http"
    assert httpx_rows[0]["registered_clients"] == "10"
    assert httpx_rows[0]["exception_types"] == '{"PoolTimeout": 1}'
    assert dispatch_rows[0]["job_id"] == "job-a"
    assert dispatch_rows[0]["concurrency_limit"] == "100"
    assert dispatch_rows[0]["avg_latency_seconds"] == "2.1"
    assert dispatch_rows[0]["avg_queue_wait_seconds"] == "0.4"
    assert dispatch_rows[0]["p95_queue_wait_seconds"] == "0.8"
    assert dispatch_rows[0]["avg_qps_wait_seconds"] == "0.1"
    assert dispatch_rows[0]["p95_qps_wait_seconds"] == "0.2"
    assert dispatch_rows[0]["avg_send_latency_seconds"] == "1.7"
    assert dispatch_rows[0]["p95_send_latency_seconds"] == "3.0"


def test_write_json_profiler_csv_writes_header_when_no_events(tmp_path: Path):
    log_path = tmp_path / "mds.log"
    csv_path = tmp_path / "json_profiler.csv"
    log_path.write_text("plain startup line\n", encoding="utf-8")

    assert write_json_profiler_csv(log_path, csv_path) == 0

    assert csv_path.read_text(encoding="utf-8").startswith("log_time,timestamp,purpose")


def test_write_batch_finalize_csv_extracts_phase_summary(tmp_path: Path):
    log_path = tmp_path / "mds.log"
    csv_path = tmp_path / "batch_finalize.csv"
    log_path.write_text(
        (
            "2026-10-11 19:48:23,161 - log.py:89 - INFO - "
            '{"event": "Batch finalize telemetry summary", '
            '"timestamp": "2026-10-12 02:48:23 UTC", '
            '"job_id": "job-a", "failed_finalize": "false", '
            '"job_total_duration_ms": 18.4, '
            '"stage_duration_ms": {"list_metastore_keys": 5.0, '
            '"complete_output_upload": 4.0, "total": 18.0}, '
            '"stage_fields": {"list_metastore_keys": {"key_count": 1.0}}, '
            '"stage_items": {"list_metastore_keys": {"keys": 1.0}}, '
            '"metric_name": "batch_finalize_job_summary"}\n'
        ),
        encoding="utf-8",
    )

    assert write_batch_finalize_csv(log_path, csv_path) == 1

    with csv_path.open(newline="", encoding="utf-8") as csv_file:
        records = list(csv.DictReader(csv_file))

    assert records[0]["job_id"] == "job-a"
    assert records[0]["failed_finalize"] == "false"
    assert records[0]["job_total_duration_ms"] == "18.4"
    assert records[0]["stage_duration_ms"] == (
        '{"complete_output_upload": 4.0, "list_metastore_keys": 5.0, "total": 18.0}'
    )
    assert records[0]["stage_fields"] == '{"list_metastore_keys": {"key_count": 1.0}}'
    assert records[0]["stage_items"] == '{"list_metastore_keys": {"keys": 1.0}}'


def test_extracts_batch_job_latency_telemetry(tmp_path: Path):
    scenario_dir = tmp_path / "scenario"
    scenario_dir.mkdir()
    log_payload = {
        "event": "Batch job latency telemetry",
        "job_id": "job-1",
        "console_job_id": "console-1",
        "endpoint": "/v1/chat/completions",
        "completion_window": "86400",
        "finalize_type": "completed",
        "error_code": "none",
        "execution_duration_ms": 3500.0,
        "phase_duration_ms": {"task_execution": 2500.0},
        "metric_name": "metadata.batch.api.job.execution_time.ms",
        "timestamp": "2026-10-11 00:00:03 UTC",
    }
    (scenario_dir / "mds.log").write_text(
        "2026-10-11 00:00:03,000 - log.py:89 - _log_metric - INFO - "
        + json.dumps(log_payload)
        + "\n",
        encoding="utf-8",
    )

    counts = extract_scenario_logs(scenario_dir)

    assert counts["batch_job_latency.csv"] == 1
    rows = list(csv.DictReader((scenario_dir / "batch_job_latency.csv").open()))
    assert rows == [
        {
            "log_time": "2026-10-11 00:00:03,000",
            "timestamp": "2026-10-11 00:00:03 UTC",
            "job_id": "job-1",
            "console_job_id": "console-1",
            "endpoint": "/v1/chat/completions",
            "completion_window": "86400",
            "finalize_type": "completed",
            "error_code": "none",
            "execution_duration_ms": "3500.0",
            "phase_duration_ms": '{"task_execution": 2500.0}',
        }
    ]


def test_extract_scenario_logs_writes_batch_finalize_csv(tmp_path: Path):
    log_path = tmp_path / "mds.log"
    log_path.write_text(
        (
            "2026-10-11 19:48:23,161 - log.py:89 - INFO - "
            '{"event": "Batch finalize telemetry summary", '
            '"timestamp": "2026-10-12 02:48:23 UTC", '
            '"job_id": "job-a", "failed_finalize": "false", '
            '"job_total_duration_ms": 18.4, '
            '"stage_duration_ms": {"total": 18.0}, '
            '"stage_fields": {}, "stage_items": {}, '
            '"metric_name": "batch_finalize_job_summary"}\n'
        ),
        encoding="utf-8",
    )

    counts = extract_scenario_logs(tmp_path)

    assert counts["batch_finalize.csv"] == 1
    assert (tmp_path / "batch_finalize.csv").exists()
