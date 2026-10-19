package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func testAPI(t *testing.T, logger *slog.Logger) (*api, *tracetest.InMemoryExporter) {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	app := newAPI(logger, provider, "")
	t.Cleanup(func() {
		app.client.CloseIdleConnections()
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracing: %v", err)
		}
	})
	return app, exporter
}

func serveTestRequest(app *api, method, target string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, httptest.NewRequest(method, target, nil))
	return response
}

func counterValue(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matches := len(metric.GetLabel()) == len(labels)
			for _, label := range metric.GetLabel() {
				value, exists := labels[label.GetName()]
				matches = matches && exists && value == label.GetValue()
			}
			if matches {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return 0
}

func TestHealthAndFailSignals(t *testing.T) {
	var logs bytes.Buffer
	app, exporter := testAPI(t, slog.New(slog.NewJSONHandler(&logs, nil)))
	healthResponse := serveTestRequest(app, http.MethodGet, "/health")
	if healthResponse.Code != http.StatusOK || strings.TrimSpace(healthResponse.Body.String()) != "ok" {
		t.Fatalf("health = %d %q", healthResponse.Code, healthResponse.Body.String())
	}
	failResponse := serveTestRequest(app, http.MethodGet, "/fail")
	if failResponse.Code != http.StatusInternalServerError {
		t.Fatalf("fail status = %d", failResponse.Code)
	}
	if got := counterValue(t, app.metrics.registry, "http_errors_total", map[string]string{"method": "GET", "route": "/fail"}); got != 1 {
		t.Fatalf("error counter = %v, want 1", got)
	}
	for route, status := range map[string]string{"/health": "200", "/fail": "500"} {
		if got := counterValue(t, app.metrics.registry, "http_requests_total", map[string]string{"method": "GET", "route": route, "status": status}); got != 1 {
			t.Fatalf("%s request counter = %v, want 1", route, got)
		}
	}
	families, err := app.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var durationCount uint64
	for _, family := range families {
		if family.GetName() == "http_request_duration_seconds" {
			for _, metric := range family.GetMetric() {
				histogram := metric.GetHistogram()
				if histogram == nil || len(histogram.GetBucket()) == 0 {
					t.Fatal("request duration must be a histogram with buckets")
				}
				durationCount += histogram.GetSampleCount()
			}
		}
	}
	if durationCount != 2 {
		t.Fatalf("duration histogram count = %d, want 2", durationCount)
	}
	var failSpan tracetest.SpanStub
	for _, span := range exporter.GetSpans() {
		if span.Name == "GET /fail" {
			failSpan = span
		}
	}
	if !failSpan.SpanContext.IsValid() || failSpan.Status.Code != codes.Error || len(failSpan.Events) == 0 {
		t.Fatalf("failure must emit a valid Error span with an exception: %+v", failSpan)
	}
	if failResponse.Header().Get("X-Trace-ID") != failSpan.SpanContext.TraceID().String() {
		t.Fatal("response trace ID does not match the current server span")
	}
	decoder := json.NewDecoder(&logs)
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("request log is not JSON: %v", err)
		}
		if record["route"] == "/fail" {
			if record["trace_id"] != failSpan.SpanContext.TraceID().String() || record["level"] != "ERROR" || record["status"] != float64(500) {
				t.Fatalf("failure log does not identify its Error span: %v", record)
			}
			return
		}
	}
	t.Fatal("no JSON request log for /fail")
}

func TestMetricsScrapesDoNotGenerateRequestSignals(t *testing.T) {
	var logs bytes.Buffer
	app, exporter := testAPI(t, slog.New(slog.NewJSONHandler(&logs, nil)))
	serveTestRequest(app, http.MethodGet, "/health")
	beforeLogs, beforeSpans := logs.Len(), len(exporter.GetSpans())
	for range 2 {
		response := serveTestRequest(app, http.MethodGet, "/metrics")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "http_requests_total") {
			t.Fatalf("metrics = %d %q", response.Code, response.Body.String())
		}
		if response.Header().Get("X-Trace-ID") != "" {
			t.Fatal("metrics scrape received a request trace header")
		}
	}
	if logs.Len() != beforeLogs || len(exporter.GetSpans()) != beforeSpans {
		t.Fatal("metrics scrapes generated request logs or spans")
	}
	families, err := app.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "http_requests_total" && len(family.GetMetric()) != 1 {
			t.Fatalf("metrics scrape added request series: %v", family.GetMetric())
		}
	}
	if got := counterValue(t, app.metrics.registry, "http_requests_total", map[string]string{"method": "GET", "route": "/health", "status": "200"}); got != 1 {
		t.Fatalf("request counter = %v", got)
	}
}

