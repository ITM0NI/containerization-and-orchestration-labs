package main

import (
	"bytes"
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
	"syscall"
	"time"
)

const (
	defaultAddress = ":8080"
	maxBodyBytes   = 1 << 20
)

type notification struct {
	Version            string            `json:"version"`
	GroupKey           string            `json:"groupKey"`
	TruncatedAlerts    uint64            `json:"truncatedAlerts"`
	Status             string            `json:"status"`
	Receiver           string            `json:"receiver"`
	GroupLabels        map[string]string `json:"groupLabels"`
	CommonLabels       map[string]string `json:"commonLabels"`
	CommonAnnotations  map[string]string `json:"commonAnnotations"`
	ExternalURL        string            `json:"externalURL"`
	NotificationReason string            `json:"notification_reason,omitempty"`
	Alerts             []alert           `json:"alerts"`
}

type alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
	Fingerprint  string            `json:"fingerprint,omitempty"`
}

type receiver struct {
	logger *slog.Logger
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("alert receiver stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	address := os.Getenv("HTTP_ADDR")
	if address == "" {
		address = defaultAddress
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	app := &receiver{logger: logger}
	server := &http.Server{
		Handler:           app.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()

	logger.Info("alert receiver is listening", "address", listener.Addr().String())
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		logger.Info("alert receiver is shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

func (a *receiver) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("POST /alerts", a.receive)
	return mux
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func (a *receiver) receive(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			http.Error(w, "notification exceeds 1 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "could not read notification", http.StatusBadRequest)
		}
		return
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	var message notification
	if err := decoder.Decode(&message); err != nil {
		http.Error(w, "invalid notification JSON", http.StatusBadRequest)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		http.Error(w, "expected one JSON notification", http.StatusBadRequest)
		return
	}
	if !message.valid() {
		http.Error(w, "expected an Alertmanager version 4 notification", http.StatusBadRequest)
		return
	}

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "alert notification received", 0)
	record.AddAttrs(
		slog.String("version", message.Version),
		slog.String("status", message.Status),
		slog.String("receiver", message.Receiver),
		slog.Any("groupLabels", message.GroupLabels),
		slog.String("groupKey", message.GroupKey),
		slog.Uint64("truncatedAlerts", message.TruncatedAlerts),
		slog.Any("commonLabels", message.CommonLabels),
		slog.Any("commonAnnotations", message.CommonAnnotations),
		slog.String("externalURL", message.ExternalURL),
		slog.String("notification_reason", message.NotificationReason),
		slog.Any("alerts", message.Alerts),
	)
	// Handle returns sink errors, so a failed log write is not acknowledged.
	if err := a.logger.Handler().Handle(r.Context(), record); err != nil {
		http.Error(w, "notification could not be logged", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func (n notification) valid() bool {
	if n.Version != "4" || !validStatus(n.Status) || n.Receiver == "" || len(n.Alerts) == 0 {
		return false
	}
	for _, alert := range n.Alerts {
		if !validStatus(alert.Status) {
			return false
		}
	}
	return true
}

func validStatus(status string) bool {
	return status == "firing" || status == "resolved"
}
