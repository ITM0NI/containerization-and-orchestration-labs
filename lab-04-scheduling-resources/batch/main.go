package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func memAvailableMiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

func main() {
	mb := 64
	if v := os.Getenv("MEMORY_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "MEMORY_MB: %v\n", err)
			os.Exit(1)
		}
		mb = n
	}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
		if err := http.ListenAndServe(":8080", mux); err != nil {
			fmt.Fprintf(os.Stderr, "listen: %v\n", err)
			os.Exit(1)
		}
	}()

	const step = 64
	var chunks [][]byte
	used := 0
	for used < mb {
		if avail := memAvailableMiB(); avail > 0 && avail <= 220 {
			fmt.Fprintf(os.Stderr, "hold used=%dMi available=%dMi\n", used, avail)
			break
		}
		n := step
		if used+n > mb {
			n = mb - used
		}
		chunk := make([]byte, n<<20)
		for i := 0; i < len(chunk); i += 4096 {
			chunk[i] = 1
		}
		chunks = append(chunks, chunk)
		used += n
		runtime.KeepAlive(chunks)
		time.Sleep(time.Second)
	}

	for {
		if len(chunks) > 0 {
			chunks[0][0]++
		}
		runtime.KeepAlive(chunks)
		time.Sleep(time.Second)
	}
}
