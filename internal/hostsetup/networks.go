package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

// NoNetworksSentinel is the network allow-list of a project when no
// network is declared (an empty list would mean "all networks").
const NoNetworksSentinel = "strata-none"

// Network is a declared guest network (ADR-0019).
type Network struct {
	Name string `json:"name"`
	// Uplink is the host NIC the bridge is attached to; "" for an
	// isolated network.
	Uplink string `json:"uplink"`
	// VLAN is the tag on the uplink; 0 for untagged.
	VLAN int `json:"vlan"`
}

// Link describes a host network interface.
type Link struct {
	Exists bool
	// Addrs are its IP addresses.
	Addrs []string
}

// LinkFunc looks up a host network interface.
type LinkFunc func(name string) (Link, error)

// HostLink looks up a host interface with the net package.
func HostLink(name string) (Link, error) {
	if _, err := os.Stat("/sys/class/net/" + name); errors.Is(err, os.ErrNotExist) {
		return Link{}, nil
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return Link{}, fmt.Errorf("interface %s: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return Link{}, fmt.Errorf("interface %s: %w", name, err)
	}
	l := Link{Exists: true}
	for _, a := range addrs {
		l.Addrs = append(l.Addrs, a.String())
	}
	return l, nil
}

// incusNetwork is the part of an Incus network Setup reads.
type incusNetwork struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Managed bool              `json:"managed"`
	Config  map[string]string `json:"config"`
	UsedBy  []string          `json:"used_by"`
}

// desiredConfig returns the Incus configuration of a declared network:
// a bridge without host addresses, attached to its uplink or a VLAN on it.
func desiredConfig(n Network) map[string]string {
	cfg := map[string]string{
		"ipv4.address":               "none",
		"ipv6.address":               "none",
		"bridge.external_interfaces": "",
	}
	switch {
	case n.Uplink == "":
	case n.VLAN > 0:
		v := strconv.Itoa(n.VLAN)
		cfg["bridge.external_interfaces"] = n.Uplink + "." + v + "/" + n.Uplink + "/" + v
	default:
		cfg["bridge.external_interfaces"] = n.Uplink
	}
	return cfg
}

// listNetworks returns the networks Incus knows, managed or not.
func (s *Setup) listNetworks(ctx context.Context) (map[string]incusNetwork, error) {
	var list []incusNetwork
	if err := s.Incus.Do(ctx, http.MethodGet, "/1.0/networks?recursion=1", nil, &list); err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	out := map[string]incusNetwork{}
	for _, n := range list {
		out[n.Name] = n
	}
	return out, nil
}

// ensureNetworks creates or updates the declared networks and returns the
// managed networks that exist afterwards and the refusals.
func (s *Setup) ensureNetworks(ctx context.Context) (map[string]incusNetwork, error) {
	all, err := s.listNetworks(ctx)
	if err != nil {
		return nil, err
	}
	declared := map[string]bool{}
	var errs []error
	for _, want := range s.Config.Networks {
		declared[want.Name] = true
		if err := s.ensureNetwork(ctx, want, all); err != nil {
			errs = append(errs, err)
			s.Log.ErrorContext(ctx, "guest network refused", "network", want.Name, "error", err)
		}
	}
	if all, err = s.listNetworks(ctx); err != nil {
		return nil, errors.Join(append(errs, err)...)
	}
	managed := map[string]incusNetwork{}
	for name, n := range all {
		if !n.Managed {
			continue
		}
		managed[name] = n
		switch {
		case declared[name]:
		case usedByProject(n.UsedBy, s.Config.Project):
			s.Log.WarnContext(ctx, "network is not declared but still used by the project; it stays open "+
				"until its instances are moved or deleted", "network", name)
		default:
			s.Log.WarnContext(ctx, "network is not declared; left untouched and closed to the project", "network", name)
		}
	}
	return managed, errors.Join(errs...)
}

