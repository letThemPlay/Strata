package workload

import (
	"errors"
	"strings"
	"testing"
)

func baseVM() VM {
	return VM{
		APIVersion: VMAPIVersion,
		Name:       "web-01",
		Resources:  Resources{CPU: 1, Memory: "1GiB"},
		Image:      "nixos",
		Disks: []Disk{
			{Name: "root", Size: "8GiB", Pool: "fast"},
			{Name: "data", Size: "1GiB", Pool: "bulk"},
		},
		Networks: []NIC{{Name: "eth0", Network: "lan"}},
	}
}

func fields(p Plan) string {
	var f []string
	for _, c := range p.Changes {
		f = append(f, c.Field)
	}
	return strings.Join(f, ",")
}

func TestPlanNoChange(t *testing.T) {
	p, err := PlanVM(baseVM(), baseVM(), true)
	if err != nil || !p.Empty() || p.Restart || p.ID == "" {
		t.Errorf("plan %+v %v", p, err)
	}
}

func TestPlanChanges(t *testing.T) {
	want := baseVM()
	want.Resources = Resources{CPU: 2, Memory: "2GiB"}
	want.Disks[0].Size = "10GiB"
	want.Disks[1] = Disk{Name: "logs", Size: "2GiB", Pool: "bulk"} // data removed, logs added
	want.Networks = []NIC{{Name: "eth0", Network: "dmz"}, {Name: "eth1", Network: "lan"}}
	want.Autostart = true
	want.CloudInit = &CloudInit{SSHAuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl a@b"}}

	p, err := PlanVM(baseVM(), want, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := fields(p); got != "autostart,cloudInit.sshAuthorizedKeys,disks.data,disks.logs,disks.root.size,networks.eth0,networks.eth1,resources.cpu,resources.memory" {
		t.Errorf("fields %s", got)
	}
	by := map[string]Change{}
	for _, c := range p.Changes {
		by[c.Field] = c
	}
	if !by["disks.data"].Detaches || !strings.Contains(by["disks.data"].Note, "volume web-01-data is kept") {
		t.Errorf("detach %+v", by["disks.data"])
	}
	if !by["disks.logs"].CreatesVolume || by["disks.root.size"].From != "8GiB" || by["disks.root.size"].To != "10GiB" {
		t.Errorf("disks %+v %+v", by["disks.logs"], by["disks.root.size"])
	}
	if by["autostart"].Restart || by["cloudInit.sshAuthorizedKeys"].Restart || !by["resources.cpu"].Restart {
		t.Errorf("restart flags %+v", p.Changes)
	}
	if !p.Restart {
		t.Error("a running VM must be restarted")
	}
	stopped, _ := PlanVM(baseVM(), want, false)
	if stopped.Restart || stopped.ID == p.ID {
		t.Errorf("stopped plan %+v", stopped)
	}
}

func TestPlanAutostartOnlyNeedsNoRestart(t *testing.T) {
	want := baseVM()
	want.Autostart = true
	p, err := PlanVM(baseVM(), want, true)
	if err != nil || p.Restart || fields(p) != "autostart" {
		t.Errorf("%+v %v", p, err)
	}
}

func TestPlanRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*VM)
		want string
	}{
		"rename":       {func(v *VM) { v.Name = "web-02" }, "cannot be renamed"},
		"image":        {func(v *VM) { v.Image = "debian-13" }, "only used when a VM is created"},
		"image server": {func(v *VM) { v.ImageServer = "images.example.org" }, "only used when a VM is created"},
		"shrink":       {func(v *VM) { v.Disks[0].Size = "4GiB" }, "disks cannot shrink"},
		"move":         {func(v *VM) { v.Disks[1].Pool = "fast" }, "disks cannot be moved"},
		"invalid":      {func(v *VM) { v.Resources.CPU = 0 }, "resources.cpu"},
	} {
		t.Run(name, func(t *testing.T) {
			want := baseVM()
			tc.edit(&want)
			_, err := PlanVM(baseVM(), want, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if name != "invalid" && !errors.Is(err, ErrRefused) {
				t.Errorf("not ErrRefused: %v", err)
			}
		})
	}
}

func TestPlanIDIsStable(t *testing.T) {
	want := baseVM()
	want.Resources.CPU = 2
	a, _ := PlanVM(baseVM(), want, false)
	b, _ := PlanVM(baseVM(), want, false)
	if a.ID != b.ID {
		t.Errorf("unstable id %s %s", a.ID, b.ID)
	}
	want.Resources.CPU = 3
	c, _ := PlanVM(baseVM(), want, false)
	cur := baseVM()
	cur.Autostart = true
	d, _ := PlanVM(cur, want, false)
	if c.ID == a.ID || d.ID == c.ID {
		t.Error("the id must change with either spec")
	}
}

func TestImageServerValidation(t *testing.T) {
	vm := baseVM()
	vm.ImageServer, vm.Image = "images.linuxcontainers.org", "debian/13/cloud"
	if err := vm.Validate(); err != nil {
		t.Errorf("valid remote image: %v", err)
	}
	vm.ImageServer = "https://images.linuxcontainers.org"
	if err := vm.Validate(); err == nil || !strings.Contains(err.Error(), "must be a host name") {
		t.Errorf("URL as server: %v", err)
	}
	vm.ImageServer, vm.Image = "images.linuxcontainers.org", "../etc"
	if err := vm.Validate(); err == nil {
		t.Error("bad remote alias accepted")
	}
}
