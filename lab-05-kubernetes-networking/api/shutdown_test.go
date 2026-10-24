package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestShutdownCompletesActiveRequest(t *testing.T) {
	if os.Getenv("LAB5_API_TEST_PROCESS") == "1" {
		main()
		return
	}

	// Отдельный процесс проверяет возврат из main, а не только поведение http.Server.
	cmd := exec.Command(os.Args[0], "-test.run=^TestShutdownCompletesActiveRequest$")
	cmd.Env = append(os.Environ(), "LAB5_API_TEST_PROCESS=1",
		"DATABASE_URL=postgres://shop:shop@127.0.0.1:1/shop?sslmode=disable")
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-done:
			t.Fatalf("API exited before readiness: %v\n%s", err, logs.String())
		default:
		}
		response, err := http.Get("http://127.0.0.1:8080/health")
		if err == nil {
			response.Body.Close()
			ready = response.StatusCode == http.StatusOK
			if ready {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("API did not become ready")
	}

	conn, err := net.DialTimeout("tcp", "127.0.0.1:8080", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	body := `{"item":"shutdown-test"}`
	fmt.Fprintf(conn, "POST /order HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", len(body))
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != http.StatusContinue {
		t.Fatalf("handler did not start reading the body: response=%v, error=%v", response, err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("API exited before the active request completed: %v\n%s", err, logs.String())
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := fmt.Fprint(conn, body); err != nil {
		t.Fatal(err)
	}
	response, err = http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("active request lost its HTTP response: %v", err)
	}
	response.Body.Close()
	// База намеренно недоступна: проверяется завершённый HTTP-ответ, не запись заказа.
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("unexpected response: %s", response.Status)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("API shutdown failed: %v\n%s", err, logs.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("API did not finish shutdown")
	}
}
