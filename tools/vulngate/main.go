// Command vulngate fails when govulncheck reports a vulnerability that the
// code calls and that is not on the allow-list (ADR-0022).
//
// Usage:
//
//	govulncheck -format json ./... | vulngate ALLOW-FILE
//
// Each non-comment line of ALLOW-FILE is "ID EXPIRY REASON", for example
//
//	GO-2026-6859 2026-12-31 agent API is on a local socket; fix not in nixpkgs yet
//
// An entry stops applying on its expiry date (UTC). Entries that are no
// longer reported are listed, so they can be removed.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: govulncheck -format json ./... | vulngate ALLOW-FILE")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1]) //nolint:gosec // The allow-list path given by the developer or CI.
	if err != nil {
		fmt.Fprintln(os.Stderr, "vulngate:", err)
		os.Exit(2)
	}
	allow, err := parseAllow(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vulngate: %s: %v\n", os.Args[1], err)
		os.Exit(2)
	}
	called, err := calledVulns(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vulngate:", err)
		os.Exit(2)
	}
	report, ok := judge(called, allow, time.Now().UTC())
	fmt.Print(report)
	if !ok {
		os.Exit(1)
	}
}

// Allow is one allow-list entry.
type Allow struct {
	ID      string
	Expires time.Time // the first day on which the entry no longer applies
	Reason  string
}

// Vuln is a vulnerability govulncheck found in called code.
type Vuln struct {
	ID      string
	Summary string
	Fixed   string // the version with the fix, if known
}

var idPattern = regexp.MustCompile(`^GO-\d{4}-\d{4,}$`)

func parseAllow(r io.Reader) (map[string]Allow, error) {
	allow := map[string]Allow{}
	s := bufio.NewScanner(r)
	for n := 1; s.Scan(); n++ {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("line %d: want \"ID EXPIRY REASON\"", n)
		}
		if !idPattern.MatchString(fields[0]) {
			return nil, fmt.Errorf("line %d: %q is not a Go vulnerability ID", n, fields[0])
		}
		day, err := time.Parse(time.DateOnly, fields[1])
		if err != nil {
			return nil, fmt.Errorf("line %d: expiry %q is not YYYY-MM-DD", n, fields[1])
		}
		if _, dup := allow[fields[0]]; dup {
			return nil, fmt.Errorf("line %d: %s is listed twice", n, fields[0])
		}
		allow[fields[0]] = Allow{
			ID:      fields[0],
			Expires: day.AddDate(0, 0, 1),
			Reason:  strings.Join(fields[2:], " "),
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return allow, nil
}

// message is the part of govulncheck's JSON protocol (v1) vulngate reads.
type message struct {
	OSV *struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"osv"`
	Finding *struct {
		OSV          string `json:"osv"`
		FixedVersion string `json:"fixed_version"`
		Trace        []struct {
			Function string `json:"function"`
		} `json:"trace"`
	} `json:"finding"`
}

// calledVulns returns the vulnerabilities with a finding in a called
// function (what govulncheck's text output reports), sorted by ID.
// Findings only at module or package level are not called and are ignored.
func calledVulns(r io.Reader) ([]Vuln, error) {
	summaries := map[string]string{}
	found := map[string]Vuln{}
	sawConfig := false
	dec := json.NewDecoder(r)
	for {
		var raw map[string]json.RawMessage
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("govulncheck output: %w", err)
		}
		if _, ok := raw["config"]; ok {
			sawConfig = true
		}
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("govulncheck output: %w", err)
		}
		var m message
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("govulncheck output: %w", err)
		}
		if m.OSV != nil {
			summaries[m.OSV.ID] = m.OSV.Summary
		}
		if f := m.Finding; f != nil && len(f.Trace) > 0 && f.Trace[0].Function != "" {
			found[f.OSV] = Vuln{ID: f.OSV, Fixed: f.FixedVersion}
		}
	}
	if !sawConfig {
		// An empty input would otherwise pass: govulncheck did not run.
		return nil, errors.New("no govulncheck output (expected -format json)")
	}
	vulns := make([]Vuln, 0, len(found))
	for _, v := range found {
		v.Summary = summaries[v.ID]
		vulns = append(vulns, v)
	}
	slices.SortFunc(vulns, func(a, b Vuln) int { return strings.Compare(a.ID, b.ID) })
	return vulns, nil
}

// judge returns a report and reports whether every called vulnerability is
// allowed by an unexpired entry.
func judge(called []Vuln, allow map[string]Allow, now time.Time) (string, bool) {
	var w strings.Builder
	ok := true
	seen := map[string]bool{}
	for _, v := range called {
		seen[v.ID] = true
		fix := v.Fixed
		if fix == "" {
			fix = "no fix yet"
		}
		a, listed := allow[v.ID]
		switch {
		case !listed:
			ok = false
			fmt.Fprintf(&w, "FAIL %s: %s (fixed in %s)\n", v.ID, v.Summary, fix)
		case !now.Before(a.Expires):
			ok = false
			fmt.Fprintf(&w, "FAIL %s: %s (fixed in %s); allowed until %s, expired\n",
				v.ID, v.Summary, fix, a.Expires.AddDate(0, 0, -1).Format(time.DateOnly))
		default:
			fmt.Fprintf(&w, "allowed %s until %s: %s (fixed in %s); reason: %s\n",
				v.ID, a.Expires.AddDate(0, 0, -1).Format(time.DateOnly), v.Summary, fix, a.Reason)
		}
	}
	ids := make([]string, 0, len(allow))
	for id := range allow {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		fmt.Fprintf(&w, "stale allow-list entry %s: no longer reported, remove it\n", id)
	}
	if ok {
		fmt.Fprintf(&w, "govulncheck: %d called vulnerabilities, none blocking\n", len(called))
	}
	return w.String(), ok
}
