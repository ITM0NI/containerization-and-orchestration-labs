package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultAddress   = ":8080"
	loadBatchTimeout = 30 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("api stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider, err := setupTracing(ctx, logger)
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := provider.Shutdown(shutdownCtx); err != nil {
			logger.Error("tracing shutdown failed", "error", err)
		}
	}()

	listener, err := net.Listen("tcp", envOrDefault("HTTP_ADDR", defaultAddress))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	app := newAPI(logger, provider, loopbackURL(listener.Addr().(*net.TCPAddr)))
	defer app.client.CloseIdleConnections()

	server := &http.Server{
		Handler:           app.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      40 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()

	logger.Info("api is listening", "address", listener.Addr().String())
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		logger.Info("api is shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func (a *api) fail(w http.ResponseWriter, r *http.Request) {
	err := errors.New("intentional failure")
	span := trace.SpanFromContext(r.Context())
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func (a *api) slow(w http.ResponseWriter, r *http.Request) {
	milliseconds, err := intParameter(r, "ms", 2000, 1000, 3000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.slowOperation(r.Context(), time.Duration(milliseconds)*time.Millisecond); err != nil {
		http.Error(w, "request canceled", http.StatusRequestTimeout)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "slept for %d ms\n", milliseconds)
}

func (a *api) slowOperation(ctx context.Context, duration time.Duration) error {
	ctx, span := a.tracer.Start(ctx, "slow-op",
		trace.WithAttributes(attribute.Int64("sleep.ms", duration.Milliseconds())))
	defer span.End()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		span.RecordError(ctx.Err())
		span.SetStatus(codes.Error, ctx.Err().Error())
		return ctx.Err()
	}
}

type loadResult struct {
	Path      string `json:"path"`
	Requested int    `json:"requested"`
	Attempted int64  `json:"attempted"`
	Succeeded int64  `json:"succeeded"`
	Failed    int64  `json:"failed"`
}

func (a *api) load(w http.ResponseWriter, r *http.Request) {
	requests, err := intParameter(r, "n", 100, 1, 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	concurrency, err := intParameter(r, "concurrency", 10, 1, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/health"
	}
	switch path {
	case "/health", "/fail", "/slow":
	default:
		http.Error(w, "path must be /health, /fail, or /slow", http.StatusBadRequest)
		return
	}

	// Bound traffic: one batch at a time, no recursive or external targets.
	select {
	case a.loadGate <- struct{}{}:
		defer func() { <-a.loadGate }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "another load batch is running", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loadBatchTimeout)
	defer cancel()
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("load.path", path),
		attribute.Int("load.requests", requests),
		attribute.Int("load.concurrency", concurrency),
	)

	jobs := make(chan struct{}, requests)
	for range requests {
		jobs <- struct{}{}
	}
	close(jobs)

	var attempted, succeeded, failed atomic.Int64
	var workers sync.WaitGroup
	for range min(requests, concurrency) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range jobs {
				if ctx.Err() != nil {
					return
				}
				attempted.Add(1)
				if err := a.selfRequest(ctx, path); err != nil {
					failed.Add(1)
				} else {
					succeeded.Add(1)
				}
			}
		}()
	}
	workers.Wait()
	result := loadResult{
		Path: path, Requested: requests,
		Attempted: attempted.Load(), Succeeded: succeeded.Load(), Failed: failed.Load(),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := ctx.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (a *api) selfRequest(ctx context.Context, path string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.selfURL+path, nil)
	if err != nil {
		return err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("self request returned HTTP %d", response.StatusCode)
	}
	return nil
}

func intParameter(r *http.Request, name string, fallback, minimum, maximum int) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < minimum || number > maximum {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return number, nil
}

func loopbackURL(address *net.TCPAddr) string {
	host := address.IP.String()
	if address.IP.IsUnspecified() {
		if address.IP.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(address.Port))
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
