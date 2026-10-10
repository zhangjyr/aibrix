#!/usr/bin/env python3

from __future__ import annotations

import argparse
import csv
import json
import math
import os
import signal
import socket
import subprocess
import sys
import time
from pathlib import Path
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.request import urlopen

from extract_stress_logs import extract_scenario_logs

RAMP_SUMMARY_CSV_FIELDS = [
    "scenario",
    "concurrency",
    "run_index",
    "total_jobs",
    "requests_per_job",
    "total_requests",
    "success_count",
    "failure_count",
    "latency_mean_ms",
    "baseline_latency_mean_ms",
    "latency_increase_ratio",
    "stable",
    "stop_reason",
    "artifact_dir",
]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run local Prometheus sink stress scenarios against MDS"
    )
    parser.add_argument(
        "--scenarios",
        nargs="+",
        default=["A", "C", "D"],
        help="Scenario sequence to execute",
    )
    parser.add_argument("--mds-host", default="127.0.0.1")
    parser.add_argument("--mds-port", type=int, default=8090)
    parser.add_argument("--runtime-port", type=int, default=18080)
    parser.add_argument("--duration", default="20s")
    parser.add_argument("--concurrency", type=int, default=4)
    parser.add_argument("--total-jobs", type=int, default=40)
    parser.add_argument("--requests-per-job", type=int, default=1)
    parser.add_argument(
        "--runtime-fixed-latency-ms",
        default="100ms",
        help="Fixed latency for the local fake runtime, forwarded to the Go driver.",
    )
    parser.add_argument(
        "--batch-runtime-capacity",
        type=int,
        default=100,
        help="MDS static runtime capacity for local batch execution, capped at 100.",
    )
    parser.add_argument(
        "--batch-runtime-replicas",
        type=int,
        default=1,
        help=(
            "Number of local debug endpoints published through DiscoveryEndpointSource."
        ),
    )
    parser.add_argument(
        "--ramp-concurrency",
        action="store_true",
        help=(
            "Run an adaptive stress search that doubles job concurrency until "
            "mean job latency increases beyond the threshold."
        ),
    )
    parser.add_argument("--ramp-start-concurrency", type=int, default=1)
    parser.add_argument("--ramp-max-concurrency", type=int, default=None)
    parser.add_argument("--ramp-growth-factor", type=float, default=2.0)
    parser.add_argument(
        "--ramp-threshold",
        type=float,
        default=0.50,
        help="Stop when stable mean latency exceeds baseline by this ratio.",
    )
    parser.add_argument(
        "--ramp-stability-threshold",
        type=float,
        default=0.10,
        help="A level is stable when the last two mean latencies differ by this ratio.",
    )
    parser.add_argument("--ramp-min-runs-per-level", type=int, default=2)
    parser.add_argument("--ramp-max-runs-per-level", type=int, default=3)
    parser.add_argument(
        "--ramp-jobs-per-concurrency",
        type=int,
        default=4,
        help=(
            "Jobs submitted per concurrency slot in each ramp run. "
            "Ramp total jobs = concurrency * this value."
        ),
    )
    parser.add_argument(
        "--ramp-requests-per-job",
        type=int,
        default=1000,
        help="Requests per job used by --ramp-concurrency.",
    )
    parser.add_argument(
        "--ramp-batch-runtime-replicas",
        type=int,
        default=10,
        help="Runtime replicas per job used by --ramp-concurrency.",
    )
    parser.add_argument(
        "--b-total-jobs",
        type=int,
        default=50,
        help="Low-cardinality job count for scenario B.",
    )
    parser.add_argument(
        "--b-requests-per-job",
        type=int,
        default=1000,
        help="High per-job request count for scenario B.",
    )
    parser.add_argument(
        "--artifacts-dir",
        default=str(
            Path(__file__).resolve().parents[1] / "artifacts" / "prometheus_sink_stress"
        ),
    )
    return parser.parse_args()


