package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	categorySuccess          = "success"
	categoryMDS4xx           = "mds_4xx"
	categoryMDS5xx           = "mds_5xx"
	categoryTransportTimeout = "transport_timeout"
	categoryPoolTimeout      = "pool_timeout"
	categoryTransportError   = "transport_error"
	categoryDecodeError      = "decode_error"
	categoryOther            = "other"
	runtimeCSVFlushInterval  = 15 * time.Second
)

type Config struct {
	Scenario               string
	MDSBaseURL             string
	APIKey                 string
	RuntimeListenAddr      string
	RuntimeBaseURL         string
	RuntimeFixedLatency    time.Duration
	Duration               time.Duration
	Concurrency            int
	OutputDir              string
	ScrapeMetricsURL       string
	Endpoint               string
	CompletionWindow       string
	JobAdaptiveConcurrency bool
	RequestsPerJob         int
	TotalJobs              int
	PollTimeout            time.Duration
	PollInterval           time.Duration
	CleanupFiles           bool
	StatsInterval          time.Duration
}

type PlannedJob struct {
	Index int
}

type DriverResult struct {
	JobIndex      int           `json:"job_index"`
	JobID         string        `json:"job_id,omitempty"`
	InputFileID   string        `json:"input_file_id,omitempty"`
	OutputFileID  string        `json:"output_file_id,omitempty"`
	Latency       time.Duration `json:"latency_ns"`
	StatusCode    int           `json:"status_code,omitempty"`
	Category      string        `json:"category"`
	Err           error         `json:"-"`
	ErrorMessage  string        `json:"error_message,omitempty"`
	BatchStatus   string        `json:"batch_status,omitempty"`
	RequestsInJob int           `json:"requests_in_job"`
}

type RunSummary struct {
	Scenario                      string         `json:"scenario"`
	TotalJobs                     int            `json:"total_jobs"`
	TotalRequests                 int            `json:"total_requests"`
	SuccessCount                  int            `json:"success_count"`
	FailureCount                  int            `json:"failure_count"`
	LatencyMeanMs                 float64        `json:"latency_mean_ms"`
	LatencyP50Ms                  float64        `json:"latency_p50_ms"`
	LatencyP95Ms                  float64        `json:"latency_p95_ms"`
	LatencyP99Ms                  float64        `json:"latency_p99_ms"`
	JobThroughputJPS              float64        `json:"job_throughput_jps"`
	RequestThroughputRPS          float64        `json:"request_throughput_rps"`
	ThroughputRPS                 float64        `json:"throughput_rps"`
	RuntimeRequests               int64          `json:"runtime_requests"`
	RuntimeMaxInFlight            int64          `json:"runtime_max_inflight"`
	RuntimeHandlerMeanMs          float64        `json:"runtime_handler_mean_ms"`
	RuntimeHandlerP95Ms           float64        `json:"runtime_handler_p95_ms"`
	RuntimeRequestThroughputRPS   float64        `json:"runtime_request_throughput_rps"`
	RuntimeRequestBytesPerSecond  float64        `json:"runtime_request_bytes_per_second"`
	RuntimeResponseBytesPerSecond float64        `json:"runtime_response_bytes_per_second"`
	FailureCategories             map[string]int `json:"failure_categories"`
}

type RuntimeStats struct {
	Timestamp              time.Time `json:"timestamp"`
	InFlight               int64     `json:"in_flight"`
	MaxInFlight            int64     `json:"max_inflight"`
	TotalRequests          int64     `json:"total_requests"`
	CompletedRequests      int64     `json:"completed_requests"`
	RequestBytesTotal      int64     `json:"request_bytes_total"`
	ResponseBytesTotal     int64     `json:"response_bytes_total"`
	HandlerLatencyMs       float64   `json:"handler_latency_ms"`
	RequestTPSWindow       float64   `json:"request_tps_window"`
	RequestBytesPerSecond  float64   `json:"request_bytes_per_second_window"`
	ResponseBytesPerSecond float64   `json:"response_bytes_per_second_window"`
}

type DriverStats struct {
	Sent      atomic.Int64
	Completed atomic.Int64
	InFlight  atomic.Int64
}

type runtimeCSVRecorder struct {
	file         *os.File
	writer       *csv.Writer
	lastSnapshot *RuntimeStats
}

