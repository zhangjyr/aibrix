package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFixedLatencyRuntimeHandler(t *testing.T) {
	handler := newRuntimeHandler(100 * time.Millisecond)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo"}`))
	rec := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("expected fixed latency >= 100ms, got %s", elapsed)
	}
	if !strings.Contains(rec.Body.String(), `"id":"stress-response"`) {
		t.Fatalf("expected deterministic response body, got %s", rec.Body.String())
	}
}

func TestSummarizeRunAggregatesLatenciesAndFailures(t *testing.T) {
	summary := summarizeRun(
		"A",
		[]DriverResult{
			{Latency: 10 * time.Millisecond, Category: categorySuccess},
			{Latency: 20 * time.Millisecond, Category: categorySuccess, RequestsInJob: 3},
			{Latency: 30 * time.Millisecond, Category: categoryPoolTimeout, Err: errors.New("pool"), RequestsInJob: 2},
			{Latency: 40 * time.Millisecond, Category: categoryMDS5xx, StatusCode: http.StatusInternalServerError},
		},
		[]RuntimeStats{
			{CompletedRequests: 1, InFlight: 1, HandlerLatencyMs: 10},
			{CompletedRequests: 2, InFlight: 2, HandlerLatencyMs: 20},
			{CompletedRequests: 3, InFlight: 1, HandlerLatencyMs: 15},
		},
		2*time.Second,
	)

	if summary.Scenario != "A" {
		t.Fatalf("expected scenario A, got %s", summary.Scenario)
	}
	if summary.TotalJobs != 4 {
		t.Fatalf("expected 4 jobs, got %d", summary.TotalJobs)
	}
	if summary.TotalRequests != 7 {
		t.Fatalf("expected 7 requests, got %d", summary.TotalRequests)
	}
	if summary.SuccessCount != 2 {
		t.Fatalf("expected 2 successes, got %d", summary.SuccessCount)
	}
	if summary.FailureCount != 2 {
		t.Fatalf("expected 2 failures, got %d", summary.FailureCount)
	}
	if summary.FailureCategories[categoryPoolTimeout] != 1 {
		t.Fatalf("expected 1 pool timeout, got %d", summary.FailureCategories[categoryPoolTimeout])
	}
	if summary.LatencyP50Ms != 20.0 {
		t.Fatalf("expected p50 20ms, got %f", summary.LatencyP50Ms)
	}
	if summary.LatencyMeanMs != 25.0 {
		t.Fatalf("expected mean 25ms, got %f", summary.LatencyMeanMs)
	}
	if summary.LatencyP95Ms != 40.0 {
		t.Fatalf("expected p95 40ms, got %f", summary.LatencyP95Ms)
	}
	if summary.JobThroughputJPS != 2.0 {
		t.Fatalf("expected 2 jobs/sec, got %f", summary.JobThroughputJPS)
	}
	if summary.RequestThroughputRPS != 3.5 {
		t.Fatalf("expected 3.5 requests/sec, got %f", summary.RequestThroughputRPS)
	}
	if summary.RuntimeRequests != 3 {
		t.Fatalf("expected 3 runtime requests, got %d", summary.RuntimeRequests)
	}
	if summary.RuntimeMaxInFlight != 2 {
		t.Fatalf("expected runtime max in-flight 2, got %d", summary.RuntimeMaxInFlight)
	}
	if summary.RuntimeHandlerMeanMs != 15.0 {
		t.Fatalf("expected runtime handler mean 15ms, got %f", summary.RuntimeHandlerMeanMs)
	}
}

func TestCollectRunDataWaitsForResultChannelAfterContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	driverResultCh := make(chan DriverResult, 1)
	runtimeReqCh := make(chan RuntimeStats, 1)
	driverResultCh <- DriverResult{JobIndex: 1, Category: categorySuccess}
	runtimeReqCh <- RuntimeStats{InFlight: 1, TotalRequests: 1}
	close(driverResultCh)

	results, runtimeSnapshots := collectRunData(ctx, driverResultCh, runtimeReqCh, time.Hour, time.Hour, nil, func() {})

	if len(results) != 1 {
		t.Fatalf("expected collector to drain one result after cancellation, got %d", len(results))
	}
	if len(runtimeSnapshots) != 1 {
		t.Fatalf("expected collector to drain one runtime snapshot, got %d", len(runtimeSnapshots))
	}
}

