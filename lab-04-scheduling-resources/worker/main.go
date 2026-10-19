package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const schema = `
CREATE TABLE IF NOT EXISTS orders (
    id           BIGSERIAL PRIMARY KEY,
    item         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed    BOOLEAN NOT NULL DEFAULT FALSE,
    processed_at TIMESTAMPTZ
);`

const address = ":8080"

var (
	ordersProcessed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "shop_orders_processed_total",
		Help: "Orders marked processed by worker.",
	})
	polls = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "shop_worker_polls_total",
		Help: "Successful order polls, including polls that found nothing.",
	})
)

func init() {
	prometheus.MustRegister(ordersProcessed, polls)
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

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := processOrders(ctx, db); err != nil && ctx.Err() == nil {
				log.Printf("process orders: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.Handle("GET /metrics", promhttp.Handler())

	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()

	log.Printf("worker is listening on %s", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func processOrders(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		UPDATE orders
		SET processed = TRUE, processed_at = now()
		WHERE processed = FALSE
		RETURNING id, item
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	polls.Inc()

	for rows.Next() {
		var id int64
		var item string
		if err := rows.Scan(&id, &item); err != nil {
			return err
		}
		ordersProcessed.Inc()
		log.Printf("processed order id=%d item=%q", id, item)
	}
	return rows.Err()
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