def scenario_env(
    base_env: dict[str, str], scenario: str, scenario_dir: Path
) -> dict[str, str]:
    env = dict(base_env)
    env.setdefault("SECRET_KEY", "stress-test-secret")
    env.setdefault("STORAGE_TYPE", "local")
    env.setdefault("METASTORE_TYPE", "local")
    env["STORAGE_LOCAL_PATH"] = str(scenario_dir / "storage")
    env["AIBRIX_BATCH_JOB_POOL_SIZE"] = "100"
    env["METRICS_EMIT_OBSERVATION_ENABLED"] = "1"
    env["METRICS_EMIT_OBSERVATION_INTERVAL_SECONDS"] = "5"
    env["METRICS_PROMETHEUS_ENABLED"] = "1" if scenario in {"B", "C", "D"} else "0"
    return env


def wait_for_http(url: str, timeout_seconds: float = 20.0) -> None:
    deadline = time.time() + timeout_seconds
    while time.time() < deadline:
        try:
            with urlopen(url, timeout=2.0) as response:  # noqa: S310
                if response.status == 200:
                    return
        except (HTTPError, URLError):
            time.sleep(0.5)
    raise RuntimeError(f"Timed out waiting for {url}")


def stop_process(process: subprocess.Popen[Any]) -> None:
    if process.poll() is not None:
        return
    process.send_signal(signal.SIGINT)
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def reserve_local_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def default_ramp_total_jobs(concurrency: int, jobs_per_concurrency: int = 4) -> int:
    return max(int(concurrency), 1) * max(int(jobs_per_concurrency), 1)


def next_ramp_concurrency(
    *,
    current: int,
    growth_factor: float,
    max_concurrency: int | None,
) -> int | None:
    current = max(int(current), 1)
    candidate = max(current + 1, int(math.ceil(current * max(growth_factor, 1.0))))
    if max_concurrency is None:
        return candidate

    limit = max(int(max_concurrency), 1)
    if current >= limit:
        return None
    return min(candidate, limit)


def is_ramp_latency_stable(values: list[float], *, threshold: float) -> bool:
    if len(values) < 2:
        return False
    previous, current = values[-2], values[-1]
    if previous <= 0:
        return current <= 0
    return abs(current - previous) / previous <= threshold


def is_ramp_level_stable(
    values: list[float], *, min_runs: int, threshold: float
) -> bool:
    min_runs = max(int(min_runs), 1)
    if len(values) < min_runs:
        return False
    if min_runs == 1:
        return True
    return is_ramp_latency_stable(values, threshold=threshold)


def should_stop_ramp(
    *,
    baseline_latency_mean_ms: float,
    current_latency_mean_ms: float,
    threshold: float,
) -> bool:
    if baseline_latency_mean_ms <= 0:
        return False
    return current_latency_mean_ms > baseline_latency_mean_ms * (1.0 + threshold)


def ramp_level_stop_reason(
    *,
    baseline_latency_mean_ms: float | None,
    current_latency_mean_ms: float,
    stable: bool,
    threshold: float,
) -> str:
    if stable:
        return ""
    if baseline_latency_mean_ms is not None and should_stop_ramp(
        baseline_latency_mean_ms=baseline_latency_mean_ms,
        current_latency_mean_ms=current_latency_mean_ms,
        threshold=threshold,
    ):
        return "latency_threshold_exceeded_unstable"
    return "latency_not_stable"


def ramp_latency_increase_ratio(
    *, baseline_latency_mean_ms: float, current_latency_mean_ms: float
) -> float:
    if baseline_latency_mean_ms <= 0:
        return 0.0
    return (current_latency_mean_ms - baseline_latency_mean_ms) / baseline_latency_mean_ms


