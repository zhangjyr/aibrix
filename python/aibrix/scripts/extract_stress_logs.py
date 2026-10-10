#!/usr/bin/env python3

from __future__ import annotations

import argparse
import csv
import json
from pathlib import Path
from typing import Any

MDS_METRICS_CSV_FIELDS = [
    "log_time",
    "timestamp",
    "active_metric_backend",
    "metric_emit_count",
    "metric_emit_total_ms",
    "metric_emit_avg_us",
    "metric_emit_p95_us",
    "process_rss_mb",
    "gc_gen0_count",
    "gc_gen1_count",
    "gc_gen2_count",
]

HTTPX_TELEMETRY_CSV_FIELDS = [
    "log_time",
    "timestamp",
    "purpose",
    "client_id",
    "registered_clients",
    "started",
    "completed",
    "failed",
    "inflight",
    "max_inflight",
    "started_qps",
    "completed_qps",
    "failed_qps",
    "p50_latency_seconds",
    "p95_latency_seconds",
    "final",
    "is_closed",
    "exception_types",
]

DISPATCH_TELEMETRY_CSV_FIELDS = [
    "log_time",
    "timestamp",
    "job_id",
    "concurrency_limit",
    "started",
    "completed",
    "failed",
    "inflight",
    "max_inflight",
    "started_qps",
    "completed_qps",
    "failed_qps",
    "avg_latency_seconds",
    "p95_latency_seconds",
    "avg_queue_wait_seconds",
    "p95_queue_wait_seconds",
    "avg_qps_wait_seconds",
    "p95_qps_wait_seconds",
    "avg_send_latency_seconds",
    "p95_send_latency_seconds",
    "final",
]

JSON_PROFILER_CSV_FIELDS = [
    "log_time",
    "timestamp",
    "purpose",
    "started",
    "completed",
    "failed",
    "window_started",
    "window_completed",
    "window_failed",
    "started_qps",
    "completed_qps",
    "failed_qps",
    "p50_latency_seconds",
    "p95_latency_seconds",
    "final",
    "exception_types",
]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Extract structured CSV telemetry from MDS stress-test logs."
    )
    parser.add_argument(
        "artifact_root",
        type=Path,
        help="Stress artifact root, a scenario directory, or a single mds.log file.",
    )
    return parser.parse_args()


def write_mds_metrics_csv(log_path: Path, csv_path: Path) -> int:
    rows: list[dict[str, Any]] = []
    if log_path.exists():
        for line in log_path.read_text(encoding="utf-8", errors="replace").splitlines():
            json_start = line.find("{")
            if json_start < 0:
                continue
            try:
                payload = json.loads(line[json_start:])
            except json.JSONDecodeError:
                continue
            if payload.get("event") != "Metric emission overhead snapshot":
                continue
            gc_counts = payload.get("gc_counts", [])
            rows.append(
                {
                    "log_time": line[:23],
                    "timestamp": payload.get("timestamp", ""),
                    "active_metric_backend": payload.get("active_metric_backend", ""),
                    "metric_emit_count": payload.get("metric_emit_count", ""),
                    "metric_emit_total_ms": payload.get("metric_emit_total_ms", ""),
                    "metric_emit_avg_us": payload.get("metric_emit_avg_us", ""),
                    "metric_emit_p95_us": payload.get("metric_emit_p95_us", ""),
                    "process_rss_mb": payload.get("process_rss_mb", ""),
                    "gc_gen0_count": gc_counts[0] if len(gc_counts) > 0 else "",
                    "gc_gen1_count": gc_counts[1] if len(gc_counts) > 1 else "",
                    "gc_gen2_count": gc_counts[2] if len(gc_counts) > 2 else "",
                }
            )

    with csv_path.open("w", newline="", encoding="utf-8") as csv_file:
        writer = csv.DictWriter(csv_file, fieldnames=MDS_METRICS_CSV_FIELDS)
        writer.writeheader()
        writer.writerows(rows)
    return len(rows)


def _json_csv_value(value: Any) -> Any:
    if isinstance(value, (dict, list)):
        return json.dumps(value, sort_keys=True)
    if value is None:
        return ""
    return value


def write_log_event_csv(
    log_path: Path,
    csv_path: Path,
    *,
    event_name: str,
    fieldnames: list[str],
) -> int:
    rows: list[dict[str, Any]] = []
    if log_path.exists():
        for line in log_path.read_text(encoding="utf-8", errors="replace").splitlines():
            json_start = line.find("{")
            if json_start < 0:
                continue
            try:
                payload = json.loads(line[json_start:])
            except json.JSONDecodeError:
                continue
            if payload.get("event") != event_name:
                continue
            row = {"log_time": line[:23]}
            for field in fieldnames:
                if field == "log_time":
                    continue
                row[field] = _json_csv_value(payload.get(field, ""))
            rows.append(row)

    with csv_path.open("w", newline="", encoding="utf-8") as csv_file:
        writer = csv.DictWriter(csv_file, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(rows)
    return len(rows)


def write_httpx_telemetry_csv(log_path: Path, csv_path: Path) -> int:
    return write_log_event_csv(
        log_path,
        csv_path,
        event_name="HTTPX client telemetry summary",
        fieldnames=HTTPX_TELEMETRY_CSV_FIELDS,
    )


def write_dispatch_telemetry_csv(log_path: Path, csv_path: Path) -> int:
    return write_log_event_csv(
        log_path,
        csv_path,
        event_name="Batch dispatch telemetry",
        fieldnames=DISPATCH_TELEMETRY_CSV_FIELDS,
    )


def write_json_profiler_csv(log_path: Path, csv_path: Path) -> int:
    return write_log_event_csv(
        log_path,
        csv_path,
        event_name="JSON profiler telemetry summary",
        fieldnames=JSON_PROFILER_CSV_FIELDS,
    )


def extract_scenario_logs(scenario_dir: Path) -> dict[str, int]:
    log_path = scenario_dir / "mds.log"
    counts = {
        "mds.csv": write_mds_metrics_csv(log_path, scenario_dir / "mds.csv"),
        "httpx.csv": write_httpx_telemetry_csv(log_path, scenario_dir / "httpx.csv"),
        "dispatch.csv": write_dispatch_telemetry_csv(
            log_path, scenario_dir / "dispatch.csv"
        ),
        "json_profiler.csv": write_json_profiler_csv(
            log_path, scenario_dir / "json_profiler.csv"
        ),
    }
    return counts


def discover_scenario_dirs(path: Path) -> list[Path]:
    if path.is_file():
        return [path.parent] if path.name == "mds.log" else []
    if (path / "mds.log").exists():
        return [path]
    return sorted({log_path.parent for log_path in path.rglob("mds.log")})


def main() -> None:
    args = parse_args()
    scenario_dirs = discover_scenario_dirs(args.artifact_root.resolve())
    if not scenario_dirs:
        raise SystemExit(f"No mds.log files found under {args.artifact_root}")
    for scenario_dir in scenario_dirs:
        counts = extract_scenario_logs(scenario_dir)
        print(scenario_dir)
        for filename, row_count in counts.items():
            print(f"  {filename}: {row_count}")


if __name__ == "__main__":
    main()
