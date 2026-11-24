package hostsetup

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// checkPins checks that every pinned interface name (ADR-0011) exists and
// carries its MAC address. A pin that does not match means the NIC was
// replaced or removed, or the name was taken by another NIC: the host's
// network configuration for it is not active.
func (s *Setup) checkPins() error {
	names := make([]string, 0, len(s.Config.Interfaces))
	for name := range s.Config.Interfaces {
		names = append(names, name)
	}
	slices.Sort(names)
	var errs []error
	for _, name := range names {
		want := s.Config.Interfaces[name]
		mac, err := fs.ReadFile(s.Root, "sys/class/net/"+name+"/address")
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("interface %s (pinned to %s) does not exist: the NIC was removed or replaced", name, want))
		case strings.TrimSpace(strings.ToLower(string(mac))) != want:
			errs = append(errs, fmt.Errorf("interface %s has MAC address %s, but is pinned to %s", name,
				strings.TrimSpace(string(mac)), want))
		}
	}
	return errors.Join(errs...)
}
