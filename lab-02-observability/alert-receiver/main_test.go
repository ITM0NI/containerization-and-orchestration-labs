package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const webhookFixture = `{
	"version": "4",
	"groupKey": "{}:{alertname=\"APIHighErrorRate\"}",
	"truncatedAlerts": 0,
	"status": "%s",
	"receiver": "lab-alert-receiver",
	"groupLabels": {"alertname": "APIHighErrorRate", "namespace": "lab"},
	"commonLabels": {"namespace": "lab"},
	"commonAnnotations": {"runbook_url": "https://example.invalid/runbook"},
	"externalURL": "http://alertmanager:9093",
	"notification_reason": "group_interval",
	"futureEnvelopeField": {"ignored": true},
	"alerts": [
		{
			"status": "%s",
			"labels": {"alertname": "APIHighErrorRate", "severity": "critical", "pod": "api-1"},
			"annotations": {"summary": "API error rate is high", "description": "API errors exceeded the threshold"},
			"startsAt": "2026-10-11T12:00:00Z",
			"endsAt": "%s",
			"generatorURL": "http://prometheus:9090/graph",
			"fingerprint": "abc123",
			"futureAlertField": "ignored"
		},
		{
			"status": "resolved",
			"labels": {"alertname": "APIHighErrorRate", "severity": "critical", "pod": "api-2"},
			"annotations": {"summary": "API error rate recovered"},
			"startsAt": "2026-10-11T11:00:00Z",
			"endsAt": "2026-10-11T11:30:00Z"
		}
	]
}`

func fixture(status string) string {
	endsAt := "0001-01-01T00:00:00Z"
	if status == "resolved" {
		endsAt = "2026-10-11T12:30:00Z"
	}
	return fmt.Sprintf(webhookFixture, status, status, endsAt)
}

func testHandler(output io.Writer) http.Handler {
	logger := slog.New(slog.NewJSONHandler(output, nil))
	return (&receiver{logger: logger}).handler()
}

func request(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, target, strings.NewReader(body)))
	return response
}

func TestHealth(t *testing.T) {
	var logs bytes.Buffer
	response := request(testHandler(&logs), http.MethodGet, "/health", "")
	if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
		t.Fatalf("health = %d %q", response.Code, response.Body.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("health emitted a log: %q", logs.String())
	}
}

func TestNotificationLogsFiringAndResolved(t *testing.T) {
	for _, status := range []string{"firing", "resolved"} {
		t.Run(status, func(t *testing.T) {
			var logs bytes.Buffer
			response := request(testHandler(&logs), http.MethodPost, "/alerts", fixture(status))
			if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
				t.Fatalf("notification = %d %q", response.Code, response.Body.String())
			}
			var record struct {
				Level              string            `json:"level"`
				Message            string            `json:"msg"`
				Status             string            `json:"status"`
				Receiver           string            `json:"receiver"`
				GroupLabels        map[string]string `json:"groupLabels"`
				CommonAnnotations  map[string]string `json:"commonAnnotations"`
				NotificationReason string            `json:"notification_reason"`
				Alerts             []struct {
					Status       string            `json:"status"`
					Labels       map[string]string `json:"labels"`
					Annotations  map[string]string `json:"annotations"`
					StartsAt     string            `json:"startsAt"`
					EndsAt       string            `json:"endsAt"`
					GeneratorURL string            `json:"generatorURL"`
					Fingerprint  string            `json:"fingerprint"`
				} `json:"alerts"`
			}
			decoder := json.NewDecoder(&logs)
			if err := decoder.Decode(&record); err != nil {
				t.Fatalf("decode JSON log: %v", err)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				t.Fatalf("expected one notification log, got %v", err)
			}
			if record.Level != "INFO" || record.Message != "alert notification received" ||
				record.Status != status || record.Receiver != "lab-alert-receiver" {
				t.Fatalf("notification log = %+v", record)
			}
			if record.GroupLabels["alertname"] != "APIHighErrorRate" ||
				record.GroupLabels["namespace"] != "lab" ||
				record.CommonAnnotations["runbook_url"] != "https://example.invalid/runbook" ||
				record.NotificationReason != "group_interval" {
				t.Fatalf("missing notification context: %+v", record)
			}
			if len(record.Alerts) != 2 {
				t.Fatalf("logged %d alerts, want 2", len(record.Alerts))
			}
			first := record.Alerts[0]
			if first.Status != status || first.Labels["alertname"] != "APIHighErrorRate" ||
				first.Labels["severity"] != "critical" || first.Labels["pod"] != "api-1" ||
				first.Annotations["summary"] != "API error rate is high" ||
				first.Annotations["description"] != "API errors exceeded the threshold" ||
				first.StartsAt != "2026-10-11T12:00:00Z" ||
				first.GeneratorURL != "http://prometheus:9090/graph" || first.Fingerprint != "abc123" {
				t.Fatalf("first alert lost its details: %+v", first)
			}
			wantEnd := "0001-01-01T00:00:00Z"
			if status == "resolved" {
				wantEnd = "2026-10-11T12:30:00Z"
			}
			if first.EndsAt != wantEnd {
				t.Fatalf("endsAt = %q, want %q", first.EndsAt, wantEnd)
			}
			second := record.Alerts[1]
			if second.Status != "resolved" || second.Labels["pod"] != "api-2" ||
				second.Annotations["summary"] != "API error rate recovered" || second.Fingerprint != "" {
				t.Fatalf("second alert lost its details: %+v", second)
			}
		})
	}
}