def ramp_record(
    *,
    scenario: str,
    concurrency: int,
    run_index: int,
    requests_per_job: int,
    summary: dict[str, Any],
    baseline_latency_mean_ms: float | None,
    stable: bool,
    stop_reason: str,
    artifact_dir: Path,
) -> dict[str, Any]:
    latency_mean_ms = float(summary.get("latency_mean_ms", 0.0))
    increase_ratio = (
        ramp_latency_increase_ratio(
            baseline_latency_mean_ms=baseline_latency_mean_ms,
            current_latency_mean_ms=latency_mean_ms,
        )
        if baseline_latency_mean_ms is not None
        else 0.0
    )
    return {
        "scenario": scenario,
        "concurrency": concurrency,
        "run_index": run_index,
        "total_jobs": summary.get("total_jobs", 0),
        "requests_per_job": requests_per_job,
        "total_requests": summary.get("total_requests", 0),
        "success_count": summary.get("success_count", 0),
        "failure_count": summary.get("failure_count", 0),
        "latency_mean_ms": latency_mean_ms,
        "baseline_latency_mean_ms": baseline_latency_mean_ms or "",
        "latency_increase_ratio": round(increase_ratio, 6),
        "stable": stable,
        "stop_reason": stop_reason,
        "artifact_dir": str(artifact_dir),
    }


def build_mds_command(
    python_executable: str,
    *,
    host: str,
    port: int,
    runtime_base_url: str,
    runtime_capacity: int,
    runtime_replicas: int,
) -> list[str]:
    capped_capacity = min(max(runtime_capacity, 1), 100)
    replicas = max(runtime_replicas, 1)
    return [
        python_executable,
        "-m",
        "aibrix.metadata.app",
        "--host",
        host,
        "--port",
        str(port),
        "--batch-runtime-endpoint",
        runtime_base_url,
        "--batch-runtime-capacity",
        str(capped_capacity),
        "--batch-runtime-source",
        "discovery",
        "--batch-runtime-replicas",
        str(replicas),
    ]


def build_go_command(
    *,
    scenario: str,
    mds_base_url: str,
    runtime_port: int,
    runtime_fixed_latency: str,
    duration: str,
    concurrency: int,
    total_jobs: int,
    requests_per_job: int,
    output_dir: Path,
    scrape_metrics_url: str | None,
) -> list[str]:
    command = [
        "go",
        "run",
        "./python/aibrix/scripts/prometheus_sink_stress",
        f"--scenario={scenario}",
        f"--mds-base-url={mds_base_url}",
        f"--runtime-listen-addr=127.0.0.1:{runtime_port}",
        f"--runtime-fixed-latency-ms={runtime_fixed_latency}",
        f"--duration={duration}",
        f"--concurrency={concurrency}",
        f"--total-jobs={total_jobs}",
        f"--requests-per-job={requests_per_job}",
        f"--output-dir={output_dir}",
    ]
    if scrape_metrics_url is not None:
        command.append(f"--scrape-metrics-url={scrape_metrics_url}")
    return command


def write_ramp_summary(records: list[dict[str, Any]], output_dir: Path) -> None:
    json_path = output_dir / "ramp_summary.json"
    csv_path = output_dir / "ramp_summary.csv"
    json_path.write_text(json.dumps(records, indent=2), encoding="utf-8")
    with csv_path.open("w", newline="", encoding="utf-8") as csv_file:
        writer = csv.DictWriter(csv_file, fieldnames=RAMP_SUMMARY_CSV_FIELDS)
        writer.writeheader()
        writer.writerows(records)


def render_comparison(summaries: dict[str, dict[str, Any]]) -> str:
    lines = ["# Prometheus Sink Stress Comparison", ""]
    for scenario in sorted(summaries):
        summary = summaries[scenario]
        lines.extend(
            [
                f"## {scenario}",
                "",
                f"- total_jobs: {summary.get('total_jobs', 0)}",
                f"- total_requests: {summary.get('total_requests', 0)}",
                f"- success_count: {summary.get('success_count', 0)}",
                f"- failure_count: {summary.get('failure_count', 0)}",
                f"- latency_p50_ms: {summary.get('latency_p50_ms', 0):.2f}",
                f"- latency_p95_ms: {summary.get('latency_p95_ms', 0):.2f}",
                f"- latency_p99_ms: {summary.get('latency_p99_ms', 0):.2f}",
                f"- throughput_rps: {summary.get('throughput_rps', 0):.2f}",
                "",
                "Failure categories:",
            ]
        )
        failure_categories = summary.get("failure_categories", {})
        if failure_categories:
            for key in sorted(failure_categories):
                lines.append(f"- {key}: {failure_categories[key]}")
        else:
            lines.append("- none")
        lines.append("")
    return "\n".join(lines)