func TestSlowDefaultAndCancellation(t *testing.T) {
	app, exporter := testAPI(t, nil)
	started := time.Now()
	response := serveTestRequest(app, http.MethodGet, "/slow")
	if response.Code != http.StatusOK || response.Body.String() != "slept for 2000 ms\n" || time.Since(started) < 1900*time.Millisecond {
		t.Fatalf("slow default = %d %q after %s", response.Code, response.Body.String(), time.Since(started))
	}
	var child, parent tracetest.SpanStub
	for _, span := range exporter.GetSpans() {
		switch span.Name {
		case "slow-op":
			child = span
		case "GET /slow":
			parent = span
		}
	}
	if !child.SpanContext.IsValid() || child.Parent.SpanID() != parent.SpanContext.SpanID() || child.SpanContext.TraceID() != parent.SpanContext.TraceID() {
		t.Fatal("slow-op is not a child of the slow HTTP span")
	}
	exporter.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response = httptest.NewRecorder()
	started = time.Now()
	app.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/slow", nil).WithContext(ctx))
	if response.Code != http.StatusRequestTimeout || time.Since(started) >= time.Second {
		t.Fatalf("canceled slow request = %d after %s", response.Code, time.Since(started))
	}
	for _, span := range exporter.GetSpans() {
		if span.Name == "slow-op" && span.Status.Code == codes.Error {
			return
		}
	}
	t.Fatal("canceled slow operation did not emit an Error child span")
}

func TestLoadUsesHTTPAndPropagatesTrace(t *testing.T) {
	for _, test := range []struct {
		name, target, route string
		succeeded, failed   int64
	}{
		{"health", "/load?n=12&concurrency=3", "/health", 12, 0},
		{"fail", "/load?n=12&concurrency=3&path=/fail", "/fail", 0, 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, exporter := testAPI(t, nil)
			server := httptest.NewServer(app.handler())
			defer server.Close()
			app.selfURL = server.URL
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
			app.handler().ServeHTTP(response, request)
			var result loadResult
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || result.Path != test.route || result.Requested != 12 || result.Attempted != 12 || result.Succeeded != test.succeeded || result.Failed != test.failed {
				t.Fatalf("load = status %d, result %+v", response.Code, result)
			}
			spans := exporter.GetSpans()
			clients := make(map[trace.SpanID]tracetest.SpanStub)
			var loadSpan tracetest.SpanStub
			for _, span := range spans {
				if span.SpanContext.TraceID().String() != "11111111111111111111111111111111" {
					t.Fatalf("self request lost incoming trace context: %s", span.Name)
				}
				if span.SpanKind == trace.SpanKindClient {
					clients[span.SpanContext.SpanID()] = span
				}
				if span.Name == "GET /load" {
					loadSpan = span
				}
			}
			var requests int
			for _, span := range spans {
				if span.SpanKind == trace.SpanKindServer && span.Name == "GET "+test.route {
					requests++
					client, found := clients[span.Parent.SpanID()]
					if !found || client.Parent.SpanID() != loadSpan.SpanContext.SpanID() {
						t.Fatal("self request is not a server child of a load client span")
					}
				}
			}
			if requests != 12 || len(clients) != 12 {
				t.Fatalf("self HTTP spans = %d server, %d client; want 12 each", requests, len(clients))
			}
			if got := counterValue(t, app.metrics.registry, "http_errors_total", map[string]string{"method": "GET", "route": test.route}); got != float64(test.failed) {
				t.Fatalf("self request error counter = %v", got)
			}
		})
	}
}

func TestLoadConcurrentGateAndCancellation(t *testing.T) {
	app, _ := testAPI(t, nil)
	started := make(chan struct{})
	var once sync.Once
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
	}))
	defer target.Close()
	app.selfURL = target.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/load?n=8&concurrency=2", nil).WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("load did not issue a real HTTP self request")
	}
	second := serveTestRequest(app, http.MethodGet, "/load?n=1")
	if second.Code != http.StatusServiceUnavailable || second.Header().Get("Retry-After") != "1" {
		t.Fatalf("concurrent batch = %d, Retry-After %q", second.Code, second.Header().Get("Retry-After"))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled batch did not finish")
	}
	var result loadResult
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if first.Code != http.StatusServiceUnavailable || result.Attempted < 1 || result.Attempted > 2 || result.Failed != result.Attempted || result.Succeeded != 0 || len(app.loadGate) != 0 {
		t.Fatalf("canceled batch = %d %+v, gate occupancy %d", first.Code, result, len(app.loadGate))
	}
}

func TestInvalidInputsAreBounded(t *testing.T) {
	app, _ := testAPI(t, nil)
	for _, target := range []string{
		"/slow?ms=999", "/slow?ms=3001", "/slow?ms=nope",
		"/load?n=0", "/load?n=1001", "/load?n=nope",
		"/load?concurrency=0", "/load?concurrency=51", "/load?concurrency=nope",
		"/load?path=/load", "/load?path=/metrics", "/load?path=https%3A%2F%2Fexample.com",
	} {
		if response := serveTestRequest(app, http.MethodGet, target); response.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", target, response.Code)
		}
	}
	if len(app.loadGate) != 0 {
		t.Fatal("invalid input acquired the load gate")
	}
}

func TestUnknownMethodsAndRoutesHaveBoundedMetricLabels(t *testing.T) {
	app, _ := testAPI(t, nil)
	serveTestRequest(app, "CUSTOM-ONE", "/unknown-one")
	serveTestRequest(app, "CUSTOM-TWO", "/unknown-two")
	if got := counterValue(t, app.metrics.registry, "http_requests_total", map[string]string{"method": "OTHER", "route": "unknown", "status": "404"}); got != 2 {
		t.Fatalf("bounded unknown request counter = %v", got)
	}
	families, err := app.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "http_requests_total" && len(family.GetMetric()) != 1 {
			t.Fatal("arbitrary methods or paths created additional request series")
		}
	}
}
