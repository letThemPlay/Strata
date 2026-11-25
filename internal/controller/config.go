package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"
)

// Config is the controller's configuration, written by the NixOS module
// (strata.gitops) as JSON.
type Config struct {
	// Node is this host's node ID; the IR is
	// nixosConfigurations.<Node>.config.strata.ir.
	Node       string `json:"node"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	// IntervalSeconds is how often the repository is synchronized.
	IntervalSeconds int `json:"intervalSeconds"`
	// AutoApply is "none" or "safe": apply plans without restarts,
	// deletions, restores and unreconcilable items without approval.
	AutoApply string `json:"autoApply"`
	// TrustedSigners are SSH public keys ("TYPE KEY [NAME]") whose
	// signatures make a commit trusted.
	TrustedSigners []string `json:"trustedSigners"`
	// KnownHostsFile pins the SSH host keys of the repository's server.
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
	// SSHKey and Token name credentials (systemd LoadCredential) in the
	// credentials directory; empty if unused.
	SSHKey string `json:"sshKey,omitempty"`
	Token  string `json:"token,omitempty"`
	// EvalTimeoutSeconds bounds one Nix evaluation.
	EvalTimeoutSeconds int `json:"evalTimeoutSeconds"`
	// KeepDeployments is how many finished deployment records to keep; 0
	// keeps all.
	KeepDeployments int `json:"keepDeployments,omitempty"`
}

var (
	nodePattern       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	credentialPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// LoadConfig reads and checks a configuration file. Unknown fields are
// rejected.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The configuration file named by the service.
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return c, c.Check()
}

// Check validates the configuration.
func (c Config) Check() error {
	var errs []error
	if !nodePattern.MatchString(c.Node) {
		errs = append(errs, fmt.Errorf("node %q is not a valid node ID", c.Node))
	}
	if c.IntervalSeconds < 10 {
		errs = append(errs, errors.New("intervalSeconds: at least 10"))
	}
	if c.AutoApply != "none" && c.AutoApply != "safe" {
		errs = append(errs, fmt.Errorf("autoApply: %q is not none or safe", c.AutoApply))
	}
	if len(c.TrustedSigners) == 0 {
		errs = append(errs, errors.New("trustedSigners: at least one key is required"))
	}
	for _, n := range []string{c.SSHKey, c.Token} {
		if n != "" && !credentialPattern.MatchString(n) {
			errs = append(errs, fmt.Errorf("credential name %q is not valid", n))
		}
	}
	if c.EvalTimeoutSeconds < 1 {
		errs = append(errs, errors.New("evalTimeoutSeconds: at least 1"))
	}
	if c.KeepDeployments < 0 {
		errs = append(errs, errors.New("keepDeployments: must not be negative"))
	}
	return errors.Join(errs...)
}

// Interval returns the synchronization interval.
func (c Config) Interval() time.Duration { return time.Duration(c.IntervalSeconds) * time.Second }
