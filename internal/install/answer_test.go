package install

import (
	"strings"
	"testing"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl admin@ws"

const staticAnswers = `{
  "apiVersion": "install.strata.dev/v1alpha1",
  "node": {"id": "hv-01"},
  "admin": {"name": "ops", "authorizedKeys": ["` + testKey + `"]},
  "disk": {"device": "/dev/disk/by-id/virtio-system", "layout": "ext4"},
  "network": {
    "interface": "eno1",
    "addressing": "static",
    "addresses": ["192.0.2.10/24", "2001:db8::10/64"],
    "gateway4": "192.0.2.1",
    "gateway6": "2001:db8::1",
    "dns": ["192.0.2.53", "2001:db8::53"]
  }
}`

const dhcpAnswers = `{
  "apiVersion": "install.strata.dev/v1alpha1",
  "node": {"id": "hv-02"},
  "admin": {"authorizedKeys": ["` + testKey + `"]},
  "disk": {"device": "/dev/vda"},
  "network": {"interface": "eth0", "addressing": "dhcp"}
}`

func mustParse(t *testing.T, s string) Answers {
	t.Helper()
	a, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return a
}

func TestParseStatic(t *testing.T) {
	a := mustParse(t, staticAnswers)
	if a.Node.ID != "hv-01" || a.Admin.Name != "ops" || a.Disk.Device != "/dev/disk/by-id/virtio-system" {
		t.Errorf("unexpected answers: %+v", a)
	}
	if got := len(a.Network.Addresses); got != 2 {
		t.Errorf("addresses: got %d, want 2", got)
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	a := mustParse(t, dhcpAnswers)
	if a.Admin.Name != DefaultAdminName {
		t.Errorf("admin.name: got %q, want %q", a.Admin.Name, DefaultAdminName)
	}
	if a.Disk.Layout != DefaultLayout {
		t.Errorf("disk.layout: got %q, want %q", a.Disk.Layout, DefaultLayout)
	}
}

func TestParseRejects(t *testing.T) {
	// Each case replaces one substring of a valid answer file.
	tests := []struct {
		name, base, old, new, wantErr string
	}{
		{"wrong apiVersion", dhcpAnswers, "v1alpha1", "v2", "apiVersion"},
		{"unknown field", dhcpAnswers, `"node": {"id": "hv-02"}`, `"node": {"id": "hv-02", "role": "x"}`, "unknown field"},
		{"uppercase node id", dhcpAnswers, `"hv-02"`, `"HV-02"`, "node.id"},
		{"node id with trailing hyphen", dhcpAnswers, `"hv-02"`, `"hv-"`, "node.id"},
		{"root admin", staticAnswers, `"name": "ops"`, `"name": "root"`, "must not be root"},
		{"invalid admin name", staticAnswers, `"name": "ops"`, `"name": "Ops User"`, "admin.name"},
		{"no keys", dhcpAnswers, `["` + testKey + `"]`, `[]`, "at least one key"},
		{"malformed key", dhcpAnswers, testKey, "not-a-key", "authorizedKeys[0]"},
		{"key with newline", dhcpAnswers, "admin@ws", `admin@ws\nssh-ed25519 AAAA other`, "authorizedKeys[0]"},
		{"device outside /dev", dhcpAnswers, `"/dev/vda"`, `"/tmp/vda"`, "disk.device"},
		{"device with traversal", dhcpAnswers, `"/dev/vda"`, `"/dev/../etc/passwd"`, "disk.device"},
		{"device with quote", dhcpAnswers, `"/dev/vda"`, `"/dev/vda\"x"`, "disk.device"},
		{"unknown layout", staticAnswers, `"layout": "ext4"`, `"layout": "zfs"`, "disk.layout"},
		{"bad interface", dhcpAnswers, `"eth0"`, `"this-name-is-too-long"`, "network.interface"},
		{"unknown addressing", dhcpAnswers, `"dhcp"`, `"slaac"`, "network.addressing"},
		{"static without addresses", staticAnswers, `["192.0.2.10/24", "2001:db8::10/64"]`, `[]`, "at least one address"},
		{"address without prefix", staticAnswers, `"192.0.2.10/24"`, `"192.0.2.10"`, "addresses[0]"},
		{"ipv6 gateway4", staticAnswers, `"gateway4": "192.0.2.1"`, `"gateway4": "2001:db8::1"`, "gateway4"},
		{"ipv4 gateway6", staticAnswers, `"gateway6": "2001:db8::1"`, `"gateway6": "192.0.2.1"`, "gateway6"},
		{"bad dns", staticAnswers, `"192.0.2.53"`, `"dns.example"`, "dns[0]"},
		{"dhcp with addresses", dhcpAnswers, `"addressing": "dhcp"`, `"addressing": "dhcp", "addresses": ["192.0.2.10/24"]`, "only allowed with static"},
		{"trailing data", dhcpAnswers, `"dhcp"}`, `"dhcp"}} {}`, ""},
		{"not json", dhcpAnswers, dhcpAnswers, "node: hv-02", "answer file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(tt.base, tt.old) {
				t.Fatalf("test bug: %q not found in base", tt.old)
			}
			_, err := Parse(strings.NewReader(strings.Replace(tt.base, tt.old, tt.new, 1)))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	err := Answers{}.Validate()
	if err == nil {
		t.Fatal("Validate succeeded on empty answers")
	}
	for _, field := range []string{"apiVersion", "node.id", "admin.name", "admin.authorizedKeys", "disk.device", "disk.layout", "network.interface", "network.addressing"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error does not mention %s:\n%v", field, err)
		}
	}
}