type countingResponseWriter struct {
	http.ResponseWriter
	bytesWritten int64
}

type runtimeTracker struct {
	inFlight           atomic.Int64
	maxInFlight        atomic.Int64
	totalRequests      atomic.Int64
	completedRequests  atomic.Int64
	requestBytesTotal  atomic.Int64
	responseBytesTotal atomic.Int64
	snapshotMu         sync.Mutex
}

func (w *countingResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytesWritten += int64(n)
	return n, err
}

func newRuntimeTracker() *runtimeTracker {
	return &runtimeTracker{}
}

func (t *runtimeTracker) RecordStart(requestBytes int64) {
	t.totalRequests.Add(1)
	t.requestBytesTotal.Add(requestBytes)
	inFlight := t.inFlight.Add(1)
	t.recordMaxInFlight(inFlight)
}

func (t *runtimeTracker) RecordCompletion(responseBytes int64, handlerLatencyMs float64) RuntimeStats {
	t.snapshotMu.Lock()
	defer t.snapshotMu.Unlock()

	t.responseBytesTotal.Add(responseBytes)
	completed := t.completedRequests.Add(1)
	inFlight := t.inFlight.Add(-1)
	return RuntimeStats{
		Timestamp:          time.Now().UTC(),
		InFlight:           inFlight,
		MaxInFlight:        t.maxInFlight.Load(),
		TotalRequests:      t.totalRequests.Load(),
		CompletedRequests:  completed,
		RequestBytesTotal:  t.requestBytesTotal.Load(),
		ResponseBytesTotal: t.responseBytesTotal.Load(),
		HandlerLatencyMs:   handlerLatencyMs,
	}
}

func (t *runtimeTracker) Snapshot() RuntimeStats {
	t.snapshotMu.Lock()
	defer t.snapshotMu.Unlock()

	return RuntimeStats{
		Timestamp:          time.Now().UTC(),
		InFlight:           t.inFlight.Load(),
		MaxInFlight:        t.maxInFlight.Load(),
		TotalRequests:      t.totalRequests.Load(),
		CompletedRequests:  t.completedRequests.Load(),
		RequestBytesTotal:  t.requestBytesTotal.Load(),
		ResponseBytesTotal: t.responseBytesTotal.Load(),
	}
}

func (t *runtimeTracker) recordMaxInFlight(value int64) {
	for {
		current := t.maxInFlight.Load()
		if value <= current {
			return
		}
		if t.maxInFlight.CompareAndSwap(current, value) {
			return
		}
	}
}

type FileCreateResponse struct {
	ID string `json:"id"`
}

type BatchCreateResponse struct {
	ID string `json:"id"`
}

type BatchStatusResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	OutputFileID string `json:"output_file_id"`
	ErrorFileID  string `json:"error_file_id"`
}

