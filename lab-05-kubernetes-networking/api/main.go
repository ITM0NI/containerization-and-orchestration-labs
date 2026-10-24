package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const address = ":8080"

var (
	ordersCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "shop_orders_created_total",
		Help: "Orders stored by api.",
	})
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shop_http_requests_total",
		Help: "HTTP responses by path and status.",
	}, []string{"path", "code"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "shop_http_request_duration_seconds",
		Help:    "HTTP request duration.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"path"})
)

func init() {
	prometheus.MustRegister(ordersCreated, httpRequests, httpDuration)
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func observe(path string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next(sw, r)
		httpRequests.WithLabelValues(path, strconv.Itoa(sw.code)).Inc()
		httpDuration.WithLabelValues(path).Observe(time.Since(start).Seconds())
	}
}

const schema = `
CREATE TABLE IF NOT EXISTS orders (
    id           BIGSERIAL PRIMARY KEY,
    item         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed    BOOLEAN NOT NULL DEFAULT FALSE,
    processed_at TIMESTAMPTZ
);`

type order struct {
	ID          int64      `json:"id"`
	Item        string     `json:"item"`
	CreatedAt   time.Time  `json:"created_at"`
	Processed   bool       `json:"processed"`
	ProcessedAt *time.Time `json:"processed_at"`
}

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	db, err := openDB(databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		for ctx.Err() == nil {
			if err := ensureSchema(ctx, db); err == nil {
				log.Print("database schema is ready")
				return
			} else if ctx.Err() == nil {
				log.Printf("database is not ready: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", observe("/health", health))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /order", observe("/order", createOrder(db)))
	mux.HandleFunc("GET /orders", observe("/orders", listOrders(db)))

	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()

	log.Printf("api is listening on %s", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}

func health(w http.ResponseWriter, _ *http.Request) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("HEALTH_FAIL")), "true") {
		http.Error(w, "health check forced to fail", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func createOrder(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Item string `json:"item"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "body must be JSON with an item field", http.StatusBadRequest)
			return
		}
		body.Item = strings.TrimSpace(body.Item)
		if body.Item == "" {
			http.Error(w, "item is required", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		var created order
		err := db.QueryRowContext(ctx, `
			INSERT INTO orders (item)
			VALUES ($1)
			RETURNING id, item, created_at, processed, processed_at
		`, body.Item).Scan(&created.ID, &created.Item, &created.CreatedAt, &created.Processed, &created.ProcessedAt)
		if err != nil {
			log.Printf("insert order: %v", err)
			http.Error(w, "failed to store order", http.StatusInternalServerError)
			return
		}
		ordersCreated.Inc()
		writeJSON(w, http.StatusCreated, created)
	}
}

func listOrders(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		rows, err := db.QueryContext(ctx, `
			SELECT id, item, created_at, processed, processed_at
			FROM orders
			ORDER BY id
		`)
		if err != nil {
			log.Printf("list orders: %v", err)
			http.Error(w, "failed to read orders", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		orders := make([]order, 0)
		for rows.Next() {
			var item order
			if err := rows.Scan(&item.ID, &item.Item, &item.CreatedAt, &item.Processed, &item.ProcessedAt); err != nil {
				log.Printf("scan order: %v", err)
				http.Error(w, "failed to read orders", http.StatusInternalServerError)
				return
			}
			orders = append(orders, item)
		}
		if err := rows.Err(); err != nil {
			log.Printf("iterate orders: %v", err)
			http.Error(w, "failed to read orders", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, orders)
	}
}

func openDB(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

func ensureSchema(ctx context.Context, db *sql.DB) error {
	var err error
	for attempt := 1; attempt <= 10; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil {
			execCtx, execCancel := context.WithTimeout(ctx, 3*time.Second)
			_, err = db.ExecContext(execCtx, schema)
			execCancel()
			if err == nil {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
	}
	return fmt.Errorf("ensure schema: %w", err)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write json: %v", err)
	}
}
