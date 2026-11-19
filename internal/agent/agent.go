// Package agent implements the Strata node agent.
//
// In milestone M1 the agent only discovers the node's hardware and
// publishes it as a JSON report in its state directory. It has no network
// interface: remote access is introduced together with authentication in a
// later milestone.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

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
	// Logger receives structured logs. A nil Logger discards logs.
	Logger *slog.Logger
}

// Run performs an initial discovery, then repeats discovery on every
// reload signal until ctx is canceled.
//
// A failure of the initial discovery is returned, so that the service
// manager can restart the agent. Failures on reload are logged and the
// previous report is kept.
func Run(ctx context.Context, opts Options) error {
	if opts.StateDir == "" {
		return errors.New("agent: state directory not set")
	}
	if opts.Root == nil {
		return errors.New("agent: root filesystem not set")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	if err := refresh(opts, log); err != nil {
		return err
	}
	log.InfoContext(ctx, "agent started", "stateDir", opts.StateDir)

	for {
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "agent stopping")
			return nil
		case <-opts.Reload:
			log.InfoContext(ctx, "reload requested")
			if err := refresh(opts, log); err != nil {
				log.ErrorContext(ctx, "hardware discovery failed; keeping previous report", "error", err)
			}
		}
	}
}

func refresh(opts Options, log *slog.Logger) error {
	report, err := hardware.Discover(opts.Root)
	if err != nil {
		return fmt.Errorf("agent: discover hardware: %w", err)
	}
	path := filepath.Join(opts.StateDir, HardwareReportFile)
	if err := WriteJSON(path, report); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	log.Info("hardware report written",
		"path", path,
		"cpus", report.CPU.LogicalCount,
		"memoryBytes", report.Memory.TotalBytes,
		"virtualization", report.Virtualization.Extension,
		"kvmDevice", report.Virtualization.KVMDevice,
	)
	return nil
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