func main() {
	cfg := parseFlags()
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx := signalCtx
	cancel := context.CancelFunc(func() {})
	if cfg.Duration > 0 {
		ctx, cancel = context.WithTimeout(signalCtx, cfg.Duration)
	}
	defer cancel()
	runStart := time.Now()

	runtimeReqCh := make(chan RuntimeStats, 1024)
	driverResultCh := make(chan DriverResult, maxInt(cfg.Concurrency*2, 64))
	jobPlanCh := make(chan PlannedJob, maxInt(cfg.Concurrency*2, 64))
	stats := &DriverStats{}
	runtimeTracker := newRuntimeTracker()

	runtimeServer := &http.Server{
		Addr:    cfg.RuntimeListenAddr,
		Handler: buildRuntimeMux(cfg.RuntimeFixedLatency, runtimeReqCh, runtimeTracker),
	}

	runtimeErrCh := make(chan error, 1)
	go func() {
		if err := runtimeServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			runtimeErrCh <- err
		}
	}()

	if err := waitForRuntime(ctx, cfg.RuntimeBaseURL); err != nil {
		log.Fatalf("runtime did not become ready: %v", err)
	}

	if cfg.Scenario == "D" && cfg.ScrapeMetricsURL != "" {
		go scrapeMetrics(ctx, cfg.ScrapeMetricsURL, cfg.StatsInterval)
	}

	go planJobs(ctx, cfg, jobPlanCh)

	var wg sync.WaitGroup
	client := &http.Client{Timeout: 60 * time.Second}
	for workerID := 0; workerID < cfg.Concurrency; workerID++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runWorker(ctx, client, cfg, id, jobPlanCh, driverResultCh, stats)
		}(workerID)
	}
	workersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workersDone)
	}()

	runtimeRecorder, err := newRuntimeCSVRecorder(filepath.Join(cfg.OutputDir, "runtime.csv"))
	if err != nil {
		log.Fatalf("create runtime csv recorder: %v", err)
	}

	doneCh := make(chan struct{})
	var results []DriverResult
	var runtimeSnapshots []RuntimeStats
	go func() {
		defer func() {
			if err := runtimeRecorder.Close(); err != nil {
				log.Printf("close runtime csv recorder: %v", err)
			}
		}()
		results, runtimeSnapshots = collectRunData(ctx, driverResultCh, runtimeReqCh, cfg.StatsInterval, runtimeCSVFlushInterval, runtimeRecorder, func() {
			log.Printf(
				"driver stats scenario=%s sent=%d completed=%d inflight=%d",
				cfg.Scenario,
				stats.Sent.Load(),
				stats.Completed.Load(),
				stats.InFlight.Load(),
			)
		})
		close(doneCh)
	}()

	select {
	case <-ctx.Done():
	case <-workersDone:
	case err := <-runtimeErrCh:
		log.Fatalf("runtime server failed: %v", err)
	}

	<-workersDone
	close(driverResultCh)
	<-doneCh
	runtimeSnapshots = append(runtimeSnapshots, runtimeTracker.Snapshot())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = runtimeServer.Shutdown(shutdownCtx)

	summary := summarizeRun(cfg.Scenario, results, runtimeSnapshots, time.Since(runStart))
	if err := writeArtifacts(cfg, summary, results, runtimeSnapshots); err != nil {
		log.Fatalf("write artifacts: %v", err)
	}
}

func collectRunData(ctx context.Context, driverResultCh <-chan DriverResult, runtimeReqCh <-chan RuntimeStats, statsInterval time.Duration, runtimeFlushInterval time.Duration, runtimeRecorder *runtimeCSVRecorder, logStats func()) ([]DriverResult, []RuntimeStats) {
	_ = ctx
	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()
	flushTicker := time.NewTicker(runtimeFlushInterval)
	defer flushTicker.Stop()

	var results []DriverResult
	var runtimeSnapshots []RuntimeStats
	for {
		select {
		case result, ok := <-driverResultCh:
			if !ok {
				for {
					select {
					case snapshot := <-runtimeReqCh:
						runtimeSnapshots = append(runtimeSnapshots, snapshot)
						if runtimeRecorder != nil {
							if err := runtimeRecorder.Record(snapshot); err != nil {
								log.Printf("record runtime csv: %v", err)
							}
						}
					default:
						if runtimeRecorder != nil {
							if err := runtimeRecorder.Flush(); err != nil {
								log.Printf("flush runtime csv: %v", err)
							}
						}
						return results, runtimeSnapshots
					}
				}
			}
			results = append(results, result)
		case snapshot := <-runtimeReqCh:
			runtimeSnapshots = append(runtimeSnapshots, snapshot)
			if runtimeRecorder != nil {
				if err := runtimeRecorder.Record(snapshot); err != nil {
					log.Printf("record runtime csv: %v", err)
				}
			}
		case <-ticker.C:
			logStats()
		case <-flushTicker.C:
			if runtimeRecorder != nil {
				if err := runtimeRecorder.Flush(); err != nil {
					log.Printf("flush runtime csv: %v", err)
				}
			}
		}
	}
}

func newRuntimeCSVRecorder(path string) (*runtimeCSVRecorder, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	writer := csv.NewWriter(file)
	if err := writer.Write(runtimeCSVHeader()); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &runtimeCSVRecorder{file: file, writer: writer}, nil
}

func (r *runtimeCSVRecorder) Record(snapshot RuntimeStats) error {
	if snapshot.Timestamp.IsZero() {
		snapshot.Timestamp = time.Now().UTC()
	}
	if r.lastSnapshot != nil {
		elapsed := snapshot.Timestamp.Sub(r.lastSnapshot.Timestamp).Seconds()
		if elapsed > 0 {
			snapshot.RequestTPSWindow = float64(snapshot.CompletedRequests-r.lastSnapshot.CompletedRequests) / elapsed
			snapshot.RequestBytesPerSecond = float64(snapshot.RequestBytesTotal-r.lastSnapshot.RequestBytesTotal) / elapsed
			snapshot.ResponseBytesPerSecond = float64(snapshot.ResponseBytesTotal-r.lastSnapshot.ResponseBytesTotal) / elapsed
		}
	}
	row := runtimeCSVRow(snapshot)
	copied := snapshot
	r.lastSnapshot = &copied
	return r.writer.Write(row)
}

