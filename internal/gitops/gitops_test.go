package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tools skips the test unless git and ssh-keygen are available (they are
// in the development shell and the Nix build).
func tools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // Test helper running git and ssh-keygen.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+dir,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// keypair creates an SSH key and returns the private key path and the
// public key line.
func keypair(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	run(t, dir, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", path)
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return path, strings.TrimSpace(string(pub))
}

// fixture is an upstream repository (a work tree pushed to a bare
// remote) and a Repo that fetches from it.
type fixture struct {
	work, remote string
	trusted      string // private key of the trusted signer
	repo         *Repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tools(t)
	dir := t.TempDir()
	f := &fixture{work: filepath.Join(dir, "work"), remote: filepath.Join(dir, "remote.git")}
	var pub string
	f.trusted, pub = keypair(t, dir, "alice")
	signers, err := AllowedSigners([]string{pub})
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(allowed, []byte(signers), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "init", "--quiet", "--bare", f.remote)
	run(t, dir, "git", "init", "--quiet", "-b", "main", f.work)
	f.repo = &Repo{Dir: filepath.Join(dir, "local.git"), URL: "file://" + f.remote, Branch: "main", AllowedSignersFile: allowed, allowFile: true}
	return f
}

// commit commits a file in the work tree, signed with key ("" for
// unsigned), and returns the commit ID.
func (f *fixture) commit(t *testing.T, file, content, key string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, f.work, "git", "add", file)
	args := []string{"-c", "gpg.format=ssh", "commit", "--quiet", "-m", file}
	if key != "" {
		args = []string{"-c", "gpg.format=ssh", "-c", "user.signingkey=" + key, "commit", "--quiet", "-S", "-m", file}
	}
	run(t, f.work, "git", args...)
	return run(t, f.work, "git", "rev-parse", "HEAD")
}

func (f *fixture) push(t *testing.T, force bool) {
	t.Helper()
	args := []string{"push", "--quiet", f.remote, "main"}
	if force {
		args = append(args, "--force")
	}
	run(t, f.work, "git", args...)
}

func TestFetchAndVerify(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.commit(t, "flake.lock", "lock 1", f.trusted)
	f.push(t, false)

	head, err := f.repo.Fetch(ctx)
	if err != nil || head != first {
		t.Fatalf("Fetch: %q %v, want %s", head, err, first)
	}
	signer, err := f.repo.Verify(ctx, head, "")
	if err != nil || signer != "alice" {
		t.Fatalf("Verify: %q %v", signer, err)
	}

	second := f.commit(t, "host.nix", "{ }", f.trusted)
	f.push(t, false)
	if head, err = f.repo.Fetch(ctx); err != nil || head != second {
		t.Fatalf("second fetch: %q %v", head, err)
	}
	if _, err := f.repo.Verify(ctx, second, first); err != nil {
		t.Errorf("descendant refused: %v", err)
	}
	if _, err := f.repo.Verify(ctx, second, second); err != nil {
		t.Errorf("same commit refused: %v", err)
	}
	if !f.repo.HasCommit(ctx, first) || f.repo.HasCommit(ctx, strings.Repeat("0", 40)) || f.repo.HasCommit(ctx, "HEAD") {
		t.Error("HasCommit")
	}

	lock1, _ := f.repo.FileID(ctx, first, "flake.lock")
	lock2, _ := f.repo.FileID(ctx, second, "flake.lock")
	missing, err := f.repo.FileID(ctx, first, "nope")
	if lock1 == "" || lock1 != lock2 || missing != "" || err != nil {
		t.Errorf("FileID %q %q %q %v", lock1, lock2, missing, err)
	}
	third := f.commit(t, "flake.lock", "lock 2", f.trusted)
	f.push(t, false)
	if _, err := f.repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if lock3, _ := f.repo.FileID(ctx, third, "flake.lock"); lock3 == lock1 {
		t.Error("flake.lock change not seen")
	}
	if got := f.repo.FlakeURL(third); got != "git+file://"+f.repo.Dir+"?rev="+third {
		t.Errorf("FlakeURL %s", got)
	}
}

func TestVerifyRefusesUntrustedCommits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	unsigned := f.commit(t, "a", "1", "")
	other, _ := keypair(t, t.TempDir(), "mallory")
	foreign := f.commit(t, "b", "2", other)
	f.push(t, false)
	if _, err := f.repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	for name, sha := range map[string]string{"unsigned": unsigned, "untrusted key": foreign} {
		if _, err := f.repo.Verify(ctx, sha, ""); !errors.Is(err, ErrUntrusted) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.repo.Verify(ctx, "HEAD", ""); err == nil {
		t.Error("a ref name was accepted as a commit ID")
	}
}

func TestVerifyRefusesRewinds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	base := f.commit(t, "a", "1", f.trusted)
	applied := f.commit(t, "b", "2", f.trusted)
	f.push(t, false)
	if _, err := f.repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	// Rewrite history: a different, signed commit on top of base.
	run(t, f.work, "git", "reset", "--quiet", "--hard", base)
	rewritten := f.commit(t, "c", "3", f.trusted)
	f.push(t, true)
	head, err := f.repo.Fetch(ctx)
	if err != nil || head != rewritten {
		t.Fatalf("fetch after force-push: %q %v", head, err)
	}
	if _, err := f.repo.Verify(ctx, head, applied); !errors.Is(err, ErrRewound) {
		t.Errorf("rewound: %v", err)
	}
	// An ancestor of the applied commit is a rewind too.
	if _, err := f.repo.Verify(ctx, base, applied); !errors.Is(err, ErrRewound) {
		t.Errorf("ancestor: %v", err)
	}
}

