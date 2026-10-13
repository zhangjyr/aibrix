from __future__ import annotations

import csv
import importlib.util
import json
from pathlib import Path

SCRIPT_PATH = (
    Path(__file__).resolve().parents[2]
    / "scripts"
    / "stress_test"
    / "extract_stress_logs.py"
)


def load_extract_stress_logs():
    spec = importlib.util.spec_from_file_location("extract_stress_logs", SCRIPT_PATH)
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def write_log_line(log_path: Path, payload: dict[str, object]) -> None:
    log_path.write_text(
        f"2026-10-12 12:00:00,000 - file.py:1 - func - INFO - {json.dumps(payload)}\n",
        encoding="utf-8",
    )


def read_single_csv_row(csv_path: Path) -> dict[str, str]:
    with csv_path.open(newline="", encoding="utf-8") as csv_file:
        rows = list(csv.DictReader(csv_file))
    assert len(rows) == 1
    return rows[0]


def test_batch_finalize_csv_extracts_summary_event(tmp_path: Path):
    module = load_extract_stress_logs()
    log_path = tmp_path / "mds.log"
    csv_path = tmp_path / "batch_finalize.csv"
    write_log_line(
        log_path,
        {
            "event": "Batch finalize telemetry summary",
            "timestamp": "2026-10-12 19:00:00 UTC",
            "job_id": "job-1",
            "failed_finalize": "false",
            "job_total_duration_ms": 20.0,
            "stage_duration_ms": {"list_metastore_keys": 12.5},
            "stage_fields": {"assemble_parts": {"output_parts": 2}},
            "stage_items": {"list_metastore_keys": {"keys": 10}},
        },
    )

    assert module.write_batch_finalize_csv(log_path, csv_path) == 1

    row = read_single_csv_row(csv_path)
    assert row["job_id"] == "job-1"
    assert row["job_total_duration_ms"] == "20.0"
    assert json.loads(row["stage_duration_ms"]) == {"list_metastore_keys": 12.5}
    assert json.loads(row["stage_fields"]) == {"assemble_parts": {"output_parts": 2}}
    assert json.loads(row["stage_items"]) == {"list_metastore_keys": {"keys": 10}}


def test_batch_finalize_csv_falls_back_to_job_latency_event(tmp_path: Path):
    module = load_extract_stress_logs()
    log_path = tmp_path / "mds.log"
    csv_path = tmp_path / "batch_finalize.csv"
    write_log_line(
        log_path,
        {
            "event": "Batch job latency telemetry",
            "timestamp": "2026-10-12 19:00:00 UTC",
            "job_id": "job-2",
            "finalize_type": "completed",
            "execution_duration_ms": 352332.749,
            "phase_duration_ms": {
                "scheduling": 535.311,
                "validation": 27.61,
                "task_execution": 312511.539,
                "finalization": 39821.094,
            },
        },
    )

    assert module.write_batch_finalize_csv(log_path, csv_path) == 1

    row = read_single_csv_row(csv_path)
    assert row["job_id"] == "job-2"
    assert row["failed_finalize"] == "false"
    assert row["job_total_duration_ms"] == "39821.094"
    assert json.loads(row["stage_duration_ms"]) == {"finalization": 39821.094}
    assert json.loads(row["stage_fields"]) == {
        "job_latency": {"execution_duration_ms": 352332.749}
    }