func (r *runtimeCSVRecorder) Flush() error {
	r.writer.Flush()
	if err := r.writer.Error(); err != nil {
		return err
	}
	return r.file.Sync()
}

func (r *runtimeCSVRecorder) Close() error {
	flushErr := r.Flush()
	closeErr := r.file.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

func runtimeCSVHeader() []string {
	return []string{
		"timestamp",
		"total_requests",
		"in_flight",
		"max_in_flight",
		"request_bytes_total",
		"response_bytes_total",
		"handler_latency_ms",
		"request_tps_window",
		"request_bytes_per_second_window",
		"response_bytes_per_second_window",
	}
}

func runtimeCSVRow(snapshot RuntimeStats) []string {
	totalRequests := snapshot.CompletedRequests
	if totalRequests == 0 {
		totalRequests = snapshot.TotalRequests
	}
	return []string{
		snapshot.Timestamp.UTC().Format(time.RFC3339),
		fmt.Sprintf("%d", totalRequests),
		fmt.Sprintf("%d", snapshot.InFlight),
		fmt.Sprintf("%d", snapshot.MaxInFlight),
		fmt.Sprintf("%d", snapshot.RequestBytesTotal),
		fmt.Sprintf("%d", snapshot.ResponseBytesTotal),
		fmt.Sprintf("%.3f", snapshot.HandlerLatencyMs),
		fmt.Sprintf("%.3f", snapshot.RequestTPSWindow),
		fmt.Sprintf("%.3f", snapshot.RequestBytesPerSecond),
		fmt.Sprintf("%.3f", snapshot.ResponseBytesPerSecond),
	}
}

func parseFlags() Config {
	var cfg Config
	flag.StringVar(&cfg.Scenario, "scenario", "A", "scenario label")
	flag.StringVar(&cfg.MDSBaseURL, "mds-base-url", "http://127.0.0.1:8090", "MDS base URL")
	flag.StringVar(&cfg.APIKey, "api-key", "dummy", "API key for MDS if required")
	flag.StringVar(&cfg.RuntimeListenAddr, "runtime-listen-addr", "127.0.0.1:18080", "fake runtime listen address")
	flag.DurationVar(&cfg.RuntimeFixedLatency, "runtime-fixed-latency-ms", 100*time.Millisecond, "fixed runtime latency")
	flag.DurationVar(&cfg.Duration, "duration", 0, "optional max run duration; 0 waits for planned jobs to finish")
	flag.IntVar(&cfg.Concurrency, "concurrency", 4, "number of driver workers")
	flag.StringVar(&cfg.OutputDir, "output-dir", filepath.Join("python", "aibrix", "artifacts", "prometheus_sink_stress"), "artifact output directory")
	flag.StringVar(&cfg.ScrapeMetricsURL, "scrape-metrics-url", "", "optional metrics endpoint to scrape")
	flag.StringVar(&cfg.Endpoint, "endpoint", "/v1/chat/completions", "batch endpoint to execute")
	flag.StringVar(&cfg.CompletionWindow, "completion-window", "24h", "batch completion window")
	flag.BoolVar(&cfg.JobAdaptiveConcurrency, "job-adaptive-concurrency", false, "enable adaptive concurrency for created batch jobs")
	flag.IntVar(&cfg.RequestsPerJob, "requests-per-job", 1, "requests per batch job")
	flag.IntVar(&cfg.TotalJobs, "total-jobs", 100, "total jobs to submit")
	flag.DurationVar(&cfg.PollTimeout, "poll-timeout", 5*time.Minute, "max wait for terminal batch state")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", 2*time.Second, "poll interval")
	flag.BoolVar(&cfg.CleanupFiles, "cleanup-files", true, "delete created files after completion")
	flag.DurationVar(&cfg.StatsInterval, "stats-interval", 5*time.Second, "stats logging interval")
	flag.Parse()

	cfg.RuntimeFixedLatency = time.Duration(int64(cfg.RuntimeFixedLatency/time.Millisecond)) * time.Millisecond
	if cfg.RuntimeFixedLatency <= 0 {
		cfg.RuntimeFixedLatency = 100 * time.Millisecond
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.RequestsPerJob <= 0 {
		cfg.RequestsPerJob = 1
	}
	if cfg.TotalJobs <= 0 {
		cfg.TotalJobs = 1
	}
	if cfg.RuntimeBaseURL == "" {
		cfg.RuntimeBaseURL = "http://" + cfg.RuntimeListenAddr
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.StatsInterval <= 0 {
		cfg.StatsInterval = 5 * time.Second
	}
	return cfg
}

func buildRuntimeMux(latency time.Duration, runtimeReqCh chan<- RuntimeStats, tracker *runtimeTracker) http.Handler {
	mux := http.NewServeMux()
	handler := newRuntimeHandler(latency)
	mux.Handle("/v1/chat/completions", trackRuntime(handler, runtimeReqCh, tracker))
	mux.Handle("/health", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	return mux
}

func trackRuntime(next http.Handler, runtimeReqCh chan<- RuntimeStats, tracker *runtimeTracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestBytes := r.ContentLength
		if requestBytes < 0 {
			requestBytes = 0
		}
		tracker.RecordStart(requestBytes)
		recorder := &countingResponseWriter{ResponseWriter: w}
		defer func() {
			responseBytes := recorder.bytesWritten
			latencyMs := float64(time.Since(start)) / float64(time.Millisecond)
			snapshot := tracker.RecordCompletion(responseBytes, latencyMs)
			select {
			case runtimeReqCh <- snapshot:
			default:
			}
		}()
		next.ServeHTTP(recorder, r)
	})
}

func newRuntimeHandler(latency time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "stress-response",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "stress-model",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "stress-ok",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     8,
				"completion_tokens": 4,
				"total_tokens":      12,
			},
		})
	})
}

