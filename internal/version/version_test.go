package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	t.Parallel()

	// version and commit may be injected via -ldflags, so Get is checked
	// against the linked values; fallback behavior is covered by TestNewInfo.
	info := Get()
	if want := newInfo(version, commit); info != want {
		t.Errorf("Get() = %+v, want %+v", info, want)
	}
	if info.Version == "" || info.Commit == "" {
		t.Errorf("Get() returned empty fields: %+v", info)
	}
	if info.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; info.Platform != want {
		t.Errorf("Platform = %q, want %q", info.Platform, want)
	}
}

func TestNewInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		version    string
		commit     string
		wantVer    string
		wantCommit string
	}{
		{"explicit values", "1.2.3", "abc1234", "1.2.3", "abc1234"},
		{"empty version falls back", "", "abc1234", DefaultVersion, "abc1234"},
		{"empty commit falls back", "1.2.3", "", "1.2.3", DefaultCommit},
		{"both empty fall back", "", "", DefaultVersion, DefaultCommit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := newInfo(tc.version, tc.commit)
			if got.Version != tc.wantVer || got.Commit != tc.wantCommit {
				t.Errorf("newInfo(%q, %q) = {%q, %q}, want {%q, %q}",
					tc.version, tc.commit, got.Version, got.Commit, tc.wantVer, tc.wantCommit)
			}
		})
	}
}

func TestInfoString(t *testing.T) {
	t.Parallel()

	s := newInfo("1.2.3", "abc1234").String()
	for _, want := range []string{"1.2.3", "commit abc1234", runtime.Version()} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}