func TestRuntimeCSVRecorderFlushesSnapshotsBeforeClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.csv")
	recorder, err := newRuntimeCSVRecorder(path)
	if err != nil {
		t.Fatalf("create recorder: %v", err)
	}
	defer recorder.Close()

	if err := recorder.Record(RuntimeStats{
		Timestamp:              time.Unix(10, 0).UTC(),
		TotalRequests:          7,
		InFlight:               3,
		RequestBytesTotal:      11,
		ResponseBytesTotal:     13,
		HandlerLatencyMs:       50,
		RequestTPSWindow:       2.5,
		RequestBytesPerSecond:  5.5,
		ResponseBytesPerSecond: 6.5,
	}); err != nil {
		t.Fatalf("record snapshot: %v", err)
	}
	if err := recorder.Flush(); err != nil {
		t.Fatalf("flush recorder: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runtime csv: %v", err)
	}
	want := strings.Join([]string{
		"timestamp,total_requests,in_flight,max_in_flight,request_bytes_total,response_bytes_total,handler_latency_ms,request_tps_window,request_bytes_per_second_window,response_bytes_per_second_window",
		"1970-01-01T00:00:10Z,7,3,0,11,13,50.000,2.500,5.500,6.500",
		"",
	}, "\n")
	if string(data) != want {
		t.Fatalf("expected flushed runtime csv %q, got %q", want, string(data))
	}
}

func TestWriteRuntimeCSVComputesWindowRates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.csv")

	err := writeRuntimeCSV(path, []RuntimeStats{
		{
			Timestamp:          time.Unix(10, 0).UTC(),
			CompletedRequests:  1,
			RequestBytesTotal:  100,
			ResponseBytesTotal: 200,
			HandlerLatencyMs:   50,
		},
		{
			Timestamp:          time.Unix(12, 0).UTC(),
			CompletedRequests:  5,
			RequestBytesTotal:  500,
			ResponseBytesTotal: 1000,
			HandlerLatencyMs:   50,
		},
	})
	if err != nil {
		t.Fatalf("write runtime csv: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runtime csv: %v", err)
	}
	want := strings.Join([]string{
		"timestamp,total_requests,in_flight,max_in_flight,request_bytes_total,response_bytes_total,handler_latency_ms,request_tps_window,request_bytes_per_second_window,response_bytes_per_second_window",
		"1970-01-01T00:00:10Z,1,0,0,100,200,50.000,0.000,0.000,0.000",
		"1970-01-01T00:00:12Z,5,0,0,500,1000,50.000,2.000,200.000,400.000",
		"",
	}, "\n")
	if string(data) != want {
		t.Fatalf("expected runtime csv %q, got %q", want, string(data))
	}
}

func TestRuntimeTrackerCountsRequestsWhenTelemetryDrops(t *testing.T) {
	tracker := newRuntimeTracker()
	runtimeReqCh := make(chan RuntimeStats)
	handler := trackRuntime(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}),
		runtimeReqCh,
		tracker,
	)

	for range 3 {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"x":1}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	snapshot := tracker.Snapshot()
	if snapshot.TotalRequests != 3 {
		t.Fatalf("expected total requests 3, got %d", snapshot.TotalRequests)
	}
	if snapshot.CompletedRequests != 3 {
		t.Fatalf("expected completed requests 3, got %d", snapshot.CompletedRequests)
	}
	if snapshot.InFlight != 0 {
		t.Fatalf("expected no in-flight requests after completion, got %d", snapshot.InFlight)
	}
	if snapshot.MaxInFlight != 1 {
		t.Fatalf("expected max in-flight 1, got %d", snapshot.MaxInFlight)
	}
	if snapshot.RequestBytesTotal == 0 {
		t.Fatalf("expected request bytes to be recorded")
	}
	if snapshot.ResponseBytesTotal != 6 {
		t.Fatalf("expected response bytes 6, got %d", snapshot.ResponseBytesTotal)
	}
}

