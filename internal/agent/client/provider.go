package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/workload"
)

// Provider is the agent's API as a workload provider for the reconciler
// (internal/reconciler.Provider): every change starts an operation and
// waits for it, so it returns only when the agent has verified the result
// or failed. Used by the controller, whose identity the agent recognizes
// (ADR-0026).
type Provider struct {
	Client *Client
}

// ListVMs lists the VMs.
func (p Provider) ListVMs(ctx context.Context) ([]workload.VMStatus, error) {
	return p.Client.ListVMs(ctx)
}

// Host returns what the workload project can use on the node.
func (p Provider) Host(ctx context.Context) (workload.Host, error) { return p.Client.Host(ctx) }

// run starts an operation and waits for it; a failed operation is an
// error.
func (p Provider) run(ctx context.Context, method, path string, body any) (api.Operation, error) {
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return api.Operation{}, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	var op api.Operation
	var err error
	if reader != nil {
		err = p.Client.do(ctx, method, path, reader, &op)
	} else {
		err = p.Client.do(ctx, method, path, nil, &op) // a nil *bytes.Reader is not a nil io.Reader
	}
	if err != nil {
		return op, err
	}
	op, err = p.Client.Wait(ctx, op)
	if err != nil {
		return op, err
	}
	if op.Status != api.StatusSucceeded {
		return op, errors.New(op.Error)
	}
	return op, nil
}

func vmPath(name string) string { return "/api/v1/vms/" + url.PathEscape(name) }

// CreateVM creates a VM with the given meta (owned by Git: controller only).
func (p Provider) CreateVM(ctx context.Context, vm workload.VM, meta workload.Meta) error {
	q := url.Values{}
	if meta.Owner == workload.OwnerGitOps {
		q.Set("owner", workload.OwnerGitOps)
		if meta.Protected {
			q.Set("protected", "true")
		}
	}
	path := "/api/v1/vms"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	_, err := p.run(ctx, http.MethodPost, path, vm)
	return err
}

// VMAction starts, stops or restarts a VM.
func (p Provider) VMAction(ctx context.Context, name, action string, force bool, timeoutSeconds int) error {
	q := url.Values{"timeout": {strconv.Itoa(timeoutSeconds)}}
	if force {
		q.Set("force", "true")
	}
	_, err := p.run(ctx, http.MethodPost, vmPath(name)+"/"+url.PathEscape(action)+"?"+q.Encode(), nil)
	return err
}

// UpdateVM applies the VM's plan with ID planID. The agent uses its
// default stop timeout.
func (p Provider) UpdateVM(ctx context.Context, vm workload.VM, planID string, _ int) (workload.Plan, error) {
	op, err := p.run(ctx, http.MethodPut, vmPath(vm.Name)+"?"+url.Values{"confirm": {planID}}.Encode(), vm)
	if err != nil {
		return workload.Plan{}, err
	}
	var plan workload.Plan
	if data, err := json.Marshal(op.Result); err == nil {
		_ = json.Unmarshal(data, &plan)
	}
	return plan, nil
}

// RestoreVM puts back what Strata last applied (controller only).
func (p Provider) RestoreVM(ctx context.Context, name string, _ int) error {
	_, err := p.run(ctx, http.MethodPost, vmPath(name)+"/restore", nil)
	return err
}

// SetMeta changes a VM's owner and protection (controller only).
func (p Provider) SetMeta(ctx context.Context, name string, meta workload.Meta) error {
	_, err := p.run(ctx, http.MethodPut, vmPath(name)+"/meta", meta)
	return err
}

// DeleteVM deletes a VM and returns its kept data volumes.
func (p Provider) DeleteVM(ctx context.Context, name string, stop bool, _ int) ([]string, error) {
	q := url.Values{"confirm": {name}}
	if stop {
		q.Set("stop", "true")
	}
	op, err := p.run(ctx, http.MethodDelete, vmPath(name)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var kept api.KeptVolumes
	if data, err := json.Marshal(op.Result); err == nil {
		_ = json.Unmarshal(data, &kept)
	}
	return kept.KeptVolumes, nil
}
