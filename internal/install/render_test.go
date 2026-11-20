package install

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRenderHostNixGolden(t *testing.T) {
	for _, tt := range []struct{ name, answers string }{
		{"host-static.nix", staticAnswers},
		{"host-dhcp.nix", dhcpAnswers},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderHostNix(mustParse(t, tt.answers))
			golden := filepath.Join("testdata", tt.name)
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("RenderHostNix mismatch (run with -update to accept):\n--- got\n%s\n--- want\n%s", got, want)
			}
		})
	}
}

func TestNixStringEscapes(t *testing.T) {
	tests := map[string]string{
		`plain`:       `"plain"`,
		`a"b`:         `"a\"b"`,
		`a\b`:         `"a\\b"`,
		`${x}`:        `"\${x}"`,
		`$x and $`:    `"$x and $"`,
		"line\nbreak": `"line\nbreak"`,
		`\${x}`:       `"\\\${x}"`,
	}
	for in, want := range tests {
		if got := nixString(in); got != want {
			t.Errorf("nixString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRenderHostNixEscapesKeyComments(t *testing.T) {
	a := mustParse(t, dhcpAnswers)
	a.Admin.AuthorizedKeys = []string{`ssh-ed25519 AAAA x"; } // ${builtins.abort "pwned"}`}
	if err := a.Validate(); err != nil {
		t.Fatalf("test key should be valid: %v", err)
	}
	got := RenderHostNix(a)
	want := `"ssh-ed25519 AAAA x\"; } // \${builtins.abort \"pwned\"}"`
	if !containsLine(got, want) {
		t.Errorf("key not escaped as %s:\n%s", want, got)
	}
}

func containsLine(text, line string) bool {
	for l := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}