func waitForRuntime(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for runtime %s", baseURL)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func planJobs(ctx context.Context, cfg Config, jobPlanCh chan<- PlannedJob) {
	defer close(jobPlanCh)
	for i := 0; i < cfg.TotalJobs; i++ {
		select {
		case <-ctx.Done():
			return
		case jobPlanCh <- PlannedJob{Index: i}:
		}
	}
}

func runWorker(ctx context.Context, client *http.Client, cfg Config, workerID int, jobPlanCh <-chan PlannedJob, resultCh chan<- DriverResult, stats *DriverStats) {
	rng := rand.New(rand.NewSource(int64(workerID + 1)))
	for {
		select {
		case <-ctx.Done():
			return
		case plan, ok := <-jobPlanCh:
			if !ok {
				return
			}
			stats.Sent.Add(1)
			stats.InFlight.Add(1)
			result := executeJob(ctx, client, cfg, plan, rng)
			stats.InFlight.Add(-1)
			stats.Completed.Add(1)
			select {
			case <-ctx.Done():
				return
			case resultCh <- result:
			}
		}
	}
}

func executeJob(ctx context.Context, client *http.Client, cfg Config, plan PlannedJob, rng *rand.Rand) DriverResult {
	start := time.Now()
	records := buildRecords(plan.Index, cfg.RequestsPerJob, cfg.Endpoint, rng)
	inputPayload, err := buildJSONL(records)
	if err != nil {
		return DriverResult{JobIndex: plan.Index, Category: categoryOther, Err: err, ErrorMessage: err.Error(), RequestsInJob: cfg.RequestsPerJob}
	}

	fileID, statusCode, err := uploadInputFile(ctx, client, cfg, plan.Index, inputPayload)
	if err != nil {
		return DriverResult{
			JobIndex:      plan.Index,
			Latency:       time.Since(start),
			StatusCode:    statusCode,
			Category:      classifyFailure(statusCode, err.Error(), err),
			Err:           err,
			ErrorMessage:  err.Error(),
			RequestsInJob: cfg.RequestsPerJob,
		}
	}

	batchID, statusCode, err := createBatch(ctx, client, cfg, fileID)
	if err != nil {
		return DriverResult{
			JobIndex:      plan.Index,
			InputFileID:   fileID,
			Latency:       time.Since(start),
			StatusCode:    statusCode,
			Category:      classifyFailure(statusCode, err.Error(), err),
			Err:           err,
			ErrorMessage:  err.Error(),
			RequestsInJob: cfg.RequestsPerJob,
		}
	}

	status, statusCode, err := pollBatch(ctx, client, cfg, batchID)
	result := DriverResult{
		JobIndex:      plan.Index,
		JobID:         batchID,
		InputFileID:   fileID,
		Latency:       time.Since(start),
		StatusCode:    statusCode,
		RequestsInJob: cfg.RequestsPerJob,
	}
	if err != nil {
		result.Category = classifyFailure(statusCode, err.Error(), err)
		result.Err = err
		result.ErrorMessage = err.Error()
		return result
	}

	result.OutputFileID = status.OutputFileID
	result.BatchStatus = status.Status
	if isSuccessStatus(status.Status) {
		result.Category = categorySuccess
	} else {
		result.Category = classifyFailure(statusCode, status.Status, fmt.Errorf(status.Status))
		result.ErrorMessage = status.Status
	}

	if cfg.CleanupFiles {
		_ = deleteFile(ctx, client, cfg, fileID)
		if status.OutputFileID != "" {
			_ = deleteFile(ctx, client, cfg, status.OutputFileID)
		}
		if status.ErrorFileID != "" {
			_ = deleteFile(ctx, client, cfg, status.ErrorFileID)
		}
	}
	return result
}

