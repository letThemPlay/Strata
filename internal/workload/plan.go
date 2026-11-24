package workload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Change is one difference between a VM's current and desired spec.
type Change struct {
	// Field names what changes, e.g. "resources.cpu", "disks.data".
	Field string `json:"field"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	// Restart: applying it to a running VM restarts the VM (ADR-0021:
	// changes to a running VM are applied with a stop and a start).
	Restart bool `json:"restart,omitempty"`
	// CreatesVolume: a new data volume is created.
	CreatesVolume bool `json:"createsVolume,omitempty"`
	// Detaches: a data disk is detached; its volume is kept.
	Detaches bool `json:"detaches,omitempty"`
	// Note explains the effect, if not obvious.
	Note string `json:"note,omitempty"`
}

// Plan is the set of changes that turns a VM's current spec into the
// desired one. It is applied only when confirmed by its ID, which
// identifies the current spec, the desired spec and whether the VM runs.
type Plan struct {
	ID      string   `json:"id"`
	VM      string   `json:"vm"`
	Running bool     `json:"running"`
	Changes []Change `json:"changes"`
	// Restart: applying the plan stops and starts the running VM.
	Restart bool `json:"restart"`
}

// Empty reports whether the plan changes nothing.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// PlanVM compares the current and desired spec of a VM. Changes that
// would lose data or cannot be applied (renaming, another image, a disk
// that shrinks or moves to another pool) are refused with ErrRefused.
func PlanVM(current, desired VM, running bool) (Plan, error) {
	if err := desired.Validate(); err != nil {
		return Plan{}, err
	}
	var refused []error
	refuse := func(format string, a ...any) {
		refused = append(refused, fmt.Errorf("%w: "+format, append([]any{ErrRefused}, a...)...))
	}
	if desired.Name != current.Name {
		refuse("name: %q -> %q (a VM cannot be renamed)", current.Name, desired.Name)
	}
	if desired.Image != current.Image || desired.ImageServer != current.ImageServer {
		refuse("image: %q -> %q (the image is only used when a VM is created)", imageRef(current), imageRef(desired))
	}

	var changes []Change
	add := func(c Change) { changes = append(changes, c) }

	if desired.Resources.CPU != current.Resources.CPU {
		add(Change{Field: "resources.cpu", From: strconv.Itoa(current.Resources.CPU), To: strconv.Itoa(desired.Resources.CPU), Restart: true})
	}
	if desired.Resources.Memory != current.Resources.Memory {
		add(Change{Field: "resources.memory", From: current.Resources.Memory, To: desired.Resources.Memory, Restart: true})
	}

	have := map[string]Disk{}
	for _, d := range current.Disks {
		have[d.Name] = d
	}
	want := map[string]Disk{}
	for _, d := range desired.Disks {
		want[d.Name] = d
	}
	for _, d := range desired.Disks {
		field := "disks." + d.Name
		old, ok := have[d.Name]
		if !ok {
			add(Change{
				Field: field, To: d.Size + " on " + d.Pool, Restart: true, CreatesVolume: true,
				Note: "new data volume " + desired.VolumeName(d),
			})
			continue
		}
		if d.Pool != old.Pool {
			refuse("%s: pool %s -> %s (disks cannot be moved)", field, old.Pool, d.Pool)
			continue
		}
		oldSize, errOld := ParseSize(old.Size)
		newSize, errNew := ParseSize(d.Size)
		if errOld != nil || errNew != nil {
			refuse("%s: size %q -> %q", field, old.Size, d.Size)
			continue
		}
		switch {
		case newSize < oldSize:
			refuse("%s: size %s -> %s (disks cannot shrink)", field, old.Size, d.Size)
		case newSize > oldSize:
			add(Change{
				Field: field + ".size", From: old.Size, To: d.Size, Restart: true,
				Note: "the guest must grow its partition and filesystem itself",
			})
		}
	}
	for _, d := range current.Disks {
		if _, ok := want[d.Name]; !ok {
			add(Change{
				Field: "disks." + d.Name, From: d.Size + " on " + d.Pool, Restart: true, Detaches: true,
				Note: "detached; volume " + current.VolumeName(d) + " is kept",
			})
		}
	}

	haveNIC := map[string]string{}
	for _, n := range current.Networks {
		haveNIC[n.Name] = n.Network
	}
	wantNIC := map[string]string{}
	for _, n := range desired.Networks {
		wantNIC[n.Name] = n.Network
	}
	for _, n := range desired.Networks {
		if old, ok := haveNIC[n.Name]; !ok || old != n.Network {
			add(Change{Field: "networks." + n.Name, From: old, To: n.Network, Restart: true})
		}
	}
	for _, n := range current.Networks {
		if _, ok := wantNIC[n.Name]; !ok {
			add(Change{Field: "networks." + n.Name, From: n.Network, Restart: true})
		}
	}

	if desired.Autostart != current.Autostart {
		add(Change{Field: "autostart", From: strconv.FormatBool(current.Autostart), To: strconv.FormatBool(desired.Autostart)})
	}
	if !slices.Equal(sshKeys(current), sshKeys(desired)) {
		add(Change{
			Field: "cloudInit.sshAuthorizedKeys", From: fmt.Sprintf("%d keys", len(sshKeys(current))),
			To: fmt.Sprintf("%d keys", len(sshKeys(desired))), Note: "cloud-init applies it only on a guest's first boot",
		})
	}

	if err := errors.Join(refused...); err != nil {
		return Plan{}, err
	}
	slices.SortStableFunc(changes, func(a, b Change) int { return strings.Compare(a.Field, b.Field) })
	p := Plan{VM: desired.Name, Running: running, Changes: changes}
	for _, c := range changes {
		p.Restart = p.Restart || (running && c.Restart)
	}
	id, err := planID(current, desired, running)
	if err != nil {
		return Plan{}, err
	}
	p.ID = id
	return p, nil
}

// planID identifies a plan by everything it was computed from.
func planID(current, desired VM, running bool) (string, error) {
	a, err := json.Marshal(current)
	if err != nil {
		return "", fmt.Errorf("plan id: %w", err)
	}
	b, err := json.Marshal(desired)
	if err != nil {
		return "", fmt.Errorf("plan id: %w", err)
	}
	h := sha256.New()
	h.Write(a)
	h.Write([]byte{0})
	h.Write(b)
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatBool(running)))
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

func sshKeys(vm VM) []string {
	if vm.CloudInit == nil {
		return nil
	}
	return vm.CloudInit.SSHAuthorizedKeys
}

func imageRef(vm VM) string {
	if vm.ImageServer != "" {
		return vm.ImageServer + ":" + vm.Image
	}
	return vm.Image
}
