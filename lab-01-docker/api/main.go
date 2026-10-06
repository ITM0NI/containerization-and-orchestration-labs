package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const address = ":8080"

var (
	allocatedMemoryMu sync.Mutex
	allocatedMemory   [][]byte
	burnOnce          sync.Once
	burnCounter       atomic.Uint64
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /eat", eatMemory)
	mux.HandleFunc("GET /burn", burnCPU)

	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()

	log.Printf("api is listening on %s", address)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func eatMemory(w http.ResponseWriter, r *http.Request) {
	mb, err := strconv.ParseInt(r.URL.Query().Get("mb"), 10, 32)
	if err != nil || mb <= 0 {
		http.Error(w, "query parameter mb must be a positive integer", http.StatusBadRequest)
		return
	}

	block := make([]byte, int(mb)*1024*1024)
	for i := 0; i < len(block); i += os.Getpagesize() {
		block[i] = 1
	}

	allocatedMemoryMu.Lock()
	allocatedMemory = append(allocatedMemory, block)
	blocks := len(allocatedMemory)
	allocatedMemoryMu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "allocated and retained %d MiB (blocks retained: %d)\n", mb, blocks)
}

func burnCPU(w http.ResponseWriter, _ *http.Request) {
	started := false
	burnOnce.Do(func() {
		started = true
		go func() {
			for {
				burnCounter.Add(1)
			}
		}()
	})

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if started {
		fmt.Fprintln(w, "started burning one CPU core")
		return
	}
	fmt.Fprintln(w, "CPU burner is already running")
}