func TestCheck(t *testing.T) {
	ok := Repo{URL: "https://git.example.org/infra.git", Branch: "main", AllowedSignersFile: "/x"}
	if err := ok.Check(); err != nil {
		t.Errorf("https: %v", err)
	}
	sshOK := ok
	sshOK.URL, sshOK.SSHKeyFile, sshOK.KnownHostsFile = "ssh://git@git.example.org/infra.git", "/k", "/kh"
	if err := sshOK.Check(); err != nil {
		t.Errorf("ssh: %v", err)
	}
	for name, edit := range map[string]func(*Repo){
		"file":             func(r *Repo) { r.URL = "file:///srv/repo" },
		"http":             func(r *Repo) { r.URL = "http://git.example.org/x.git" },
		"ext":              func(r *Repo) { r.URL = "ext::sh -c touch% /tmp/pwned" },
		"scp-like":         func(r *Repo) { r.URL = "git@git.example.org:infra.git" },
		"credentials":      func(r *Repo) { r.URL = "https://user:pw@git.example.org/x.git" },
		"ssh without keys": func(r *Repo) { r.URL = "ssh://git@git.example.org/x.git" },
		"branch dots":      func(r *Repo) { r.Branch = "main/../x" },
		"branch option":    func(r *Repo) { r.Branch = "--upload-pack=x" },
		"no signers":       func(r *Repo) { r.AllowedSignersFile = "" },
	} {
		r := ok
		edit(&r)
		if err := r.Check(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCredentialsNeverOnTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	// A stand-in git that records its arguments and environment.
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + dir + "/args\nenv > " + dir + "/env\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil { //nolint:gosec // An executable test script.
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "repo.git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo.git", "HEAD"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Repo{Dir: filepath.Join(dir, "repo.git"), URL: "https://git.example.org/x.git", Branch: "main", TokenFile: token, AllowedSignersFile: "/x", Git: fake}
	if _, err := r.Fetch(context.Background()); err == nil {
		t.Fatal("fake git succeeded")
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if strings.Contains(string(args), "s3cret") || strings.Contains(string(args), "Authorization") {
		t.Errorf("credentials in arguments:\n%s", args)
	}
	if !strings.Contains(string(env), "GIT_CONFIG_KEY_0=http.extraHeader") || !strings.Contains(string(env), "GIT_CONFIG_NOSYSTEM=1") {
		t.Errorf("environment:\n%s", env)
	}
	for _, want := range []string{"protocol.allow=never", "core.hooksPath=/dev/null", "--no-tags"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("arguments lack %q:\n%s", want, args)
		}
	}
}

func TestSSHCommand(t *testing.T) {
	r := &Repo{URL: "ssh://git@h/x.git", SSHKeyFile: "/run/credentials/s/key", KnownHostsFile: "/etc/kh", SSH: "/bin/ssh"}
	env, err := r.fetchEnv()
	if err != nil || len(env) != 1 || !strings.Contains(env[0], "-o StrictHostKeyChecking=yes -o UserKnownHostsFile=/etc/kh") ||
		!strings.Contains(env[0], "-F /dev/null -i /run/credentials/s/key") {
		t.Errorf("env %q %v", env, err)
	}
	r.SSHKeyFile = "/tmp/a key"
	if _, err := r.fetchEnv(); err == nil {
		t.Error("a path with a space was accepted")
	}
}

func TestAllowedSigners(t *testing.T) {
	got, err := AllowedSigners([]string{"ssh-ed25519 AAAA alice laptop", "ssh-rsa BBBB", "ssh-ed25519 CCCC x,y\"z"})
	want := "alice_laptop namespaces=\"git\" ssh-ed25519 AAAA\nsigner2 namespaces=\"git\" ssh-rsa BBBB\nx_y_z namespaces=\"git\" ssh-ed25519 CCCC\n"
	if err != nil || got != want {
		t.Errorf("got %q %v", got, err)
	}
	if _, err := AllowedSigners([]string{"ssh-ed25519"}); err == nil {
		t.Error("key without data accepted")
	}
}
