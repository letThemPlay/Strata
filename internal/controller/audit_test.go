package controller

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The audit log records the request path, which the caller chooses. It is
// written by slog's JSON handler, which escapes control characters: a
// path with a newline and a forged record stays one record, with the
// path as a value (code scanning: go/log-injection).
func TestAuditLogCannotBeForged(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	s := &Server{Controller: &Controller{}, Log: logger}
	forged := "/api/v1/x\n{\"msg\":\"audit\",\"uid\":0,\"path\":\"/api/v1/forged\"}\r"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/x%0A%7B%22msg%22:%22audit%22,%22uid%22:0,%22path%22:%22/api/v1/forged%22%7D%0D", nil)
	if req.URL.Path != forged {
		t.Fatalf("path %q", req.URL.Path)
	}
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d log lines, want 1:\n%s", len(lines), logs.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("log line is not one JSON record: %v\n%s", err, lines[0])
	}
	if rec["msg"] != "audit" || rec["path"] != forged {
		t.Errorf("record %v", rec)
	}
}