def run_scenario(
    args: argparse.Namespace,
    *,
    base_env: dict[str, str],
    repo_root: Path,
    artifacts_root: Path,
    mds_base_url: str,
    scenario: str,
    concurrency: int,
    total_jobs: int,
    requests_per_job: int,
    runtime_replicas: int,
) -> dict[str, Any]:
    runtime_port = reserve_local_port()
    runtime_base_url = f"http://127.0.0.1:{runtime_port}"
    scenario_dir = artifacts_root / scenario
    scenario_dir.mkdir(parents=True, exist_ok=True)
    mds_log_path = scenario_dir / "mds.log"
    driver_log_path = scenario_dir / "driver.log"

    env = scenario_env(base_env, scenario, scenario_dir)
    with mds_log_path.open("w", encoding="utf-8") as mds_log:
        mds_process = subprocess.Popen(
            build_mds_command(
                sys.executable,
                host=args.mds_host,
                port=args.mds_port,
                runtime_base_url=runtime_base_url,
                runtime_capacity=args.batch_runtime_capacity,
                runtime_replicas=runtime_replicas,
            ),
            cwd=repo_root / "python" / "aibrix",
            env=env,
            stdout=mds_log,
            stderr=subprocess.STDOUT,
            text=True,
        )
        try:
            wait_for_http(f"{mds_base_url}/healthz")
            go_args = build_go_command(
                scenario=scenario,
                mds_base_url=mds_base_url,
                runtime_port=runtime_port,
                runtime_fixed_latency=args.runtime_fixed_latency_ms,
                duration=args.duration,
                concurrency=concurrency,
                total_jobs=total_jobs,
                requests_per_job=requests_per_job,
                output_dir=scenario_dir,
                scrape_metrics_url=(f"{mds_base_url}/metrics" if scenario == "D" else None),
            )
            with driver_log_path.open("w", encoding="utf-8") as driver_log:
                subprocess.run(
                    go_args,
                    cwd=repo_root,
                    check=True,
                    env=base_env,
                    stdout=driver_log,
                    stderr=subprocess.STDOUT,
                    text=True,
                )
        finally:
            stop_process(mds_process)

    for filename, row_count in extract_scenario_logs(scenario_dir).items():
        print(f"Stress log CSV written to {scenario_dir / filename} rows={row_count}")

    summary_path = scenario_dir / "summary.json"
    if not summary_path.exists():
        raise RuntimeError(f"Missing summary for scenario {scenario}: {summary_path}")
    return json.loads(summary_path.read_text(encoding="utf-8"))


