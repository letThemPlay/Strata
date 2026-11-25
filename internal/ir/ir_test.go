package ir

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// golden is the IR Nix produces for examples/standalone (kept equal to it
// by the examples flake check).
func golden(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/standalone.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mutated returns the golden document as generic JSON after edit.
func mutated(t *testing.T, edit func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(golden(t), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func obj(v any) map[string]any { return v.(map[string]any) }
func arr(v any) []any          { return v.([]any) }

// resource returns resource i of doc.
func resource(doc map[string]any, i int) map[string]any { return obj(arr(doc["resources"])[i]) }

func TestParseGolden(t *testing.T) {
	doc, err := Parse(bytes.NewReader(golden(t)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Node != "hv-01" || doc.Host.Project != "strata" || doc.Host.DefaultPool != "fast" || len(doc.Resources) != 3 {
		t.Errorf("document %+v", doc)
	}
	db := doc.Resources[0]
	if db.ID != "vm/db-01" || !db.Lifecycle.Protected || db.VM == nil || db.VM.Disks[1].Pool != "bulk" {
		t.Errorf("db-01 %+v", db)
	}
	if web := doc.Resources[2]; web.VM.ImageServer != "images.linuxcontainers.org" || web.Lifecycle.Protected {
		t.Errorf("web-01 %+v", web)
	}
	if doc.Host.Limits != (Limits{CPU: 16, Memory: "64GiB", Instances: 20}) {
		t.Errorf("limits %+v", doc.Host.Limits)
	}
}

func TestParseEmpty(t *testing.T) {
	data := mutated(t, func(d map[string]any) { d["resources"] = []any{} })
	doc, err := Parse(bytes.NewReader(data))
	if err != nil || len(doc.Resources) != 0 {
		t.Errorf("%+v %v", doc, err)
	}
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(map[string]any)
		want []string
	}{
		"version":       {func(d map[string]any) { d["apiVersion"] = "ir.strata.dev/v2" }, []string{`apiVersion must be "ir.strata.dev/v1alpha1"`}},
		"unknown field": {func(d map[string]any) { d["extra"] = 1 }, []string{`unknown field "extra"`}},
		"unknown spec field": {
			func(d map[string]any) { obj(resource(d, 1)["spec"])["raw"] = "x" },
			[]string{`resources[vm/scratch].spec`, "unknown field"},
		},
		"unknown type": {func(d map[string]any) { resource(d, 1)["type"] = "lxc" }, []string{`resources[vm/scratch].type: "lxc" is not a supported`}},
		"bad node":     {func(d map[string]any) { d["node"] = "HV_01" }, []string{`node: "HV_01"`}},
		"id mismatch":  {func(d map[string]any) { resource(d, 1)["id"] = "vm/scratch2" }, []string{`id: must be "vm/scratch"`}},
		"duplicate id": {
			func(d map[string]any) { d["resources"] = append(arr(d["resources"]), arr(d["resources"])[2]) },
			[]string{"IDs must be unique and sorted"},
		},
		"unsorted": {
			func(d map[string]any) { r := arr(d["resources"]); r[0], r[2] = r[2], r[0] },
			[]string{"IDs must be unique and sorted"},
		},
		"missing dependency": {
			func(d map[string]any) { resource(d, 0)["dependsOn"] = []any{"network/dmz", "network/lan", "pool/fast"} },
			[]string{`dependsOn: must be ["network/dmz" "network/lan" "pool/bulk" "pool/fast"]`},
		},
		"extra dependency": {
			func(d map[string]any) { resource(d, 1)["dependsOn"] = []any{"network/lan", "pool/fast"} },
			[]string{`resources[vm/scratch].dependsOn`},
		},
		"undeclared pool": {
			func(d map[string]any) {
				obj(d["host"])["pools"] = []any{map[string]any{"name": "fast", "driver": "zfs"}}
			},
			[]string{"resources[vm/db-01]: pool/bulk is not declared on the host"},
		},
		"undeclared network": {
			func(d map[string]any) { obj(d["host"])["networks"] = []any{map[string]any{"name": "lan"}} },
			[]string{"resources[vm/db-01]: network/dmz is not declared", "resources[vm/web-01]: network/dmz is not declared"},
		},
		"image server not allowed": {
			func(d map[string]any) { obj(d["host"])["imageServers"] = []any{} },
			[]string{`resources[vm/web-01].spec.imageServer: "images.linuxcontainers.org" is not an allowed`},
		},
		"invalid spec": {
			func(d map[string]any) { obj(obj(resource(d, 1)["spec"])["resources"])["cpu"] = 0 },
			[]string{"resources[vm/scratch].spec", "resources.cpu"},
		},
		"instances": {func(d map[string]any) { obj(obj(d["host"])["limits"])["instances"] = 2 }, []string{"3 exceed the project's instance limit (2)"}},
		"cpu":       {func(d map[string]any) { obj(obj(d["host"])["limits"])["cpu"] = 6 }, []string{"7 CPUs in total exceed the project's CPU limit (6)"}},
		"memory":    {func(d map[string]any) { obj(obj(d["host"])["limits"])["memory"] = "10GiB" }, []string{"exceed the project's memory limit (10GiB)"}},
		"bad limit": {func(d map[string]any) { obj(obj(d["host"])["limits"])["memory"] = "lots" }, []string{"host.limits.memory"}},
		"driver":    {func(d map[string]any) { obj(arr(obj(d["host"])["pools"])[0])["driver"] = "btrfs" }, []string{`host.pools[bulk].driver: "btrfs"`}},
		"default pool": {
			func(d map[string]any) { obj(d["host"])["defaultPool"] = "slow" },
			[]string{`host.defaultPool: "slow" is not a declared pool`},
		},
		"default project": {func(d map[string]any) { obj(d["host"])["project"] = "default" }, []string{"host.project"}},
		"missing lists": {
			func(d map[string]any) { delete(obj(d["host"]), "networks") },
			[]string{"pools, networks and imageServers must be present"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(bytes.NewReader(mutated(t, tc.edit)))
			if err == nil {
				t.Fatal("accepted")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	data := mutated(t, func(d map[string]any) {
		d["node"] = "Bad"
		resource(d, 1)["id"] = "vm/x"
		obj(obj(d["host"])["limits"])["cpu"] = 1
	})
	_, err := Parse(bytes.NewReader(data))
	if err == nil || strings.Count(err.Error(), "\n") < 2 {
		t.Errorf("want three problems, got %v", err)
	}
}

func TestParseMalformed(t *testing.T) {
	for name, in := range map[string]string{
		"not json": "nope",
		"trailing": string(golden(t)) + "{}",
		"too big":  strings.Repeat(" ", maxBytes+1),
	} {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseLimit(t *testing.T) {
	for in, want := range map[string]uint64{"64GiB": 64 << 30, "2PiB": 2 << 50, "1B": 1} {
		if n, err := parseLimit(in); err != nil || n != want {
			t.Errorf("%s: %d %v", in, n, err)
		}
	}
	for _, in := range []string{"", "1EiB", "999999999999999PiB"} {
		if _, err := parseLimit(in); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}
