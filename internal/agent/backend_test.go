package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/letthemplay/Strata/internal/incus"
)

func readStatus(t *testing.T, path string) BackendStatus {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	var st BackendStatus
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// fakeBackend returns statuses set by the test and counts checks.
type fakeBackend struct {
	mu     sync.Mutex
	status BackendStatus
	checks int
}

func (f *fakeBackend) check(context.Context) BackendStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.status
}

func (f *fakeBackend) set(st BackendStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = st
}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

func TestRunChecksBackendEachCycle(t *testing.T) {
	dir := t.TempDir()
	usable := BackendStatus{APIVersion: BackendStatusAPIVersion, Backend: "incus", Reachable: true, Trusted: true, Version: "7.0.2"}
	fb := &fakeBackend{status: usable}
	var logs lockedBuffer
	reload := make(chan os.Signal, 1)
	stop := startAgent(t, Options{
		StateDir: dir, Root: hostFS(), Reload: reload, Backend: fb.check,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	path := filepath.Join(dir, BackendStatusFile)
	waitFor(t, func() bool { _, err := os.Stat(path); return err == nil })
	if st := readStatus(t, path); st != usable {
		t.Errorf("status: %+v", st)
	}

	// An unchanged status is not logged again.
	reload <- os.Interrupt
	waitFor(t, func() bool { return fb.count() >= 2 })

	// A change is written and logged as a warning.
	fb.set(BackendStatus{APIVersion: BackendStatusAPIVersion, Backend: "incus", Error: "connection refused"})
	reload <- os.Interrupt
	waitFor(t, func() bool { return !readStatus(t, path).Reachable })
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	out := logs.String()
	if n := strings.Count(out, `"msg":"workload backend available"`); n != 1 {
		t.Errorf("available logged %d times:\n%s", n, out)
	}
	if !strings.Contains(out, `"level":"WARN","msg":"workload backend unavailable"`) ||
		!strings.Contains(out, `"error":"connection refused"`) {
		t.Errorf("missing warning:\n%s", out)
	}
}

func TestRunWithoutBackendRemovesStaleStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BackendStatusFile)
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startAgent(t, Options{StateDir: dir, Root: hostFS()})
	waitFor(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestIncusBackendReportsConfigurationErrors(t *testing.T) {
	st := IncusBackend(incus.Config{Address: "192.0.2.1:8443", Project: "strata"})(context.Background())
	if st.Backend != "incus" || st.Reachable || st.Trusted || !strings.Contains(st.Error, "not a loopback") {
		t.Errorf("status: %+v", st)
	}
}
