package incus

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serveUnix serves handler on a Unix socket and returns its path.
func serveUnix(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "unix.socket")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

func TestUnixAsyncOperation(t *testing.T) {
	var gotBody, gotQuery string
	socket := serveUnix(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/1.0/storage-pools":
			b, _ := io.ReadAll(r.Body)
			gotBody, gotQuery = string(b), r.URL.RawQuery
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"type":"async","status_code":100,"operation":"/1.0/operations/op1","metadata":{}}`))
		case r.URL.Path == "/1.0/operations/op1/wait":
			_, _ = w.Write([]byte(`{"type":"sync","metadata":{"status":"Success","err":""}}`))
		case r.URL.Path == "/1.0/operations/op2/wait":
			_, _ = w.Write([]byte(`{"type":"sync","metadata":{"status":"Failure","err":"Failed to create pool"}}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"type":"async","operation":"/1.0/operations/op2","metadata":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":"not found","error_code":404}`))
		}
	})
	c := NewUnix(socket, "", time.Second)
	defer c.Close()
	ctx := context.Background()

	if err := c.Do(ctx, http.MethodPost, "/1.0/storage-pools", map[string]string{"name": "p"}, nil); err != nil {
		t.Fatalf("async success: %v", err)
	}
	if gotBody != `{"name":"p"}` || gotQuery != "" {
		t.Errorf("body %q, query %q (no project for the server-wide client)", gotBody, gotQuery)
	}
	err := c.Do(ctx, http.MethodDelete, "/1.0/storage-pools/p", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "DELETE /1.0/storage-pools/p: operation failed: Failed to create pool") {
		t.Errorf("async failure: %v", err)
	}
	err = c.Do(ctx, http.MethodGet, "/1.0/projects/missing", nil, nil)
	if !IsNotFound(err) {
		t.Errorf("not found: %v", err)
	}
}
