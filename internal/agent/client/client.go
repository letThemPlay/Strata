// Package client is a client of the agent's local API (ADR-0020), used
// by `strata vm` and the controller.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/workload"
)

// DefaultSocket is the agent's local API socket.
const DefaultSocket = "/run/strata/agent.sock"

// Client talks to the agent's local API.
type Client struct {
	Socket string
	// PollInterval and Timeout bound waiting for operations.
	PollInterval time.Duration
	Timeout      time.Duration
	http         *http.Client
}

func (c *Client) init() {
	if c.http != nil {
		return
	}
	if c.PollInterval == 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.Timeout == 0 {
		c.Timeout = 20 * time.Minute
	}
	socket := c.Socket
	c.http = &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

// APIError is an error reply of the agent.
type APIError struct {
	Status int
	api.ErrorResponse
}

func (e *APIError) Error() string { return e.ErrorResponse.Error }

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	c.init()
	req, err := http.NewRequestWithContext(ctx, method, "http://strata-agent"+path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.connectError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	if resp.StatusCode >= 400 {
		e := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(data, &e.ErrorResponse) != nil || e.ErrorResponse.Error == "" {
			e.ErrorResponse.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("malformed reply: %w", err)
		}
	}
	return nil
}

func (c *Client) connectError(err error) error {
	switch {
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("cannot connect to %s: permission denied (members of the %s group may use it; "+
			"log in again after being added)", c.Socket, "strata-operators")
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("cannot connect to %s: the Strata agent's API is not running on this host "+
			"(strata.agent.enable and strata.compute.enable)", c.Socket)
	}
	return fmt.Errorf("cannot connect to %s: %w", c.Socket, err)
}

// ListVMs lists the VMs.
func (c *Client) ListVMs(ctx context.Context) ([]workload.VMStatus, error) {
	var out []workload.VMStatus
	return out, c.do(ctx, http.MethodGet, "/api/v1/vms", nil, &out)
}

// GetVM returns a VM.
func (c *Client) GetVM(ctx context.Context, name string) (workload.VMStatus, error) {
	var out workload.VMStatus
	return out, c.do(ctx, http.MethodGet, "/api/v1/vms/"+url.PathEscape(name), nil, &out)
}

// Host returns what the workload project can use on the node.
func (c *Client) Host(ctx context.Context) (workload.Host, error) {
	var out workload.Host
	return out, c.do(ctx, http.MethodGet, "/api/v1/host", nil, &out)
}

// CreateVM starts creating a VM from a spec and returns the operation.
func (c *Client) CreateVM(ctx context.Context, spec io.Reader) (api.Operation, error) {
	var op api.Operation
	return op, c.do(ctx, http.MethodPost, "/api/v1/vms", spec, &op)
}

// Action starts, stops or restarts a VM and returns the operation.
// timeout < 0 uses the agent's default.
func (c *Client) Action(ctx context.Context, name, action string, force bool, timeout int) (api.Operation, error) {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	if timeout >= 0 {
		q.Set("timeout", strconv.Itoa(timeout))
	}
	var op api.Operation
	path := "/api/v1/vms/" + url.PathEscape(name) + "/" + action
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return op, c.do(ctx, http.MethodPost, path, nil, &op)
}

// Plan returns the agent's plan for a changed spec.
func (c *Client) Plan(ctx context.Context, name, spec string) (workload.Plan, error) {
	var p workload.Plan
	return p, c.do(ctx, http.MethodPut, "/api/v1/vms/"+url.PathEscape(name)+"?plan=true", strings.NewReader(spec), &p)
}

// Apply applies the plan confirmed by its ID and returns the operation.
func (c *Client) Apply(ctx context.Context, name, spec, planID string) (api.Operation, error) {
	var op api.Operation
	path := "/api/v1/vms/" + url.PathEscape(name) + "?" + url.Values{"confirm": {planID}}.Encode()
	return op, c.do(ctx, http.MethodPut, path, strings.NewReader(spec), &op)
}

// Delete deletes a VM (confirmed by its name) and returns the operation.
func (c *Client) Delete(ctx context.Context, name string, stop bool) (api.Operation, error) {
	q := url.Values{"confirm": {name}}
	if stop {
		q.Set("stop", "true")
	}
	var op api.Operation
	return op, c.do(ctx, http.MethodDelete, "/api/v1/vms/"+url.PathEscape(name)+"?"+q.Encode(), nil, &op)
}

// Wait polls an operation until it has finished.
func (c *Client) Wait(ctx context.Context, op api.Operation) (api.Operation, error) {
	c.init()
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	for op.Status != api.StatusSucceeded && op.Status != api.StatusFailed {
		select {
		case <-ctx.Done():
			return op, fmt.Errorf("operation %s still %s: %w", op.ID, op.Status, ctx.Err())
		case <-time.After(c.PollInterval):
		}
		if err := c.do(ctx, http.MethodGet, "/api/v1/operations/"+op.ID, nil, &op); err != nil {
			return op, err
		}
	}
	return op, nil
}
