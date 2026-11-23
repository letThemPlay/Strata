package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/letthemplay/Strata/internal/incus"
)

// BackendStatusFile is the name of the workload backend status within the
// state directory.
const BackendStatusFile = "backend.json"

// BackendStatusAPIVersion identifies the schema of BackendStatus.
const BackendStatusAPIVersion = "backend.strata.dev/v1alpha1"

// BackendStatus reports whether the agent can use the workload backend.
type BackendStatus struct {
	APIVersion string `json:"apiVersion"`
	// Backend names the workload backend, e.g. "incus".
	Backend string `json:"backend"`
	// Reachable reports whether the backend answered.
	Reachable bool `json:"reachable"`
	// Trusted reports whether the backend accepted the agent's identity.
	Trusted bool `json:"trusted"`
	// Version is the backend's version, if it revealed it.
	Version string `json:"version,omitempty"`
	// Error describes why the backend is unreachable or untrusted.
	Error string `json:"error,omitempty"`
}

// BackendCheck checks the workload backend. It reports failures in the
// returned status instead of returning an error.
type BackendCheck func(ctx context.Context) BackendStatus

// IncusBackend returns a BackendCheck for Incus. The identity files are
// loaded on every check, so a replaced identity takes effect without a
// restart.
func IncusBackend(cfg incus.Config) BackendCheck {
	return func(ctx context.Context) BackendStatus {
		st := BackendStatus{APIVersion: BackendStatusAPIVersion, Backend: "incus"}
		c, err := incus.New(cfg)
		if err != nil {
			st.Error = err.Error()
			return st
		}
		defer c.Close()
		s, err := c.Server(ctx)
		if err != nil {
			st.Error = err.Error()
			return st
		}
		st.Reachable, st.Trusted, st.Version = true, s.Trusted, s.Version
		if !s.Trusted {
			st.Error = "Incus does not trust the agent's identity"
		}
		return st
	}
}

// checkBackend runs the backend check and writes its status. It logs when
// the status differs from prev (or prev is nil): at info level when the
// backend is usable, at warning level otherwise.
func checkBackend(ctx context.Context, opts Options, log *slog.Logger, prev *BackendStatus) *BackendStatus {
	st := opts.Backend(ctx)
	path := filepath.Join(opts.StateDir, BackendStatusFile)
	if err := WriteJSON(path, st); err != nil {
		log.ErrorContext(ctx, "cannot write backend status", "error", err)
	}
	if prev != nil && *prev == st {
		return &st
	}
	attrs := []any{"backend", st.Backend, "reachable", st.Reachable, "trusted", st.Trusted, "version", st.Version}
	if st.Reachable && st.Trusted {
		log.InfoContext(ctx, "workload backend available", attrs...)
	} else {
		log.WarnContext(ctx, "workload backend unavailable", append(attrs, "error", st.Error)...)
	}
	return &st
}

// removeBackendStatus removes a status file left by an earlier
// configuration with a backend.
func removeBackendStatus(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, BackendStatusFile))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("agent: remove stale backend status: %w", err)
	}
	return nil
}