func uploadInputFile(ctx context.Context, client *http.Client, cfg Config, jobIndex int, payload []byte) (string, int, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("purpose", "batch")
	part, err := writer.CreateFormFile("file", fmt.Sprintf("batch-%06d.jsonl", jobIndex))
	if err != nil {
		return "", 0, err
	}
	if _, err := part.Write(payload); err != nil {
		return "", 0, err
	}
	if err := writer.Close(); err != nil {
		return "", 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.MDSBaseURL, "/")+"/v1/files", &body)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", resp.StatusCode, fmt.Errorf(string(respBody))
	}
	var decoded FileCreateResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return "", resp.StatusCode, err
	}
	return decoded.ID, resp.StatusCode, nil
}

func createBatch(ctx context.Context, client *http.Client, cfg Config, fileID string) (string, int, error) {
	payload := map[string]any{
		"input_file_id":     fileID,
		"endpoint":          cfg.Endpoint,
		"completion_window": cfg.CompletionWindow,
		"aibrix": map[string]any{
			"client": map[string]any{
				"adaptive_concurrency": cfg.JobAdaptiveConcurrency,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.MDSBaseURL, "/")+"/v1/batches", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", resp.StatusCode, fmt.Errorf(string(respBody))
	}
	var decoded BatchCreateResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return "", resp.StatusCode, err
	}
	return decoded.ID, resp.StatusCode, nil
}

func pollBatch(ctx context.Context, client *http.Client, cfg Config, batchID string) (BatchStatusResponse, int, error) {
	pollCtx, cancel := context.WithTimeout(ctx, cfg.PollTimeout)
	defer cancel()
	url := strings.TrimRight(cfg.MDSBaseURL, "/") + "/v1/batches/" + batchID
	for {
		req, err := http.NewRequestWithContext(pollCtx, http.MethodGet, url, nil)
		if err != nil {
			return BatchStatusResponse{}, 0, err
		}
		if cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return BatchStatusResponse{}, 0, err
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return BatchStatusResponse{}, resp.StatusCode, fmt.Errorf(string(respBody))
		}
		var decoded BatchStatusResponse
		if err := json.Unmarshal(respBody, &decoded); err != nil {
			return BatchStatusResponse{}, resp.StatusCode, err
		}
		if isTerminalStatus(decoded.Status) {
			return decoded, resp.StatusCode, nil
		}
		select {
		case <-pollCtx.Done():
			return BatchStatusResponse{}, 0, pollCtx.Err()
		case <-time.After(cfg.PollInterval):
		}
	}
}

func deleteFile(ctx context.Context, client *http.Client, cfg Config, fileID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(cfg.MDSBaseURL, "/")+"/v1/files/"+fileID, nil)
	if err != nil {
		return err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete %s: %s", fileID, string(body))
	}
	return nil
}

