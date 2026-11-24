package workload

import (
	"strings"
	"testing"
)

const validSpec = `{
  "apiVersion": "vm.strata.dev/v1alpha1",
  "name": "web-01",
  "resources": {"cpu": 2, "memory": "4GiB"},
  "image": "debian-13",
  "disks": [
    {"name": "root", "size": "20GiB", "pool": "fast"},
    {"name": "data", "size": "100GiB", "pool": "bulk"}
  ],
  "networks": [{"name": "eth0", "network": "lan"}],
  "autostart": true,
  "cloudInit": {"sshAuthorizedKeys": ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl admin@ws"]}
}`

func TestParseValidVM(t *testing.T) {
	vm, err := ParseVM(strings.NewReader(validSpec))
	if err != nil {
		t.Fatalf("ParseVM: %v", err)
	}
	if vm.Name != "web-01" || vm.Resources.CPU != 2 || len(vm.Disks) != 2 || !vm.Autostart {
		t.Errorf("parsed %+v", vm)
	}
	if got := vm.VolumeName(vm.Disks[1]); got != "web-01-data" {
		t.Errorf("volume name %q", got)
	}
	// An empty VM (no image, no network) is valid.
	empty := `{"apiVersion":"vm.strata.dev/v1alpha1","name":"blank","resources":{"cpu":1,"memory":"512MiB"},
		"disks":[{"name":"root","size":"8GiB","pool":"fast"}]}`
	if _, err := ParseVM(strings.NewReader(empty)); err != nil {
		t.Errorf("empty VM: %v", err)
	}
}

func TestParseVMRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		replace [2]string
		want    string
	}{
		"api version":      {[2]string{`vm.strata.dev/v1alpha1`, `vm.strata.dev/v9`}, "apiVersion: must be"},
		"name":             {[2]string{`"web-01"`, `"Web_01"`}, "name: \"Web_01\""},
		"cpu":              {[2]string{`"cpu": 2`, `"cpu": 0`}, "resources.cpu: must be 1-256"},
		"memory unit":      {[2]string{`"4GiB"`, `"4G"`}, "resources.memory:"},
		"memory too small": {[2]string{`"4GiB"`, `"64MiB"`}, "at least 128MiB"},
		"remote image":     {[2]string{`"debian-13"`, `"images:debian/13"`}, "remote images (server:alias) are not allowed"},
		"root not first":   {[2]string{`"name": "root"`, `"name": "boot"`}, `the first disk must be named "root"`},
		"small root":       {[2]string{`"size": "20GiB"`, `"size": "512MiB"`}, "the root disk needs at least 1GiB"},
		"duplicate disk":   {[2]string{`"name": "data"`, `"name": "root"`}, `"root" is used twice`},
		"bad pool":         {[2]string{`"pool": "bulk"`, `"pool": "../etc"`}, "is not a valid pool name"},
		"nic name":         {[2]string{`"name": "eth0"`, `"name": "data"`}, "not a disk name"},
		"ssh key":          {[2]string{`"ssh-ed25519 AAAA`, `"ssh-foo AAAA`}, "not an OpenSSH public key"},
		"unknown field":    {[2]string{`"autostart": true`, `"autostart": true, "raw": {"lxc": 1}`}, "unknown field"},
		"trailing data":    {[2]string{"}\n}", "}\n}{}"}, "trailing data"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := strings.Replace(validSpec, tc.replace[0], tc.replace[1], 1)
			if spec == validSpec {
				t.Fatalf("replacement %q not found", tc.replace[0])
			}
			_, err := ParseVM(strings.NewReader(spec))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	err := VM{Name: "X"}.Validate()
	for _, want := range []string{"apiVersion", "name:", "resources.cpu", "resources.memory", "disks:"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"1B": 1, "2KiB": 2048, "3MiB": 3 << 20, "20GiB": 20 << 30, "1TiB": 1 << 40} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "1", "1GB", "-1GiB", "1.5GiB", "99999999999999TiB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
