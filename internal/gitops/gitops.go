// Package gitops reads the configuration repository (M3, ADR-0025): it
// fetches the tracked branch into a bare repository with the git binary,
// and checks a commit before it may be evaluated: signed by a trusted key,
// and not a rewind of what was applied.
//
// git runs with fixed arguments and no shell, without the system or user
// Git configuration, without prompts and with only the https and ssh
// protocols. The repository is bare: nothing is checked out and no hooks
// run. Credentials never appear on a command line.
package gitops

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Errors returned by the trust checks.
var (
	// ErrUntrusted: the commit is not signed by a trusted key.
	ErrUntrusted = errors.New("not signed by a trusted key")
	// ErrRewound: the commit does not descend from the last applied
	// commit (the branch was rewound or force-pushed).
	ErrRewound = errors.New("does not descend from the last applied commit")
)

var (
	shaPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	// principalUnsafe matches characters not allowed in a signer's name.
	principalUnsafe = regexp.MustCompile(`[^A-Za-z0-9._1190 1190-]`)
)

// Repo is the local copy of the configuration repository.
type Repo struct {
	// Dir is the bare repository; it is created if missing.
	Dir string
	// URL is https://... or ssh://...
	URL    string
	Branch string
	// SSHKeyFile and KnownHostsFile are used for ssh:// URLs.
	SSHKeyFile     string
	KnownHostsFile string
	// TokenFile holds an access token for https:// URLs (optional).
	TokenFile string
	// AllowedSignersFile lists the trusted signing keys (OpenSSH
	// allowed_signers format).
	AllowedSignersFile string
	// Git and SSH are the binaries (defaults: "git", "ssh" from PATH).
	Git, SSH string
	// Timeout bounds each git command (default 2 minutes).
	Timeout time.Duration

	// allowFile permits file:// remotes (tests only).
	allowFile bool
}

// Check validates the configuration.
func (r *Repo) Check() error {
	u, err := url.Parse(r.URL)
	if r.allowFile && err == nil && u.Scheme == "file" {
		return nil
	}
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("repository %q: must be an https:// or ssh:// URL", r.URL)
	}
	if u.Scheme == "ssh" && (r.SSHKeyFile == "" || r.KnownHostsFile == "") {
		return fmt.Errorf("repository %q: ssh needs a key and known hosts", r.URL)
	}
	if u.Scheme == "https" && u.User != nil {
		return fmt.Errorf("repository %q: put credentials in the token file, not the URL", r.URL)
	}
	if !branchPattern.MatchString(r.Branch) || strings.Contains(r.Branch, "..") {
		return fmt.Errorf("branch %q is not a valid branch name", r.Branch)
	}
	if r.AllowedSignersFile == "" {
		return errors.New("no allowed signers file: commits cannot be verified")
	}
	return nil
}

// git runs git in the repository and returns its standard output.
func (r *Repo) git(ctx context.Context, extraEnv []string, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bin := r.Git
	if bin == "" {
		bin = "git"
	}
	full := append([]string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always",
		"-c", "protocol.file.allow=" + map[bool]string{true: "always", false: "never"}[r.allowFile],
		"-c", "core.hooksPath=/dev/null",
		"-c", "gpg.format=ssh",
		"-c", "gpg.ssh.allowedSignersFile=" + r.AllowedSignersFile,
		"--git-dir", r.Dir,
	}, args...)
	cmd := exec.CommandContext(ctx, bin, full...) //nolint:gosec // Fixed arguments; values are validated (Check) or commit IDs.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.Dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"LC_ALL=C",
	}, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return stdout.Bytes(), nil
}

// init creates the bare repository if needed.
func (r *Repo) init(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(r.Dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", r.Dir, err)
	}
	_, err := r.git(ctx, nil, "init", "--quiet", "--bare")
	return err
}

