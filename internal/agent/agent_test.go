package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/letthemplay/Strata/internal/hardware"
)

func hostFS() fstest.MapFS {
	return fstest.MapFS{
		"proc/cpuinfo": {Data: []byte("processor : 0\nvendor_id : AuthenticAMD\nflags : svm\n")},
		"proc/meminfo": {Data: []byte("MemTotal: 1024 kB\n")},
	}
}

// waitFor polls cond until it returns true or the deadline expires.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startAgent(t *testing.T, opts Options) (cancel func() error) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	return func() error {
		stop()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("agent did not stop after cancellation")
			return nil
		}
	}
}

func readReport(t *testing.T, path string) hardware.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var r hardware.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return r
}

func TestRunWritesReportAndStops(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stop := startAgent(t, Options{StateDir: dir, Root: hostFS()})

	path := filepath.Join(dir, HardwareReportFile)
	waitFor(t, func() bool { _, err := os.Stat(path); return err == nil })

	r := readReport(t, path)
	if r.APIVersion != hardware.APIVersion || r.CPU.LogicalCount != 1 ||
		r.Virtualization.Extension != hardware.ExtensionAMDV || r.Memory.TotalBytes != 1024*1024 {
		t.Errorf("unexpected report: %+v", r)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("report mode = %o, want 644", perm)
	}

	if err := stop(); err != nil {
		t.Errorf("Run returned %v after cancellation, want nil", err)
	}
	assertNoTempFiles(t, dir)
}

func TestRunReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reload := make(chan os.Signal, 1)
	stop := startAgent(t, Options{StateDir: dir, Root: hostFS(), Reload: reload})
	defer func() { _ = stop() }()

	path := filepath.Join(dir, HardwareReportFile)
	waitFor(t, func() bool { _, err := os.Stat(path); return err == nil })

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reload <- syscall.SIGHUP
	waitFor(t, func() bool { _, err := os.Stat(path); return err == nil })
}

func TestRunInitialDiscoveryFailure(t *testing.T) {
	t.Parallel()

	err := Run(context.Background(), Options{StateDir: t.TempDir(), Root: fstest.MapFS{}})
	if err == nil || !strings.Contains(err.Error(), "discover hardware") {
		t.Errorf("Run error = %v, want discovery error", err)
	}
}

func TestRunUnwritableStateDir(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := Run(context.Background(), Options{StateDir: missing, Root: hostFS()})
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Run error = %v, want not-exist error", err)
	}
}

func TestRunInvalidOptions(t *testing.T) {
	t.Parallel()

	for name, opts := range map[string]Options{
		"no state dir": {Root: hostFS()},
		"no root":      {StateDir: t.TempDir()},
	} {
		if err := Run(context.Background(), opts); err == nil {
			t.Errorf("%s: Run returned nil error", name)
		}
	}
}

func TestWriteJSONReplacesAtomically(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	for _, v := range []map[string]int{{"a": 1}, {"a": 2}} {
		if err := WriteJSON(path, v); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\n  \"a\": 2\n}\n" {
		t.Errorf("content = %q", data)
	}
	assertNoTempFiles(t, dir)
}

func TestWriteJSONEncodeError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := WriteJSON(filepath.Join(dir, "x.json"), make(chan int)); err == nil {
		t.Error("WriteJSON accepted an unencodable value")
	}
	assertNoTempFiles(t, dir)
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover temporary file %s", e.Name())
		}
	}
}
