CONFIG = {
    # Run only the local batch API scenario.
    "scenarios": ["A"],
    # Ramp job concurrency until task_execution exceeds the baseline by 50%.
    "ramp-concurrency": True,
    "ramp-latency-source": "execution-phase",
    "ramp-execution-phase": "task_execution",
    "ramp-threshold": 0.50,
    # In ramp mode, a positive total-jobs value caps total jobs per run and
    # also caps the maximum useful job concurrency. Set 0 to remove that cap.
    "total-jobs": 0,
    # Ramp total jobs are concurrency * ramp-jobs-per-concurrency before any
    # positive total-jobs cap is applied. A value of 1 means concurrency 1 runs
    # 1 job, concurrency 2 runs 2 jobs, and so on.
    "ramp-jobs-per-concurrency": 1,
    # Keep each concurrency level to one run.
    "ramp-min-runs-per-level": 1,
    # "ramp-max-runs-per-level": 1,
    "ramp-requests-per-job": 1000,
    # Keep adaptive disabled; each job should dispatch at the configured capacity.
    "batch-runtime-capacity": 100,
    "batch-runtime-timeout-seconds": 45,
    # Use the original HTTPX implementation when collecting HTTPX baseline data.
    # Switch to "aiohttp_client" for the optimized client path.
    "batch-runtime-http-client": "httpx_client",
    "runtime-fixed-latency-ms": "30s",
    # "duration": "45m",
    "artifacts-dir": "artifacts/ramp_stress_test_30s_httpx_caffeinated",
}
