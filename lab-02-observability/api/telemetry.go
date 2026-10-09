package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type api struct {
	logger   *slog.Logger
	metrics  *httpMetrics
	tracer   trace.Tracer
	provider trace.TracerProvider
	client   *http.Client
	selfURL  string
	loadGate chan struct{}
}

type httpMetrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newAPI(logger *slog.Logger, provider trace.TracerProvider, selfURL string) *api {
	registry := prometheus.NewRegistry()
	metrics := &httpMetrics{
		registry: registry,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "Total completed HTTP requests, excluding /metrics.",
		}, []string{"method", "route", "status"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_errors_total", Help: "Total HTTP requests completed with a 5xx status.",
		}, []string{"method", "route"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP request duration in seconds, excluding /metrics.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 3, 5, 10, 30},
		}, []string{"method", "route", "status"}),
	}
	registry.MustRegister(metrics.requests, metrics.errors, metrics.duration,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	for _, route := range []string{"/health", "/fail", "/slow", "/load"} {
		metrics.errors.WithLabelValues(http.MethodGet, route).Add(0)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 50
	transport.MaxConnsPerHost = 50
	client := &http.Client{
		Transport: otelhttp.NewTransport(transport,
			otelhttp.WithTracerProvider(provider), otelhttp.WithPropagators(propagators())),
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &api{
		logger: logger, metrics: metrics, tracer: provider.Tracer("lab2-api"),
		provider: provider, client: client, selfURL: selfURL, loadGate: make(chan struct{}, 1),
	}
}

func (a *api) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /fail", a.fail)
	mux.HandleFunc("GET /slow", a.slow)
	mux.HandleFunc("GET /load", a.load)
	mux.Handle("GET /metrics", promhttp.HandlerFor(a.metrics.registry, promhttp.HandlerOpts{}))

	// Tracing is outside observation so JSON logs see the current server span.
	return otelhttp.NewHandler(a.observe(mux), "api",
		otelhttp.WithTracerProvider(a.provider),
		otelhttp.WithPropagators(propagators()),
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/metrics" }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return metricMethod(r.Method) + " " + metricRoute(r.URL.Path)
		}),
	)
}

func (a *api) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		response := &statusWriter{ResponseWriter: w}
		span := trace.SpanFromContext(r.Context())
		spanContext := span.SpanContext()
		w.Header().Set("X-Trace-ID", spanContext.TraceID().String())
		defer func() {
			if recovered := recover(); recovered != nil {
				err := fmt.Errorf("handler panic: %v", recovered)
				span.RecordError(err)
				span.SetStatus(codes.Error, "handler panic")
				http.Error(response, "internal server error", http.StatusInternalServerError)
			}
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}
			method, route := metricMethod(r.Method), metricRoute(r.URL.Path)
			duration := time.Since(started)
			a.metrics.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
			a.metrics.duration.WithLabelValues(method, route, strconv.Itoa(status)).Observe(duration.Seconds())
			level, message := slog.LevelInfo, "request completed"
			if status >= 500 {
				a.metrics.errors.WithLabelValues(method, route).Inc()
				level, message = slog.LevelError, "request failed"
			}
			a.logger.Log(r.Context(), level, message,
				"method", method, "route", route, "status", status,
				"duration_ms", float64(duration)/float64(time.Millisecond),
				"trace_id", spanContext.TraceID().String(), "span_id", spanContext.SpanID().String())
		}()
		next.ServeHTTP(response, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func metricRoute(path string) string {
	switch path {
	case "/health", "/fail", "/slow", "/load":
		return path
	default:
		return "unknown"
	}
}

func metricMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return method
	default:
		return "OTHER"
	}
}

func propagators() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

func setupTracing(ctx context.Context, logger *slog.Logger) (*sdktrace.TracerProvider, error) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Error("telemetry export failed", "error", err)
	}))
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK(),
		resource.WithAttributes(attribute.String("service.name", envOrDefault("OTEL_SERVICE_NAME", "lab2-api"))))
	if err != nil {
		return nil, err
	}
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	}
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("OTLP endpoint must be an http(s) URL without query or fragment")
		}
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		options = append(options, sdktrace.WithBatcher(exporter))
		logger.Info("trace export enabled", "protocol", "http/protobuf")
	} else {
		logger.Info("trace export disabled until an OTLP endpoint is configured")
	}
	// Keep valid trace IDs before installing an external trace backend.
	return sdktrace.NewTracerProvider(options...), nil
}
