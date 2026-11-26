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
		return ErrInputsMissing.Error() + " (has flake.lock changed? apply the host configuration with nixos-rebuild first):\n" + e.Cause()
	}
	return "evaluation failed:\n" + e.Cause()
}

// Cause is the innermost error in Detail: Nix prints it last, from its
// final "error:" line (with what follows, such as the failed assertions),
// after the trace of where it happened. The full Detail is logged.
func (e *EvalError) Cause() string {
	lines := strings.Split(e.Detail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimLeft(lines[i], " ")
		if !strings.HasPrefix(trimmed, "error:") {
			continue
		}
		indent := len(lines[i]) - len(trimmed)
		out := make([]string, 0, len(lines)-i)
		for _, l := range lines[i:] {
			if len(l)-len(strings.TrimLeft(l, " ")) >= indent {
				l = l[indent:]
			}
			out = append(out, l)
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	return e.Detail
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

// restricted is a nix command (its words, then the installable last)
// kept offline, pure, without import-from-derivation and without
// substitution (ADR-0025).
func restricted(words ...string) []string {
	n := len(words) - 1
	return append(append(append([]string{}, words[:n]...),
		"--offline", "--no-write-lock-file", "--quiet",
		"--option", "allow-import-from-derivation", "false",
		"--option", "pure-eval", "true",
		"--option", "substitute", "false",
	), words[n])
}

func (e *Evaluator) timeout() time.Duration {
	if e.Timeout == 0 {
		return 5 * time.Minute
	}
	return e.Timeout
}

// run runs nix with args (after the experimental features) and returns
// its standard output, at most limit bytes. ctx carries the evaluation's
// deadline, timeout is only reported. env is added to nix's environment.
func (e *Evaluator) run(ctx context.Context, timeout time.Duration, limit int, env []string, args ...string) ([]byte, error) {
	bin := e.Nix
	if bin == "" {
		bin = "nix"
	}
	cmd := exec.CommandContext(ctx, bin, //nolint:gosec // Fixed arguments; flake URLs, nodes and store paths are validated.
		append([]string{"--extra-experimental-features", "nix-command flakes"}, args...)...)
	// After the timeout, do not wait for children that keep the pipes open.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.Home,
		"XDG_CACHE_HOME=" + e.Home + "/.cache",
		"LC_ALL=C",
	}
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = limit, 1<<20
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("evaluation timed out after %s", timeout)
	}
	if stdout.over {
		return nil, fmt.Errorf("evaluation output exceeds %d bytes", limit)
	}
	if err != nil {
		detail := nixError(stderr.String())
		return nil, &EvalError{Detail: detail, InputsMissing: offlineFailure.MatchString(detail)}
	}
	return stdout.Bytes(), nil
}

// IR evaluates nixosConfigurations.<node>.config.strata.ir of flakeURL
// and returns the JSON.
func (e *Evaluator) IR(ctx context.Context, flakeURL, node string) ([]byte, error) {
	if !nodePattern.MatchString(node) {
		return nil, fmt.Errorf("node %q is not a valid node ID", node)
	}
	timeout := e.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	attr := flakeURL + `#nixosConfigurations."` + node + `".config.strata.ir`
	return e.run(ctx, timeout, maxIRBytes, nil, restricted("eval", "--json", attr)...)
}

// SystemEval is a node's evaluated NixOS system: the derivation of
// config.system.build.toplevel and the output path it builds. The output
// need not exist: evaluating builds nothing.
type SystemEval struct {
	DrvPath string `json:"drvPath"`
	OutPath string `json:"outPath"`
}

// storePath is a store path: the store, a Nix base-32 hash and a name.
var storePath = regexp.MustCompile(`^/nix/store/[0-9abcdfghijklmnpqrsvwxyz]{32}-[0-9A-Za-z+._?=-]+$`)

// Validate checks that both paths are store paths, of a derivation and of
// an output.
func (s SystemEval) Validate() error {
	switch {
	case !storePath.MatchString(s.DrvPath) || !strings.HasSuffix(s.DrvPath, ".drv"):
		return fmt.Errorf("derivation %q is not a store derivation", s.DrvPath)
	case !storePath.MatchString(s.OutPath) || strings.HasSuffix(s.OutPath, ".drv"):
		return fmt.Errorf("output %q is not a store path", s.OutPath)
	}
	return nil
}

// System evaluates nixosConfigurations.<node>.config.system.build.toplevel
// of flakeURL with the same restrictions as IR (offline, pure, no
// import-from-derivation, no substitution) and returns its derivation and
// output path.
//
// It uses `nix derivation show` on the attribute, not `nix eval`: nix eval
// runs read-only and writes no derivations, so a NixOS system's drvPath
// can't be computed unless every input derivation is already in the
// store. `derivation show` instantiates the system (writing its .drv files
// to the store; it builds nothing) and reports the derivation and its
// output from one evaluation.
func (e *Evaluator) System(ctx context.Context, flakeURL, node string) (SystemEval, error) {
	if !nodePattern.MatchString(node) {
		return SystemEval{}, fmt.Errorf("node %q is not a valid node ID", node)
	}
	timeout := e.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	attr := flakeURL + `#nixosConfigurations."` + node + `".config.system.build.toplevel`
	// Instantiating writes derivations, through nix-daemon: named
	// explicitly rather than left to nix's automatic store choice in the
	// service's read-only view of /nix. The evaluation cache is off, so
	// every system evaluation computes its paths from the commit itself.
	show, err := e.run(ctx, timeout, maxIRBytes, []string{"NIX_REMOTE=daemon"},
		restricted("derivation", "show", "--option", "eval-cache", "false", attr)...)
	if err != nil {
		return SystemEval{}, err
	}
	sys, err := parseDerivationShow(show)
	if err != nil {
		return SystemEval{}, fmt.Errorf("system evaluation: %w", err)
	}
	return sys, nil
}

// drvJSON is the part of `nix derivation show` that matters here.
type drvJSON struct {
	Outputs map[string]struct {
		Path string `json:"path"`
	} `json:"outputs"`
}

// parseDerivationShow reads `nix derivation show` output for one
// derivation: the format of Nix 2.35 (version 4: derivations keyed by base
// name, base-name output paths) and the earlier one (keyed by full path,
// full output paths). It returns the derivation and its output "out".
func parseDerivationShow(show []byte) (SystemEval, error) {
	var v4 struct {
		Derivations map[string]drvJSON `json:"derivations"`
	}
	if err := json.Unmarshal(show, &v4); err != nil {
		return SystemEval{}, fmt.Errorf("unexpected output of nix derivation show: %w", err)
	}
	drvs := v4.Derivations
	if drvs == nil {
		if err := json.Unmarshal(show, &drvs); err != nil {
			return SystemEval{}, fmt.Errorf("unexpected output of nix derivation show: %w", err)
		}
	}
	if len(drvs) != 1 {
		return SystemEval{}, fmt.Errorf("nix derivation show listed %d derivations, want 1", len(drvs))
	}
	full := func(p string) string {
		if p != "" && !strings.HasPrefix(p, "/") {
			return "/nix/store/" + p
		}
		return p
	}
	var sys SystemEval
	for name, d := range drvs {
		sys = SystemEval{DrvPath: full(name), OutPath: full(d.Outputs["out"].Path)}
	}
	if err := sys.Validate(); err != nil {
		return SystemEval{}, err
	}
	return sys, nil
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
	// Kind is "ir" (also when empty) or "system".
	Kind string `json:"kind,omitempty"`
}

// Evaluation kinds.
const (
	KindIR     = "ir"
	KindSystem = "system"
)

type evalReply struct {
	IR            json.RawMessage `json:"ir,omitempty"`
	System        *SystemEval     `json:"system,omitempty"`
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
	url := "git+file://" + repoDir + "?rev=" + req.Commit
	var err error
	switch {
	case !shaPattern.MatchString(req.Commit):
		err = fmt.Errorf("commit %q: not a full commit ID", req.Commit)
	case req.Kind == "" || req.Kind == KindIR:
		reply.IR, err = e.IR(ctx, url, req.Node)
	case req.Kind == KindSystem:
		var sys SystemEval
		if sys, err = e.System(ctx, url, req.Node); err == nil {
			reply.System = &sys
		}
	default:
		// Never fall back to another kind.
		err = fmt.Errorf("unknown evaluation kind %q", req.Kind)
	}
	var ee *EvalError
	switch {
	case errors.As(err, &ee):
		reply.Detail, reply.InputsMissing = ee.Detail, ee.InputsMissing
	case err != nil:
		reply.Error = err.Error()
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
	reply, err := r.call(ctx, flakeURL, node, "")
	if err != nil {
		return nil, err
	}
	return reply.IR, nil
}

// System implements SystemEvaluator: the evaluation service evaluates the
// node's system; the reply is checked again here.
func (r *RemoteEvaluator) System(ctx context.Context, flakeURL, node string) (SystemEval, error) {
	reply, err := r.call(ctx, flakeURL, node, KindSystem)
	if err != nil {
		return SystemEval{}, err
	}
	if reply.System == nil {
		return SystemEval{}, errors.New("evaluation service: the reply has no system")
	}
	if err := reply.System.Validate(); err != nil {
		return SystemEval{}, fmt.Errorf("evaluation service: %w", err)
	}
	return *reply.System, nil
}

// call sends one request for the commit of flakeURL (only the commit is
// sent) and returns the reply.
func (r *RemoteEvaluator) call(ctx context.Context, flakeURL, node, kind string) (evalReply, error) {
	_, commit, ok := strings.Cut(flakeURL, "?rev=")
	if !ok || !shaPattern.MatchString(commit) {
		return evalReply{}, fmt.Errorf("flake URL %q has no commit", flakeURL)
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout+30*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", r.Socket)
	if err != nil {
		return evalReply{}, fmt.Errorf("evaluation service: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(evalRequest{Commit: commit, Node: node, Kind: kind}); err != nil {
		return evalReply{}, fmt.Errorf("evaluation service: %w", err)
	}
	var reply evalReply
	if err := json.NewDecoder(io.LimitReader(bufio.NewReader(conn), maxIRBytes+1<<20)).Decode(&reply); err != nil {
		return evalReply{}, fmt.Errorf("evaluation service: no reply: %w", err)
	}
	switch {
	case reply.Error != "":
		return evalReply{}, errors.New(reply.Error)
	case reply.Detail != "":
		return evalReply{}, &EvalError{Detail: reply.Detail, InputsMissing: reply.InputsMissing}
	}
	return reply, nil
}
