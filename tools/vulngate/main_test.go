package main

import (
	"strings"
	"testing"
	"time"
)

// Trimmed govulncheck -format json output: one called vulnerability
// (GO-2026-0001), one only imported (GO-2026-0002).
const sample = `{"config": {"protocol_version": "v1.0.0", "go_version": "go1.27.3"}}
{"osv": {"id": "GO-2026-0001", "summary": "Crash in net/http"}}
{"osv": {"id": "GO-2026-0002", "summary": "Bug in archive/zip"}}
{"finding": {"osv": "GO-2026-0002", "fixed_version": "v1.27.4", "trace": [{"module": "stdlib", "package": "archive/zip"}]}}
{"finding": {"osv": "GO-2026-0001", "fixed_version": "v1.27.4", "trace": [{"module": "stdlib", "package": "net/http"}]}}
{"finding": {"osv": "GO-2026-0001", "fixed_version": "v1.27.4", "trace": [{"module": "stdlib", "package": "net/http", "function": "Serve"}, {"function": "main"}]}}
`

func TestCalledVulns(t *testing.T) {
	v, err := calledVulns(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || v[0] != (Vuln{ID: "GO-2026-0001", Summary: "Crash in net/http", Fixed: "v1.27.4"}) {
		t.Errorf("got %+v", v)
	}
}

func TestCalledVulnsRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"empty":     "",
		"text":      "No vulnerabilities found.\n",
		"no config": `{"osv": {"id": "GO-2026-0001"}}`,
	} {
		if _, err := calledVulns(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseAllow(t *testing.T) {
	a, err := parseAllow(strings.NewReader("# comment\n\nGO-2026-0001 2026-12-31 local socket only\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := a["GO-2026-0001"]
	if e.Reason != "local socket only" || !e.Expires.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("got %+v", e)
	}
	for name, in := range map[string]string{
		"no reason":  "GO-2026-0001 2026-12-31\n",
		"bad id":     "CVE-2026-1 2026-12-31 x\n",
		"bad date":   "GO-2026-0001 31/12/2026 x\n",
		"duplicate":  "GO-2026-0001 2026-12-31 x\nGO-2026-0001 2027-01-31 y\n",
		"date first": "2026-12-31 GO-2026-0001 x\n",
	} {
		if _, err := parseAllow(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestJudge(t *testing.T) {
	called := []Vuln{{ID: "GO-2026-0001", Summary: "Crash", Fixed: "v1.27.4"}}
	allow := map[string]Allow{
		"GO-2026-0001": {ID: "GO-2026-0001", Expires: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Reason: "r"},
		"GO-2025-9999": {ID: "GO-2025-9999", Expires: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Reason: "old"},
	}
	for _, tc := range []struct {
		name  string
		allow map[string]Allow
		now   time.Time
		ok    bool
		want  string
	}{
		{"not listed", nil, time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), false, "FAIL GO-2026-0001"},
		{"allowed", allow, time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), true, "allowed GO-2026-0001 until 2026-12-31"},
		{"expired", allow, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), false, "expired"},
		{"stale entry", allow, time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), true, "stale allow-list entry GO-2025-9999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, ok := judge(called, tc.allow, tc.now); ok != tc.ok || !strings.Contains(out, tc.want) {
				t.Errorf("ok=%v, output:\n%s", ok, out)
			}
		})
	}
	if out, ok := judge(nil, nil, time.Now()); !ok || !strings.Contains(out, "0 called vulnerabilities") {
		t.Errorf("no findings: %s", out)
	}
}