func (s *Setup) ensureNetwork(ctx context.Context, n Network, all map[string]incusNetwork) error {
	if err := s.checkUplink(n); err != nil {
		return err
	}
	want := desiredConfig(n)
	have, exists := all[n.Name]
	if !exists {
		body := map[string]any{"name": n.Name, "type": "bridge", "config": withoutEmpty(want)}
		if err := s.Incus.Do(ctx, http.MethodPost, "/1.0/networks", body, nil); err != nil {
			return fmt.Errorf("network %s: create: %w", n.Name, err)
		}
		s.Log.InfoContext(ctx, "guest network created", "network", n.Name, "uplink", n.Uplink, "vlan", n.VLAN)
		return nil
	}
	if !have.Managed || have.Type != "bridge" {
		return fmt.Errorf("network %s: a host interface or a non-bridge network of this name exists (type %s); not changed",
			n.Name, have.Type)
	}
	var changed []string
	for k, v := range want {
		if have.Config[k] != v {
			changed = append(changed, fmt.Sprintf("%s: %q -> %q", k, have.Config[k], v))
		}
	}
	if len(changed) == 0 {
		return nil
	}
	slices.Sort(changed)
	// PATCH merges; an empty value removes the key.
	body := map[string]any{"config": want}
	if err := s.Incus.Do(ctx, http.MethodPatch, "/1.0/networks/"+url.PathEscape(n.Name), body, nil); err != nil {
		return fmt.Errorf("network %s: update (%s): %w", n.Name, strings.Join(changed, ", "), err)
	}
	s.Log.InfoContext(ctx, "guest network updated", "network", n.Name, "changes", strings.Join(changed, ", "))
	return nil
}

// checkUplink checks that a network's uplink exists, is not the management
// interface and has no IP address (the host must not be on guest
// networks).
func (s *Setup) checkUplink(n Network) error {
	if n.Uplink == "" {
		return nil
	}
	if n.Uplink == s.Config.ManagementInterface {
		return fmt.Errorf("network %s: uplink %s is the management interface", n.Name, n.Uplink)
	}
	l, err := s.Link(n.Uplink)
	if err != nil {
		return fmt.Errorf("network %s: %w", n.Name, err)
	}
	if !l.Exists {
		return fmt.Errorf("network %s: uplink %s does not exist", n.Name, n.Uplink)
	}
	if len(l.Addrs) > 0 {
		return fmt.Errorf("network %s: uplink %s has IP addresses (%s); the host must not be on guest networks",
			n.Name, n.Uplink, strings.Join(l.Addrs, ", "))
	}
	return nil
}

// networkAccess returns the project's network allow-list: the declared
// networks plus undeclared managed networks the project still uses.
func (s *Setup) networkAccess(managed map[string]incusNetwork) string {
	allowed := make([]string, 0, len(s.Config.Networks))
	declared := map[string]bool{}
	for _, n := range s.Config.Networks {
		allowed = append(allowed, n.Name)
		declared[n.Name] = true
	}
	for name, n := range managed {
		if !declared[name] && usedByProject(n.UsedBy, s.Config.Project) {
			allowed = append(allowed, name)
		}
	}
	if len(allowed) == 0 {
		return NoNetworksSentinel
	}
	slices.Sort(allowed)
	return strings.Join(allowed, ",")
}

// usedByProject reports whether instances or profiles of project appear in
// usedBy. Those are what Incus checks before it lets a project lose access
// to a pool or network; cached images and volumes do not block it.
func usedByProject(usedBy []string, project string) bool {
	for _, u := range usedBy {
		if !strings.HasPrefix(u, "/1.0/instances/") && !strings.HasPrefix(u, "/1.0/profiles/") {
			continue
		}
		if _, query, ok := strings.Cut(u, "?"); ok {
			if q, err := url.ParseQuery(query); err == nil && q.Get("project") == project {
				return true
			}
		}
	}
	return false
}

func withoutEmpty(m map[string]string) map[string]string {
	out := maps.Clone(m)
	maps.DeleteFunc(out, func(_, v string) bool { return v == "" })
	return out
}
