package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// maxIRBytes bounds the evaluator's output (as ir.Parse does).
const maxIRBytes = 8 << 20

// ErrInputsMissing reports that the commit's flake inputs are not on the
// host: the evaluation has no network, so they must already be in the
// store.
var ErrInputsMissing = errors.New("the commit's flake inputs are not on this host")

// EvalError is a failed evaluation.
type EvalError struct {
	// Detail is Nix's error output, from its first "error:" line.
	Detail string
	// InputsMissing: Nix needed to download something.
	InputsMissing bool
}

func (e *EvalError) Error() string {
	if e.InputsMissing {
		return ErrInputsMissing.Error() + " (has flake.lock changed? apply the host configuration with nixos-rebuild first):\n" + e.Detail
	}
	return "evaluation failed:\n" + e.Detail
}

// Unwrap makes errors.Is(err, ErrInputsMissing) work.
func (e *EvalError) Unwrap() error {
	if e.InputsMissing {
		return ErrInputsMissing
	}
	return nil
}

// Evaluator evaluates the IR of a commit with Nix, offline, in pure mode
// and without import-from-derivation (ADR-0025). The service that runs it
// (strata-controller-eval) has no network, so evaluation cannot download
// anything whatever the configuration contains.
type Evaluator struct {
	// Nix is the nix binary (default "nix" from PATH).
	Nix string
	// Home is HOME for nix (its evaluation cache).
	Home    string
	Timeout time.Duration
}

var (
	ansi           = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	offlineFailure = regexp.MustCompile(`(?i)(offline mode|unable to download|cannot (be )?fetch|does not exist in the Nix store|not available in the store|Could not resolve host|Couldn't connect)`)
)

// IR evaluates nixosConfigurations.<node>.config.strata.ir of flakeURL
// and returns the JSON.
func (e *Evaluator) IR(ctx context.Context, flakeURL, node string) ([]byte, error) {
	if !nodePattern.MatchString(node) {
		return nil, fmt.Errorf("node %q is not a valid node ID", node)
	}
	timeout := e.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bin := e.Nix
	if bin == "" {
		bin = "nix"
	}
	attr := flakeURL + `#nixosConfigurations."` + node + `".config.strata.ir`
	cmd := exec.CommandContext(ctx, bin, //nolint:gosec // Fixed arguments; the flake URL and node are validated.
		"--extra-experimental-features", "nix-command flakes",
		"eval", "--json", "--offline", "--no-write-lock-file", "--quiet",
		"--option", "allow-import-from-derivation", "false",
		"--option", "pure-eval", "true",
		"--option", "substitute", "false",
		attr)
	// After the timeout, do not wait for children that keep the pipes open.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.Home,
		"XDG_CACHE_HOME=" + e.Home + "/.cache",
		"LC_ALL=C",
	}
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = maxIRBytes, 1<<20
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("evaluation timed out after %s", timeout)
	}
	if stdout.over {
		return nil, fmt.Errorf("evaluation output exceeds %d bytes", maxIRBytes)
	}
	if err != nil {
		detail := nixError(stderr.String())
		return nil, &EvalError{Detail: detail, InputsMissing: offlineFailure.MatchString(detail)}
	}
	return stdout.Bytes(), nil
}

// nixError returns the useful part of Nix's error output: from its first
// "error:" line (dropping fetch progress and warnings), without terminal
// codes, at most 40 lines.
func nixError(s string) string {
	s = ansi.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r", "\n")
	if i := strings.Index(s, "\nerror:"); i >= 0 {
		s = s[i+1:]
	}
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}

// limitedBuffer keeps at most limit bytes and records overflow.
type limitedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p) //nolint:wrapcheck // bytes.Buffer never fails.
}

// evalRequest and evalReply are the protocol of the evaluation service: one
// JSON request and one JSON reply per connection.
type evalRequest struct {
	Commit string `json:"commit"`
	Node   string `json:"node"`
}

type evalReply struct {
	IR            json.RawMessage `json:"ir,omitempty"`
	Error         string          `json:"error,omitempty"`
	Detail        string          `json:"detail,omitempty"`
	InputsMissing bool            `json:"inputsMissing,omitempty"`
}

// ServeEval answers one evaluation request on rw (the evaluation service,
// started by systemd per connection). It only evaluates commits of the
// controller's repository, repoDir.
func ServeEval(ctx context.Context, rw io.ReadWriter, e *Evaluator, repoDir string) error {
	var req evalRequest
	if err := json.NewDecoder(io.LimitReader(rw, 4096)).Decode(&req); err != nil {
		return fmt.Errorf("eval request: %w", err)
	}
	reply := evalReply{}
	if !shaPattern.MatchString(req.Commit) {
		reply.Error = fmt.Sprintf("commit %q: not a full commit ID", req.Commit)
	} else {
		data, err := e.IR(ctx, "git+file://"+repoDir+"?rev="+req.Commit, req.Node)
		var ee *EvalError
		switch {
		case errors.As(err, &ee):
			reply.Detail, reply.InputsMissing = ee.Detail, ee.InputsMissing
		case err != nil:
			reply.Error = err.Error()
		default:
			reply.IR = data
		}
	}
	if err := json.NewEncoder(rw).Encode(reply); err != nil {
		return fmt.Errorf("eval reply: %w", err)
	}
	return nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// RemoteEvaluator asks the evaluation service on Socket to evaluate a
// commit of the controller's repository.
type RemoteEvaluator struct {
	Socket  string
	Timeout time.Duration
}

// IR implements IREvaluator. flakeURL must be the repository's URL of a
// commit (gitops.Repo.FlakeURL); only the commit is sent.
func (r *RemoteEvaluator) IR(ctx context.Context, flakeURL, node string) ([]byte, error) {
	_, commit, ok := strings.Cut(flakeURL, "?rev=")
	if !ok || !shaPattern.MatchString(commit) {
		return nil, fmt.Errorf("flake URL %q has no commit", flakeURL)
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout+30*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", r.Socket)
	if err != nil {
		return nil, fmt.Errorf("evaluation service: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(evalRequest{Commit: commit, Node: node}); err != nil {
		return nil, fmt.Errorf("evaluation service: %w", err)
	}
	var reply evalReply
	if err := json.NewDecoder(io.LimitReader(bufio.NewReader(conn), maxIRBytes+1<<20)).Decode(&reply); err != nil {
		return nil, fmt.Errorf("evaluation service: no reply: %w", err)
	}
	switch {
	case reply.Error != "":
		return nil, errors.New(reply.Error)
	case reply.Detail != "":
		return nil, &EvalError{Detail: reply.Detail, InputsMissing: reply.InputsMissing}
	}
	return reply.IR, nil
}
