package api

import (
	"fmt"
	"os/user"
	"slices"
	"strconv"
)

// GroupAuthorizer allows root and members of group (primary or
// supplementary). The socket's permissions (group, mode 0660) already limit
// who can connect; this checks again with the caller's uid, so a socket
// with wrong permissions does not open the API.
func GroupAuthorizer(group string) func(uid uint32) error {
	return func(uid uint32) error {
		if uid == 0 {
			return nil
		}
		g, err := user.LookupGroup(group)
		if err != nil {
			return fmt.Errorf("authorize uid %d: %w", uid, err)
		}
		u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
		if err != nil {
			return fmt.Errorf("authorize uid %d: %w", uid, err)
		}
		gids, err := u.GroupIds()
		if err != nil {
			return fmt.Errorf("authorize uid %d: %w", uid, err)
		}
		if u.Gid == g.Gid || slices.Contains(gids, g.Gid) {
			return nil
		}
		return fmt.Errorf("uid %d (%s) is not in group %s", uid, u.Username, group)
	}
}
