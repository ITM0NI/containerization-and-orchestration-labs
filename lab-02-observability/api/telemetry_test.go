package main

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func tracingEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
		"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
		"OTEL_RESOURCE_ATTRIBUTES",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("OTEL_SERVICE_NAME", "lab2-api-test")
	previousHandler := otel.GetErrorHandler()
	t.Cleanup(func() { otel.SetErrorHandler(previousHandler) })
}

func TestSetupTracingExportsHTTPProtobuf(t *testing.T) {
	for _, test := range []struct {
		name, genericPath, tracePath, expectedPath string
	}{
		{"generic endpoint appends trace path", "", "", "/v1/traces"},
		{"generic endpoint preserves base path", "/collector", "", "/collector/v1/traces"},
		{"trace endpoint takes precedence", "/unused", "/custom/traces", "/custom/traces"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracingEnvironment(t)
			type capturedExport struct {
				path, contentType string
				body              *collectortrace.ExportTraceServiceRequest
				err               error
			}
			captured := make(chan capturedExport, 8)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				request := new(collectortrace.ExportTraceServiceRequest)
				if err == nil {
					err = proto.Unmarshal(body, request)
				}
				captured <- capturedExport{r.URL.Path, r.Header.Get("Content-Type"), request, err}
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.WriteHeader(http.StatusOK)
			}))
			defer receiver.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL+test.genericPath)
			if test.tracePath != "" {
				t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", receiver.URL+test.tracePath)
			}
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			provider, err := setupTracing(context.Background(), logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := provider.Shutdown(ctx); err != nil {
					t.Errorf("shutdown tracing: %v", err)
				}
			})
			app := newAPI(logger, provider, "")
			t.Cleanup(app.client.CloseIdleConnections)
			response := serveTestRequest(app, http.MethodGet, "/fail")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := provider.ForceFlush(ctx); err != nil {
				t.Fatalf("flush trace: %v", err)
			}
			var export capturedExport
			select {
			case export = <-captured:
			default:
				t.Fatal("ForceFlush did not send traces to the HTTP receiver")
			}
			if export.err != nil || export.path != test.expectedPath || export.contentType != "application/x-protobuf" {
				t.Fatalf("OTLP request = path %q, content type %q, decode error %v", export.path, export.contentType, export.err)
			}
			var foundSpan, foundService bool
			for _, resource := range export.body.GetResourceSpans() {
				for _, attribute := range resource.GetResource().GetAttributes() {
					if attribute.GetKey() == "service.name" && attribute.GetValue().GetStringValue() == "lab2-api-test" {
						foundService = true
					}
				}
				for _, scope := range resource.GetScopeSpans() {
					for _, span := range scope.GetSpans() {
						if span.GetName() == "GET /fail" {
							foundSpan = true
							if span.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR || hex.EncodeToString(span.GetTraceId()) != response.Header().Get("X-Trace-ID") || len(span.GetEvents()) == 0 {
								t.Fatalf("exported failure lost Error status, trace correlation, or exception: %v", span)
							}
						}
					}
				}
			}
			if !foundSpan || !foundService {
				t.Fatalf("OTLP payload missing failure span or configured service name: %v", export.body)
			}
		})
	}
}

func TestSetupTracingWithoutEndpointStillCreatesTraceIDs(t *testing.T) {
	tracingEnvironment(t)
	provider, err := setupTracing(context.Background(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Shutdown(context.Background())
	_, span := provider.Tracer("test").Start(context.Background(), "without-exporter")
	if !span.SpanContext().IsValid() || !span.SpanContext().IsSampled() {
		t.Fatal("disabled export must still provide valid sampled trace IDs for logs")
	}
	span.End()
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush without exporter: %v", err)
	}
}

func TestSetupTracingRejectsInvalidEndpoint(t *testing.T) {
	tracingEnvironment(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	for _, endpoint := range []string{
		"collector:4318", "ftp://collector:4318", "http://[",
		"http://collector:4318/v1/traces?token=value", "http://collector:4318/#fragment",
	} {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
		provider, err := setupTracing(context.Background(), logger)
		if err == nil {
			if provider != nil {
				_ = provider.Shutdown(context.Background())
			}
			t.Errorf("endpoint %q accepted", endpoint)
		} else if provider != nil {
			t.Errorf("endpoint %q returned a provider with an error", endpoint)
		}
	}
}
