// Package version exposes build metadata for Strata binaries.
//
// The package-level variables are populated at link time via -ldflags, e.g.
//
//	-X github.com/letthemplay/Strata/internal/version.version=0.1.0
//	-X github.com/letthemplay/Strata/internal/version.commit=abc1234
//
// They are unexported and never modified at runtime; callers read them
// through Get.
package version

import (
	"fmt"
	"runtime"
)

// Default values used when a binary is built without link-time metadata
// (for example with a plain `go build` or `go test`).
const (
	DefaultVersion = "0.0.0-dev"
	DefaultCommit  = "unknown"
)

//nolint:gochecknoglobals // Set exclusively by the linker; read-only at runtime.
var (
	version = DefaultVersion
	commit  = DefaultCommit
)

// Info describes the build of a Strata binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build information for the running binary.
func Get() Info {
	return newInfo(version, commit)
}

func newInfo(v, c string) Info {
	if v == "" {
		v = DefaultVersion
	}
	if c == "" {
		c = DefaultCommit
	}
	return Info{
		Version:   v,
		Commit:    c,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String renders the build information on a single line.
func (i Info) String() string {
	return fmt.Sprintf("%s (commit %s, %s, %s)", i.Version, i.Commit, i.GoVersion, i.Platform)
}
