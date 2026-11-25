// Package ir defines Strata's intermediate representation (IR) of desired
// infrastructure, ir.strata.dev/v1alpha1 (ADR-0023).
//
// Nix evaluation produces the IR (`config.strata.ir` of a host
// configuration); Go never reads Nix. An IR document describes one node:
// the host objects the workloads were validated against (pools, networks,
// image servers, project limits) and the declared resources, each with a
// stable ID, its type, its dependencies, its lifecycle policy and its spec
// in the workload's own schema (vm.strata.dev/v1alpha1 for VMs).
//
// The IR holds desired state only. Where it came from (repository, commit,
// flake.lock) is recorded by the controller around it.
package ir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/letthemplay/Strata/internal/workload"
)

// APIVersion is the IR schema version this package reads.
const APIVersion = "ir.strata.dev/v1alpha1"

// TypeVM is the resource type of a virtual machine.
const TypeVM = "vm"

// maxBytes bounds an IR document (about 30 maximal VM specs per MiB).
const maxBytes = 8 << 20

// Document is an IR document for one node.
type Document struct {
	APIVersion string     `json:"apiVersion"`
	Generator  Generator  `json:"generator"`
	Node       string     `json:"node"`
	Host       Host       `json:"host"`
	Resources  []Resource `json:"resources"`
}

// Generator identifies what produced the document.
type Generator struct {
	StrataVersion string `json:"strataVersion"`
}

// Host is the node's declared configuration that workloads depend on, as
// it was when the IR was produced. It is also validation metadata: the IR
// is checked against it on its own, and against the observed host when
// planned.
type Host struct {
	Project      string    `json:"project"`
	Pools        []Pool    `json:"pools"`
	DefaultPool  string    `json:"defaultPool,omitempty"`
	Networks     []Network `json:"networks"`
	ImageServers []string  `json:"imageServers"`
	Limits       Limits    `json:"limits"`
}

// Pool is a storage pool declared on the host.
type Pool struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

// Network is a guest network declared on the host.
type Network struct {
	Name string `json:"name"`
}

// Limits are the project's aggregate limits; zero values mean no limit.
type Limits struct {
	CPU       int    `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Instances int    `json:"instances,omitempty"`
}

// Resource is one declared resource.
type Resource struct {
	// ID is TYPE/NAME, stable for the resource's lifetime.
	ID   string `json:"id"`
	Type string `json:"type"`
	// DependsOn lists the IDs of the host objects (pool/NAME,
	// network/NAME) the resource uses, sorted.
	DependsOn []string        `json:"dependsOn"`
	Lifecycle Lifecycle       `json:"lifecycle"`
	Spec      json.RawMessage `json:"spec"`

	// VM is the decoded spec of a resource of type vm.
	VM *workload.VM `json:"-"`
}

// Lifecycle is a resource's lifecycle policy.
type Lifecycle struct {
	// Protected resources are never deleted or restarted by Strata until
	// the flag is cleared and applied.
	Protected bool `json:"protected"`
}

// Parse decodes and validates an IR document. Unknown fields are rejected,
// at the top level and in specs. Every problem found is reported.
func Parse(r io.Reader) (*Document, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read IR: %w", err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("IR exceeds %d bytes", maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("IR: %w", err)
	}
	if dec.More() {
		return nil, errors.New("IR: trailing data after the JSON object")
	}
	if doc.APIVersion != APIVersion {
		// Nothing else can be interpreted under another version.
		return nil, fmt.Errorf("IR: apiVersion must be %q, got %q", APIVersion, doc.APIVersion)
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return &doc, nil
}