func buildRecords(jobIndex, requestsPerJob int, endpoint string, rng *rand.Rand) []map[string]any {
	records := make([]map[string]any, 0, requestsPerJob)
	for i := 0; i < requestsPerJob; i++ {
		records = append(records, map[string]any{
			"custom_id": fmt.Sprintf("job-%06d-req-%02d", jobIndex, i),
			"method":    http.MethodPost,
			"url":       endpoint,
			"body": map[string]any{
				"model": "stress-model",
				"messages": []map[string]string{
					{"role": "user", "content": fmt.Sprintf("stress-%d-%d-%d", jobIndex, i, rng.Intn(1000))},
				},
				"max_tokens": 16,
			},
		})
	}
	return records
}

func buildJSONL(records []map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, record := range records {
		if err := enc.Encode(record); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func scrapeMetrics(ctx context.Context, metricsURL string, interval time.Duration) {
	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("metrics scrape failed url=%s err=%v", metricsURL, err)
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			log.Printf("metrics scrape status=%d url=%s", resp.StatusCode, metricsURL)
		}
	}
}

func summarizeRun(scenario string, results []DriverResult, runtimeSnapshots []RuntimeStats, duration time.Duration) RunSummary {
	latencies := make([]float64, 0, len(results))
	failures := map[string]int{}
	successCount := 0
	totalRequests := 0
	for _, result := range results {
		latencies = append(latencies, float64(result.Latency/time.Millisecond))
		failures[result.Category]++
		totalRequests += maxInt(result.RequestsInJob, 1)
		if result.Category == categorySuccess {
			successCount++
		}
	}
	summary := RunSummary{
		Scenario:          scenario,
		TotalJobs:         len(results),
		TotalRequests:     totalRequests,
		SuccessCount:      successCount,
		FailureCount:      len(results) - successCount,
		FailureCategories: failures,
	}
	summary.LatencyP50Ms = percentile(latencies, 0.50)
	summary.LatencyMeanMs = mean(latencies)
	summary.LatencyP95Ms = percentile(latencies, 0.95)
	summary.LatencyP99Ms = percentile(latencies, 0.99)
	if duration > 0 {
		summary.JobThroughputJPS = float64(len(results)) / duration.Seconds()
		summary.RequestThroughputRPS = float64(totalRequests) / duration.Seconds()
		summary.ThroughputRPS = summary.JobThroughputJPS
	}
	applyRuntimeSummary(&summary, runtimeSnapshots, duration)
	return summary
}

func applyRuntimeSummary(summary *RunSummary, snapshots []RuntimeStats, duration time.Duration) {
	if len(snapshots) == 0 {
		return
	}
	handlerLatencies := make([]float64, 0, len(snapshots))
	var maxInFlight int64
	var completedRequests int64
	var totalRequests int64
	var requestBytesTotal int64
	var responseBytesTotal int64
	for _, snapshot := range snapshots {
		if snapshot.MaxInFlight > maxInFlight {
			maxInFlight = snapshot.MaxInFlight
		}
		if snapshot.InFlight > maxInFlight {
			maxInFlight = snapshot.InFlight
		}
		if snapshot.CompletedRequests > completedRequests {
			completedRequests = snapshot.CompletedRequests
		}
		if snapshot.TotalRequests > totalRequests {
			totalRequests = snapshot.TotalRequests
		}
		if snapshot.RequestBytesTotal > requestBytesTotal {
			requestBytesTotal = snapshot.RequestBytesTotal
		}
		if snapshot.ResponseBytesTotal > responseBytesTotal {
			responseBytesTotal = snapshot.ResponseBytesTotal
		}
		if snapshot.HandlerLatencyMs > 0 {
			handlerLatencies = append(handlerLatencies, snapshot.HandlerLatencyMs)
		}
	}
	summary.RuntimeRequests = completedRequests
	if summary.RuntimeRequests == 0 {
		summary.RuntimeRequests = totalRequests
	}
	summary.RuntimeMaxInFlight = maxInFlight
	summary.RuntimeHandlerMeanMs = mean(handlerLatencies)
	summary.RuntimeHandlerP95Ms = percentile(handlerLatencies, 0.95)
	if duration > 0 {
		summary.RuntimeRequestThroughputRPS = float64(summary.RuntimeRequests) / duration.Seconds()
		summary.RuntimeRequestBytesPerSecond = float64(requestBytesTotal) / duration.Seconds()
		summary.RuntimeResponseBytesPerSecond = float64(responseBytesTotal) / duration.Seconds()
	}
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sortedValues := append([]float64(nil), values...)
	sort.Float64s(sortedValues)
	index := int(math.Ceil(float64(len(sortedValues))*p)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sortedValues) {
		index = len(sortedValues) - 1
	}
	return sortedValues[index]
}