func TestInvalidRequestsAreNotLogged(t *testing.T) {
	valid := fixture("firing")
	tests := []struct {
		name   string
		method string
		target string
		body   string
		status int
	}{
		{"wrong alert method", http.MethodGet, "/alerts", valid, http.StatusMethodNotAllowed},
		{"wrong health method", http.MethodPost, "/health", valid, http.StatusMethodNotAllowed},
		{"empty body", http.MethodPost, "/alerts", "", http.StatusBadRequest},
		{"malformed JSON", http.MethodPost, "/alerts", "{raw-secret", http.StatusBadRequest},
		{"array instead of object", http.MethodPost, "/alerts", "[]", http.StatusBadRequest},
		{"null notification", http.MethodPost, "/alerts", "null", http.StatusBadRequest},
		{"missing notification fields", http.MethodPost, "/alerts", "{}", http.StatusBadRequest},
		{"unknown version", http.MethodPost, "/alerts", strings.Replace(valid, `"version": "4"`, `"version": "5"`, 1), http.StatusBadRequest},
		{"invalid group status", http.MethodPost, "/alerts", strings.Replace(valid, `"status": "firing"`, `"status": "unknown"`, 1), http.StatusBadRequest},
		{"invalid alert status", http.MethodPost, "/alerts", strings.Replace(valid, `"status": "resolved"`, `"status": "unknown"`, 1), http.StatusBadRequest},
		{"invalid timestamp", http.MethodPost, "/alerts", strings.Replace(valid, "2026-10-11T12:00:00Z", "not-a-time", 1), http.StatusBadRequest},
		{"second JSON value", http.MethodPost, "/alerts", valid + " {}", http.StatusBadRequest},
		{"trailing garbage", http.MethodPost, "/alerts", valid + " raw-secret", http.StatusBadRequest},
		{"oversized JSON", http.MethodPost, "/alerts", valid + strings.Repeat(" ", maxBodyBytes), http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			response := request(testHandler(&logs), test.method, test.target, test.body)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, test.status, response.Body.String())
			}
			if logs.Len() != 0 {
				t.Fatalf("rejected request emitted a log: %q", logs.String())
			}
			if strings.Contains(response.Body.String(), "raw-secret") {
				t.Fatalf("error exposed request content: %q", response.Body.String())
			}
		})
	}
}

func TestBodyLimitIncludesTrailingWhitespace(t *testing.T) {
	for _, extraByte := range []bool{false, true} {
		t.Run(fmt.Sprintf("extra_byte_%t", extraByte), func(t *testing.T) {
			valid := fixture("firing")
			padding := maxBodyBytes - len(valid)
			if extraByte {
				padding++
			}
			var logs bytes.Buffer
			response := request(testHandler(&logs), http.MethodPost, "/alerts", valid+strings.Repeat(" ", padding))
			wantStatus := http.StatusOK
			if extraByte {
				wantStatus = http.StatusRequestEntityTooLarge
			}
			if response.Code != wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, wantStatus)
			}
			if extraByte && logs.Len() != 0 {
				t.Fatal("oversized notification was logged")
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("log sink unavailable")
}

func TestLogFailureIsNotAcknowledged(t *testing.T) {
	response := request(testHandler(failingWriter{}), http.MethodPost, "/alerts", fixture("firing"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}
