package incus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/letthemplay/Strata/internal/workload"
)

// RestoreVM repairs drift: it puts back the spec and meta Strata last
// applied to the VM (recorded on the instance), replacing whatever was
// changed outside Strata, and verifies the result. A running VM is stopped
// gracefully first and started again afterwards, as for an update
// (ADR-0021). A VM without drift is left alone.
func (c *Client) RestoreVM(ctx context.Context, name string, stopTimeout int) error {
	inst, err := c.getInstance(ctx, name)
	if err != nil {
		return err
	}
	st, err := status(inst)
	if err != nil {
		return err
	}
	if len(st.Drift) == 0 {
		return nil
	}
	running := inst.Status == "Running"
	if running {
		if err := c.VMAction(ctx, name, workload.ActionStop, false, stopTimeout); err != nil {
			return fmt.Errorf("vm %s: stop before restoring: %w", name, err)
		}
	}
	if err := c.putInstance(ctx, st.Spec, st.Meta); err != nil {
		err = fmt.Errorf("vm %s: restore: %w", name, err)
		if running {
			if serr := c.VMAction(context.WithoutCancel(ctx), name, workload.ActionStart, false, 0); serr != nil {
				err = errors.Join(err, fmt.Errorf("vm %s: start again after the failed restore: %w", name, serr))
			}
		}
		return err
	}
	if running {
		if err := c.VMAction(ctx, name, workload.ActionStart, false, 0); err != nil {
			return fmt.Errorf("vm %s: start after restoring: %w", name, err)
		}
	}
	inst, err = c.getInstance(ctx, name)
	if err != nil {
		return err
	}
	return verify(st.Spec, st.Meta, inst)
}

// SetMeta changes a VM's owner or protection (adoption by the controller,
// a changed lifecycle). The VM must not have drifted. Its spec and state
// are unchanged.
func (c *Client) SetMeta(ctx context.Context, name string, meta workload.Meta) error {
	if meta.Owner != workload.OwnerCLI && meta.Owner != workload.OwnerGitOps {
		return fmt.Errorf("vm %s: %w: unknown owner %q", name, workload.ErrRefused, meta.Owner)
	}
	_, st, err := c.managed(ctx, name)
	if err != nil {
		return err
	}
	if st.Meta == meta {
		return nil
	}
	if err := c.putInstance(ctx, st.Spec, meta); err != nil {
		return fmt.Errorf("vm %s: set owner and protection: %w", name, err)
	}
	inst, err := c.getInstance(ctx, name)
	if err != nil {
		return err
	}
	return verify(st.Spec, meta, inst)
}

// Host returns what the project can use: storage pools, managed networks
// and custom volumes. Incus lists only what the restricted identity may
// see, so this is what workloads can actually reference.
func (c *Client) Host(ctx context.Context) (workload.Host, error) {
	h := workload.Host{Pools: []string{}, Networks: []string{}, Volumes: []string{}}
	var pools []string
	if err := c.Do(ctx, http.MethodGet, "/1.0/storage-pools", nil, &pools); err != nil {
		return workload.Host{}, fmt.Errorf("list storage pools: %w", err)
	}
	for _, p := range pools {
		name, err := lastSegment(p)
		if err != nil {
			return workload.Host{}, err
		}
		h.Pools = append(h.Pools, name)
	}
	var nets []struct {
		Name    string `json:"name"`
		Managed bool   `json:"managed"`
	}
	if err := c.Do(ctx, http.MethodGet, "/1.0/networks?recursion=1", nil, &nets); err != nil {
		return workload.Host{}, fmt.Errorf("list networks: %w", err)
	}
	for _, n := range nets {
		if n.Managed {
			h.Networks = append(h.Networks, n.Name)
		}
	}
	for _, pool := range h.Pools {
		var vols []string
		if err := c.Do(ctx, http.MethodGet, "/1.0/storage-pools/"+url.PathEscape(pool)+"/volumes/custom", nil, &vols); err != nil {
			return workload.Host{}, fmt.Errorf("list volumes on pool %s: %w", pool, err)
		}
		for _, v := range vols {
			name, err := lastSegment(v)
			if err != nil {
				return workload.Host{}, err
			}
			h.Volumes = append(h.Volumes, pool+"/"+name)
		}
	}
	slices.Sort(h.Pools)
	slices.Sort(h.Networks)
	slices.Sort(h.Volumes)
	return h, nil
}

// lastSegment returns the unescaped name at the end of an Incus URL such as
// "/1.0/storage-pools/fast?project=strata".
func lastSegment(u string) (string, error) {
	p, _, _ := strings.Cut(u, "?")
	name, err := url.PathUnescape(path.Base(p))
	if err != nil || name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("unexpected Incus URL %q", u)
	}
	return name, nil
}