func classifyFailure(statusCode int, body string, err error) string {
	if err == nil && statusCode < 400 {
		return categorySuccess
	}
	combined := strings.ToLower(body)
	if err != nil {
		combined += " " + strings.ToLower(err.Error())
		if errors.Is(err, context.DeadlineExceeded) {
			return categoryTransportTimeout
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return categoryTransportTimeout
		}
	}
	if strings.Contains(combined, "pooltimeout") {
		return categoryPoolTimeout
	}
	if statusCode >= 400 && statusCode < 500 {
		return categoryMDS4xx
	}
	if statusCode >= 500 {
		return categoryMDS5xx
	}
	if strings.Contains(combined, "timeout") {
		return categoryTransportTimeout
	}
	if strings.Contains(combined, "json") || strings.Contains(combined, "decode") {
		return categoryDecodeError
	}
	if err != nil {
		return categoryTransportError
	}
	return categoryOther
}

func writeArtifacts(cfg Config, summary RunSummary, results []DriverResult, runtimeSnapshots []RuntimeStats) error {
	summaryPath := filepath.Join(cfg.OutputDir, "summary.json")
	if err := writeJSON(summaryPath, summary); err != nil {
		return err
	}
	resultsPath := filepath.Join(cfg.OutputDir, "results.json")
	if err := writeJSON(resultsPath, results); err != nil {
		return err
	}
	runtimePath := filepath.Join(cfg.OutputDir, "runtime.csv")
	if err := writeRuntimeCSV(runtimePath, runtimeSnapshots); err != nil {
		return err
	}
	reportPath := filepath.Join(cfg.OutputDir, "report.md")
	return os.WriteFile(reportPath, []byte(renderReport(cfg, summary)), 0o644)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeRuntimeCSV(path string, snapshots []RuntimeStats) error {
	recorder, err := newRuntimeCSVRecorder(path)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		if err := recorder.Record(snapshot); err != nil {
			_ = recorder.Close()
			return err
		}
	}
	return recorder.Close()
}

func renderReport(cfg Config, summary RunSummary) string {
	return fmt.Sprintf(`# Prometheus Sink Stress Report

- Scenario: %s
- MDS base URL: %s
- Runtime listen addr: %s
- Runtime latency: %s
- Total jobs: %d
- Total requests: %d
- Success count: %d
- Failure count: %d
- Latency p50: %.2f ms
- Latency p95: %.2f ms
- Latency p99: %.2f ms
- Throughput: %.2f rps

## Runtime Stats

- Runtime requests: %d
- Runtime max in-flight: %d
- Runtime handler mean: %.2f ms
- Runtime handler p95: %.2f ms
- Runtime request throughput: %.2f rps
- Runtime request bytes/sec: %.2f
- Runtime response bytes/sec: %.2f

## Failure Categories

%s
`, summary.Scenario, cfg.MDSBaseURL, cfg.RuntimeListenAddr, cfg.RuntimeFixedLatency, summary.TotalJobs, summary.TotalRequests, summary.SuccessCount, summary.FailureCount, summary.LatencyP50Ms, summary.LatencyP95Ms, summary.LatencyP99Ms, summary.ThroughputRPS, summary.RuntimeRequests, summary.RuntimeMaxInFlight, summary.RuntimeHandlerMeanMs, summary.RuntimeHandlerP95Ms, summary.RuntimeRequestThroughputRPS, summary.RuntimeRequestBytesPerSecond, summary.RuntimeResponseBytesPerSecond, renderFailureCategories(summary.FailureCategories))
}

func renderFailureCategories(failures map[string]int) string {
	if len(failures) == 0 {
		return "- none"
	}
	keys := make([]string, 0, len(failures))
	for key := range failures {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("- %s: %d", key, failures[key]))
	}
	return strings.Join(lines, "\n")
}

func isTerminalStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "expired":
		return true
	default:
		return false
	}
}

func isSuccessStatus(status string) bool {
	return status == "completed"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