// fetchEnv returns the environment carrying the credentials.
func (r *Repo) fetchEnv() ([]string, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, fmt.Errorf("repository URL: %w", err)
	}
	switch u.Scheme {
	case "ssh":
		ssh := r.SSH
		if ssh == "" {
			ssh = "ssh"
		}
		for _, p := range []string{ssh, r.SSHKeyFile, r.KnownHostsFile} {
			if strings.ContainsAny(p, " \t\n'\"\\$`") {
				return nil, fmt.Errorf("path %q must not contain spaces or shell characters", p)
			}
		}
		return []string{"GIT_SSH_COMMAND=" + ssh + " -F /dev/null -i " + r.SSHKeyFile +
			" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + r.KnownHostsFile}, nil
	case "file":
		return nil, nil
	case "https":
		if r.TokenFile == "" {
			return nil, nil
		}
		token, err := os.ReadFile(r.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read token: %w", err)
		}
		// Passed as configuration in the environment, never as an argument.
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + strings.TrimSpace(string(token))))
		return []string{
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic " + auth,
		}, nil
	}
	return nil, fmt.Errorf("repository %q: unsupported scheme", r.URL)
}

// Fetch fetches the tracked branch and returns its head commit.
func (r *Repo) Fetch(ctx context.Context) (string, error) {
	if err := r.Check(); err != nil {
		return "", err
	}
	if err := r.init(ctx); err != nil {
		return "", err
	}
	env, err := r.fetchEnv()
	if err != nil {
		return "", err
	}
	// The branch is fetched as itself, and HEAD names it, so the bare
	// repository is a well-formed source for Nix.
	ref := "refs/heads/" + r.Branch
	if _, err := r.git(ctx, env, "fetch", "--quiet", "--no-tags", "--force", "--", r.URL, "+refs/heads/"+r.Branch+":"+ref); err != nil {
		return "", err
	}
	if _, err := r.git(ctx, nil, "symbolic-ref", "HEAD", ref); err != nil {
		return "", err
	}
	out, err := r.git(ctx, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("branch %s: %w", r.Branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// HasCommit reports whether the repository has the commit (fetched
// earlier, e.g. for pinning).
func (r *Repo) HasCommit(ctx context.Context, sha string) bool {
	if !shaPattern.MatchString(sha) {
		return false
	}
	_, err := r.git(ctx, nil, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// Verify checks that sha is signed by a trusted key and returns the
// signer's name (the principal in the allowed signers file). If
// lastApplied is set, sha must descend from it (or be it).
func (r *Repo) Verify(ctx context.Context, sha, lastApplied string) (string, error) {
	if !shaPattern.MatchString(sha) {
		return "", fmt.Errorf("commit %q: not a full commit ID", sha)
	}
	// Only SSH signatures are verified: git would run gpg (or gpgsm) for
	// the others, which the controller neither has nor trusts.
	kind, err := r.signatureKind(ctx, sha)
	if err != nil {
		return "", err
	}
	switch kind {
	case sigNone:
		return "", fmt.Errorf("commit %s: %w (the commit is not signed)", sha[:12], ErrUntrusted)
	case sigOpenPGP:
		return "", fmt.Errorf("commit %s: %w (it has an OpenPGP signature; only SSH signatures are trusted. "+
			"Commits made on a Git host's website are signed by the host's key: push a commit signed with your SSH key on top)", sha[:12], ErrUntrusted)
	case sigOther:
		return "", fmt.Errorf("commit %s: %w (it has an X.509 or unknown signature; only SSH signatures are trusted)", sha[:12], ErrUntrusted)
	}
	// --raw prints the ssh-keygen result on stderr; git reports it as an
	// error when the signature is not trusted.
	if _, err := r.git(ctx, nil, "verify-commit", "--raw", sha); err != nil {
		return "", fmt.Errorf("commit %s: %w (%w)", sha[:12], ErrUntrusted, err)
	}
	signer, err := r.signer(ctx, sha)
	if err != nil {
		return "", err
	}
	if lastApplied != "" && lastApplied != sha {
		if !shaPattern.MatchString(lastApplied) {
			return "", fmt.Errorf("last applied commit %q: not a full commit ID", lastApplied)
		}
		if _, err := r.git(ctx, nil, "merge-base", "--is-ancestor", lastApplied, sha); err != nil {
			return "", fmt.Errorf("commit %s %w %s; pin a commit explicitly to roll back", sha[:12], ErrRewound, lastApplied[:12])
		}
	}
	return signer, nil
}

// Signature kinds of a commit.
const (
	sigNone = iota
	sigSSH
	sigOpenPGP
	sigOther
)

// signatureKind reads the signature header of commit sha (gpgsig, or
// gpgsig-sha256 in a SHA-256 repository) and returns its kind.
func (r *Repo) signatureKind(ctx context.Context, sha string) (int, error) {
	out, err := r.git(ctx, nil, "cat-file", "commit", sha)
	if err != nil {
		return sigNone, err
	}
	header, _, _ := strings.Cut(string(out), "\n\n")
	for line := range strings.SplitSeq(header, "\n") {
		name, value, ok := strings.Cut(line, " ")
		if !ok || (name != "gpgsig" && name != "gpgsig-sha256") {
			continue
		}
		switch strings.TrimSpace(value) {
		case "-----BEGIN SSH SIGNATURE-----":
			return sigSSH, nil
		case "-----BEGIN PGP SIGNATURE-----":
			return sigOpenPGP, nil
		default:
			return sigOther, nil
		}
	}
	return sigNone, nil
}

// signer returns the principal whose key signed sha.
func (r *Repo) signer(ctx context.Context, sha string) (string, error) {
	out, err := r.git(ctx, nil, "log", "-1", "--format=%GS", sha)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// FileID returns the blob ID of path at commit sha, or "" if the commit
// has no such file.
func (r *Repo) FileID(ctx context.Context, sha, path string) (string, error) {
	if !shaPattern.MatchString(sha) {
		return "", fmt.Errorf("commit %q: not a full commit ID", sha)
	}
	out, err := r.git(ctx, nil, "ls-tree", "--object-only", sha, "--", path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Tree returns the ID of commit sha's tree: what the commit contains,
// recorded with each evaluation.
func (r *Repo) Tree(ctx context.Context, sha string) (string, error) {
	if !shaPattern.MatchString(sha) {
		return "", fmt.Errorf("commit %q: not a full commit ID", sha)
	}
	out, err := r.git(ctx, nil, "rev-parse", "--verify", "--quiet", sha+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("commit %s: no tree: %w", sha[:12], err)
	}
	tree := strings.TrimSpace(string(out))
	if !shaPattern.MatchString(tree) {
		return "", fmt.Errorf("commit %s: unexpected tree ID %q", sha[:12], tree)
	}
	return tree, nil
}

// FlakeURL is the flake reference of commit sha in the local repository.
func (r *Repo) FlakeURL(sha string) string {
	return "git+file://" + r.Dir + "?rev=" + sha
}

// AllowedSigners renders trusted keys ("TYPE KEY [NAME]") in the OpenSSH
// allowed_signers format; a key without a name is called signerN.
func AllowedSigners(keys []string) (string, error) {
	var b strings.Builder
	for i, k := range keys {
		f := strings.Fields(k)
		if len(f) < 2 {
			return "", fmt.Errorf("trusted signer %d: want \"TYPE KEY [NAME]\"", i+1)
		}
		name := fmt.Sprintf("signer%d", i+1)
		if len(f) > 2 {
			if n := principalUnsafe.ReplaceAllString(strings.Join(f[2:], "_"), "_"); n != "" {
				name = n
			}
		}
		fmt.Fprintf(&b, "%s namespaces=\"git\" %s %s\n", name, f[0], f[1])
	}
	return b.String(), nil
}

var prefixPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Resolve returns the full ID of a commit in the fetched history, given
// its ID or a prefix of at least 7 hex digits.
func (r *Repo) Resolve(ctx context.Context, ref string) (string, error) {
	if !prefixPattern.MatchString(ref) {
		return "", fmt.Errorf("%q is not a commit ID", ref)
	}
	out, err := r.git(ctx, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("commit %s is not in the fetched history (or the prefix is ambiguous)", ref)
	}
	return strings.TrimSpace(string(out)), nil
}