def run_ramp(
    args: argparse.Namespace,
    *,
    base_env: dict[str, str],
    repo_root: Path,
    artifacts_root: Path,
    mds_base_url: str,
) -> None:
    scenario = args.scenarios[0]
    concurrency = max(args.ramp_start_concurrency, 1)
    baseline_latency_mean_ms: float | None = None
    records: list[dict[str, Any]] = []
    stop_reason = "max_concurrency_reached"

    while True:
        run_latencies: list[float] = []
        level_stable = False
        level_stop_reason = ""
        total_jobs = default_ramp_total_jobs(
            concurrency,
            jobs_per_concurrency=args.ramp_jobs_per_concurrency,
        )
        for run_index in range(1, max(args.ramp_max_runs_per_level, 1) + 1):
            run_root = (
                artifacts_root
                / "ramp"
                / f"concurrency-{concurrency:03d}"
                / f"run-{run_index:02d}"
            )
            summary = run_scenario(
                args,
                base_env=base_env,
                repo_root=repo_root,
                artifacts_root=run_root,
                mds_base_url=mds_base_url,
                scenario=scenario,
                concurrency=concurrency,
                total_jobs=total_jobs,
                requests_per_job=max(args.ramp_requests_per_job, 1),
                runtime_replicas=max(args.ramp_batch_runtime_replicas, 1),
            )
            latency_mean_ms = float(summary.get("latency_mean_ms", 0.0))
            run_latencies.append(latency_mean_ms)
            level_stable = is_ramp_level_stable(
                run_latencies,
                min_runs=args.ramp_min_runs_per_level,
                threshold=max(args.ramp_stability_threshold, 0.0),
            )
            record = ramp_record(
                scenario=scenario,
                concurrency=concurrency,
                run_index=run_index,
                requests_per_job=max(args.ramp_requests_per_job, 1),
                summary=summary,
                baseline_latency_mean_ms=baseline_latency_mean_ms,
                stable=level_stable,
                stop_reason="",
                artifact_dir=run_root / scenario,
            )
            records.append(record)
            write_ramp_summary(records, artifacts_root)
            if level_stable:
                if baseline_latency_mean_ms is None:
                    baseline_latency_mean_ms = latency_mean_ms
                    record["baseline_latency_mean_ms"] = baseline_latency_mean_ms
                    record["latency_increase_ratio"] = 0.0
                    write_ramp_summary(records, artifacts_root)
                elif should_stop_ramp(
                    baseline_latency_mean_ms=baseline_latency_mean_ms,
                    current_latency_mean_ms=latency_mean_ms,
                    threshold=max(args.ramp_threshold, 0.0),
                ):
                    stop_reason = "latency_threshold_exceeded"
                    level_stop_reason = stop_reason
                break
        if not level_stable:
            stop_reason = ramp_level_stop_reason(
                baseline_latency_mean_ms=baseline_latency_mean_ms,
                current_latency_mean_ms=run_latencies[-1] if run_latencies else 0.0,
                stable=level_stable,
                threshold=max(args.ramp_threshold, 0.0),
            )
            level_stop_reason = stop_reason
        if level_stop_reason:
            records[-1]["stop_reason"] = level_stop_reason
            write_ramp_summary(records, artifacts_root)
            print(f"Ramp stopped: {level_stop_reason}")
            return
        next_concurrency = next_ramp_concurrency(
            current=concurrency,
            growth_factor=args.ramp_growth_factor,
            max_concurrency=args.ramp_max_concurrency,
        )
        if next_concurrency is None:
            records[-1]["stop_reason"] = stop_reason
            write_ramp_summary(records, artifacts_root)
            print(f"Ramp stopped: {stop_reason}")
            return
        concurrency = next_concurrency


def main() -> None:
    args = parse_args()
    repo_root = Path(__file__).resolve().parents[3]
    artifacts_root = Path(args.artifacts_dir).resolve()
    artifacts_root.mkdir(parents=True, exist_ok=True)

    base_env = os.environ.copy()
    mds_base_url = f"http://{args.mds_host}:{args.mds_port}"
    if args.ramp_concurrency:
        run_ramp(
            args,
            base_env=base_env,
            repo_root=repo_root,
            artifacts_root=artifacts_root,
            mds_base_url=mds_base_url,
        )
        return

    summaries: dict[str, dict[str, Any]] = {}

    for scenario in args.scenarios:
        total_jobs = args.b_total_jobs if scenario == "B" else args.total_jobs
        requests_per_job = (
            args.b_requests_per_job if scenario == "B" else args.requests_per_job
        )
        summaries[scenario] = run_scenario(
            args,
            base_env=base_env,
            repo_root=repo_root,
            artifacts_root=artifacts_root,
            mds_base_url=mds_base_url,
            scenario=scenario,
            concurrency=args.concurrency,
            total_jobs=total_jobs,
            requests_per_job=requests_per_job,
            runtime_replicas=args.batch_runtime_replicas,
        )

    comparison = render_comparison(summaries)
    comparison_path = artifacts_root / "comparison.md"
    comparison_path.write_text(comparison, encoding="utf-8")
    print(f"Comparison report written to {comparison_path}")


if __name__ == "__main__":
    main()