func TestSummarizeRuntimeStatsAggregatesHandlerLatencyAndBytes(t *testing.T) {
	summary := summarizeRun(
		"A",
		[]DriverResult{{Latency: 10 * time.Millisecond, Category: categorySuccess}},
		[]RuntimeStats{
			{TotalRequests: 1, InFlight: 1, RequestBytesTotal: 100, ResponseBytesTotal: 200, HandlerLatencyMs: 10},
			{TotalRequests: 2, InFlight: 2, RequestBytesTotal: 250, ResponseBytesTotal: 500, HandlerLatencyMs: 20},
			{TotalRequests: 3, InFlight: 1, RequestBytesTotal: 450, ResponseBytesTotal: 900, HandlerLatencyMs: 30},
		},
		2*time.Second,
	)

	if summary.RuntimeRequests != 3 {
		t.Fatalf("expected runtime requests 3, got %d", summary.RuntimeRequests)
	}
	if summary.RuntimeMaxInFlight != 2 {
		t.Fatalf("expected runtime max in-flight 2, got %d", summary.RuntimeMaxInFlight)
	}
	if summary.RuntimeHandlerMeanMs != 20 {
		t.Fatalf("expected runtime mean 20ms, got %f", summary.RuntimeHandlerMeanMs)
	}
	if summary.RuntimeRequestThroughputRPS != 1.5 {
		t.Fatalf("expected runtime throughput 1.5 rps, got %f", summary.RuntimeRequestThroughputRPS)
	}
	if summary.RuntimeRequestBytesPerSecond != 225 {
		t.Fatalf("expected runtime request bytes/sec 225, got %f", summary.RuntimeRequestBytesPerSecond)
	}
	if summary.RuntimeResponseBytesPerSecond != 450 {
		t.Fatalf("expected runtime response bytes/sec 450, got %f", summary.RuntimeResponseBytesPerSecond)
	}
}

func TestRenderReportIncludesRuntimeStats(t *testing.T) {
	report := renderReport(
		Config{
			Scenario:            "A",
			MDSBaseURL:          "http://127.0.0.1:8090",
			RuntimeListenAddr:   "127.0.0.1:18080",
			RuntimeFixedLatency: 50 * time.Millisecond,
		},
		RunSummary{
			Scenario:                      "A",
			TotalJobs:                     2,
			TotalRequests:                 10,
			SuccessCount:                  2,
			LatencyP50Ms:                  100,
			LatencyP95Ms:                  110,
			LatencyP99Ms:                  120,
			ThroughputRPS:                 0.5,
			RuntimeRequests:               10,
			RuntimeMaxInFlight:            5,
			RuntimeHandlerMeanMs:          50.1,
			RuntimeHandlerP95Ms:           51.2,
			RuntimeRequestThroughputRPS:   2.5,
			RuntimeRequestBytesPerSecond:  256.3,
			RuntimeResponseBytesPerSecond: 652.5,
			FailureCategories:             map[string]int{categorySuccess: 2},
		},
	)

	for _, want := range []string{
		"## Runtime Stats",
		"- Runtime requests: 10",
		"- Runtime max in-flight: 5",
		"- Runtime handler mean: 50.10 ms",
		"- Runtime request throughput: 2.50 rps",
		"- Runtime response bytes/sec: 652.50",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("expected report to contain %q, got %s", want, report)
		}
	}
}

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		err        error
		want       string
	}{
		{name: "success", statusCode: http.StatusOK, want: categorySuccess},
		{name: "pool timeout body", statusCode: http.StatusInternalServerError, body: "PoolTimeout", want: categoryPoolTimeout},
		{name: "mds 4xx", statusCode: http.StatusBadRequest, want: categoryMDS4xx},
		{name: "mds 5xx", statusCode: http.StatusInternalServerError, want: categoryMDS5xx},
		{name: "transport timeout", err: context.DeadlineExceeded, want: categoryTransportTimeout},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFailure(tc.statusCode, tc.body, tc.err)
			if got != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}
}
