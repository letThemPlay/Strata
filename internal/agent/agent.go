// Package agent implements the Strata node agent.
//
// The agent discovers the node's hardware and publishes it as a JSON report
// in its state directory. On a compute host it also checks that it can use
// the workload backend (Incus, through a restricted identity) and publishes
// that status. It has no network interface: remote access is introduced
// together with authentication in a later milestone.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/hardware"
)

// HardwareReportFile is the name of the hardware report within the state
// directory.
const HardwareReportFile = "hardware.json"

// Options configures Run.
type Options struct {
	// StateDir is the directory the agent writes its reports to. It must
	// already exist (systemd's StateDirectory= creates it).
	StateDir string
	// Root is the host filesystem used for discovery, normally os.DirFS("/").
	Root fs.FS
	// Reload triggers a new discovery cycle whenever a value is received
	// (typically SIGHUP). May be nil.
	Reload <-chan os.Signal
	// Interval is the period between automatic discovery cycles. Hardware
	// can change after the agent starts (late kernel module loading,
	// hotplug), so the report must not be a one-time snapshot. Zero
	// disables periodic discovery; negative values are invalid.
	Interval time.Duration
	// Logger receives structured logs. A nil Logger discards logs.
	Logger *slog.Logger
	// Backend checks the workload backend in every cycle. Nil means the
	// node has no workload backend.
	Backend BackendCheck
	// API, if set, is served on APIListener (the local API, ADR-0020).
	API         *api.Server
	APIListener net.Listener
}

// Run performs an initial discovery, then repeats discovery every Interval
// and on every reload signal until ctx is canceled.
//
// A failure of the initial discovery is returned, so that the service
// manager can restart the agent. Later failures are logged and the
// previous report is kept.
func Run(ctx context.Context, opts Options) error {
	if opts.StateDir == "" {
		return errors.New("agent: state directory not set")
	}
	if opts.Root == nil {
		return errors.New("agent: root filesystem not set")
	}
	if opts.Interval < 0 {
		return fmt.Errorf("agent: invalid discovery interval %s", opts.Interval)
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	last, err := refresh(ctx, opts, log, nil)
	if err != nil {
		return err
	}
	var backend *BackendStatus
	if opts.Backend != nil {
		backend = checkBackend(ctx, opts, log, nil)
	} else if err := removeBackendStatus(opts.StateDir); err != nil {
		return err
	}
	apiErr := make(chan error, 1)
	if opts.API != nil && opts.APIListener != nil {
		go func() { apiErr <- opts.API.Serve(ctx, opts.APIListener) }()
	}
	log.InfoContext(ctx, "agent started", "stateDir", opts.StateDir, "interval", opts.Interval.String(),
		"backend", opts.Backend != nil, "api", opts.APIListener != nil)

	var tick <-chan time.Time
	if opts.Interval > 0 {
		ticker := time.NewTicker(opts.Interval)
		defer ticker.Stop()
		tick = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "agent stopping")
			if opts.API != nil && opts.APIListener != nil {
				if err := <-apiErr; err != nil {
					return err
				}
			}
			return nil
		case err := <-apiErr:
			return fmt.Errorf("agent: %w", err)
		case <-opts.Reload:
			log.InfoContext(ctx, "reload requested")
		case <-tick:
		}
		if opts.Backend != nil {
			backend = checkBackend(ctx, opts, log, backend)
		}
		report, err := refresh(ctx, opts, log, last)
		if err != nil {
			log.ErrorContext(ctx, "hardware discovery failed; keeping previous report", "error", err)
			continue
		}
		last = report
	}
}

// refresh discovers hardware and writes the report. It logs at info level
// when the report differs from prev (or prev is nil) and at debug level
// otherwise, so periodic discovery does not flood the journal.
func refresh(ctx context.Context, opts Options, log *slog.Logger, prev *hardware.Report) (*hardware.Report, error) {
	report, err := hardware.Discover(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("agent: discover hardware: %w", err)
	}
	path := filepath.Join(opts.StateDir, HardwareReportFile)
	if err := WriteJSON(path, report); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}

	level := slog.LevelInfo
	msg := "hardware report written"
	if prev != nil {
		if reflect.DeepEqual(*prev, report) {
			level = slog.LevelDebug
			msg = "hardware report unchanged"
		} else {
			msg = "hardware report changed"
		}
	}
	log.Log(ctx, level, msg,
		"path", path,
		"cpus", report.CPU.LogicalCount,
		"memoryBytes", report.Memory.TotalBytes,
		"virtualization", report.Virtualization.Extension,
		"kvmDevice", report.Virtualization.KVMDevice,
	)
	return &report, nil
}

// WriteJSON atomically writes v as indented JSON to path with mode 0644.
// Readers never observe a partially written file.
func WriteJSON(path string, v any) (err error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
