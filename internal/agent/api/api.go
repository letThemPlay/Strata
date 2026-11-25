// Package api is the agent's local API (ADR-0020): versioned HTTP+JSON on
// a Unix socket that systemd creates (group strata-operators, mode 0660)
// and hands to the agent. The strata CLI uses it until the management API
// (M5) exists; its paths match the planned /api/v1 resources.
//
// Every request is authorized by the caller's uid (SO_PEERCRED) and
// written to the audit log. Changes run as operations with an ID; success
// is reported only after the provider has verified the result.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/letthemplay/Strata/internal/workload"
)

// Limits.
const (
	maxBodyBytes     = 64 << 10
	operationTimeout = 15 * time.Minute
	keepOperations   = 1000
	// DefaultStopTimeout is how long a graceful stop waits for the guest.
	DefaultStopTimeout = 60
)

// VMs is a workload provider for VMs (incus.Client implements it).
type VMs interface {
	ListVMs(ctx context.Context) ([]workload.VMStatus, error)
	GetVM(ctx context.Context, name string) (workload.VMStatus, error)
	CreateVM(ctx context.Context, vm workload.VM, meta workload.Meta) error
	VMAction(ctx context.Context, name, action string, force bool, timeoutSeconds int) error
	PlanVM(ctx context.Context, vm workload.VM) (workload.Plan, error)
	UpdateVM(ctx context.Context, vm workload.VM, planID string, stopTimeout int) (workload.Plan, error)
	DeleteVM(ctx context.Context, name string, stop bool, stopTimeout int) ([]string, error)
	Host(ctx context.Context) (workload.Host, error)
	RestoreVM(ctx context.Context, name string, stopTimeout int) error
	SetMeta(ctx context.Context, name string, meta workload.Meta) error
}

// Server is the local API.
type Server struct {
	// Provider returns the VM provider and a function that releases it.
	// It fails when the node has no workload backend.
	Provider func() (VMs, func(), error)
	// Authorize returns nil if the caller with uid may use the API.
	Authorize func(uid uint32) error
	// IsController reports whether uid is the Strata controller, the only
	// caller that may create and change VMs managed through Git (ADR-0026).
	// nil: no controller on this node.
	IsController func(uid uint32) bool
	Log          *slog.Logger

	mu  sync.Mutex
	ops map[string]*Operation
	// busy holds the VMs with an operation in progress.
	busy  map[string]string
	order []string
	// base is the context operations run in (canceled on shutdown).
	base context.Context
}

// Operation states (CLAUDE.md §9). Validation happens before an operation
// is accepted; providers verify within APPLYING.
const (
	StatusPending   = "PENDING"
	StatusApplying  = "APPLYING"
	StatusSucceeded = "SUCCEEDED"
	StatusFailed    = "FAILED"
)

// Operation is a change in progress or done.
type Operation struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Resource    string    `json:"resource"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	CompletedAt time.Time `json:"completedAt,omitzero"`
	// Result is what a successful operation produced: the applied plan
	// (update) or the kept data volumes (delete).
	Result any `json:"result,omitempty"`
	// UID is the caller who requested it.
	UID uint32 `json:"uid"`
}

func (o Operation) done() bool { return o.Status == StatusSucceeded || o.Status == StatusFailed }

// ErrorResponse is the body of every error reply.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// Serve serves the API on l until ctx is canceled, then shuts down
// gracefully (waiting up to 10 seconds for requests in flight).
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.init()
	s.mu.Lock()
	if s.base == nil {
		s.base = ctx
	}
	s.mu.Unlock()
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		ConnContext:       withPeer,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return fmt.Errorf("api: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			return fmt.Errorf("api: shutdown: %w", err)
		}
		return nil
	}
}

func (s *Server) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ops == nil {
		s.ops, s.busy = map[string]*Operation{}, map[string]string{}
	}
	if s.Log == nil {
		s.Log = slog.New(slog.DiscardHandler)
	}
}

// Handler returns the API's HTTP handler.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/vms", s.listVMs)
	mux.HandleFunc("POST /api/v1/vms", s.createVM)
	mux.HandleFunc("GET /api/v1/vms/{name}", s.getVM)
	mux.HandleFunc("PUT /api/v1/vms/{name}", s.updateVM)
	mux.HandleFunc("DELETE /api/v1/vms/{name}", s.deleteVM)
	mux.HandleFunc("POST /api/v1/vms/{name}/{action}", s.vmAction)
	mux.HandleFunc("POST /api/v1/vms/{name}/restore", s.restoreVM)
	mux.HandleFunc("PUT /api/v1/vms/{name}/meta", s.setMeta)
	mux.HandleFunc("GET /api/v1/operations/{id}", s.getOperation)
	mux.HandleFunc("GET /api/v1/host", s.getHost)
	return s.authorize(mux)
}

// peer is the caller's credentials from SO_PEERCRED.
type peer struct {
	uid uint32
	ok  bool
}

type peerKey struct{}

// withPeer reads the caller's credentials from a Unix connection.
func withPeer(ctx context.Context, c net.Conn) context.Context {
	p := peer{}
	if uc, ok := c.(*net.UnixConn); ok {
		if raw, err := uc.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) {
				cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
				if err == nil {
					p = peer{uid: cred.Uid, ok: true}
				}
			})
		}
	}
	return context.WithValue(ctx, peerKey{}, p)
}

func peerOf(r *http.Request) peer {
	p, _ := r.Context().Value(peerKey{}).(peer)
	return p
}

// statusRecorder captures the status code for the audit log.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := peerOf(r)
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func(ctx context.Context) {
			s.Log.InfoContext(ctx, "audit", "uid", p.uid, "method", r.Method, "path", r.URL.Path,
				"status", rec.code, "operation", rec.Header().Get("X-Strata-Operation"))
		}(r.Context())
		switch {
		case !p.ok:
			writeError(rec, http.StatusForbidden, "forbidden", errors.New("caller credentials unavailable"))
			return
		case s.Authorize != nil:
			if err := s.Authorize(p.uid); err != nil {
				writeError(rec, http.StatusForbidden, "forbidden", err)
				return
			}
		}
		r.Body = http.MaxBytesReader(rec, r.Body, maxBodyBytes)
		next.ServeHTTP(rec, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, kind string, err error) {
	writeJSON(w, code, ErrorResponse{Error: err.Error(), Code: kind})
}

// writeProviderError maps provider errors to HTTP replies.
func writeProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workload.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err)
	case errors.Is(err, workload.ErrExists):
		writeError(w, http.StatusConflict, "exists", err)
	case errors.Is(err, workload.ErrGitManaged):
		writeError(w, http.StatusConflict, "git_managed", err)
	case errors.Is(err, workload.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err)
	case errors.Is(err, workload.ErrRefused):
		writeError(w, http.StatusUnprocessableEntity, "refused", err)
	default:
		writeError(w, http.StatusBadGateway, "backend", err)
	}
}

func (s *Server) provider(w http.ResponseWriter) (VMs, func(), bool) {
	if s.Provider == nil {
		writeError(w, http.StatusServiceUnavailable, "no_backend", errors.New("this node has no workload backend (strata.compute)"))
		return nil, nil, false
	}
	p, release, err := s.Provider()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_backend", err)
		return nil, nil, false
	}
	return p, release, true
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	p, release, ok := s.provider(w)
	if !ok {
		return
	}
	defer release()
	list, err := p.ListVMs(r.Context())
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) {
	p, release, ok := s.provider(w)
	if !ok {
		return
	}
	defer release()
	vm, err := p.GetVM(r.Context(), r.PathValue("name"))
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	vm, err := workload.ParseVM(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err)
		return
	}
	meta := workload.Meta{Owner: workload.OwnerCLI}
	q := r.URL.Query()
	switch {
	case q.Get("owner") == workload.OwnerGitOps:
		if !s.controller(r) {
			writeError(w, http.StatusForbidden, "forbidden", errors.New("only the Strata controller may create VMs managed through Git"))
			return
		}
		meta = workload.Meta{Owner: workload.OwnerGitOps, Protected: q.Get("protected") == "true"}
	case q.Get("owner") != "" || q.Get("protected") != "":
		writeError(w, http.StatusBadRequest, "invalid", errors.New("owner may only be gitops, and protected only with it"))
		return
	}
	s.start(w, r, "create", vm.Name, func(ctx context.Context, p VMs) (any, error) {
		return nil, p.CreateVM(ctx, vm, meta)
	})
}

// KeptVolumes is the result of a delete operation.
type KeptVolumes struct {
	// KeptVolumes are the VM's data volumes ("pool/volume"), which are
	// never deleted with it.
	KeptVolumes []string `json:"keptVolumes"`
}

// updateVM returns the plan for a spec (?plan=true) or applies the plan
// confirmed by its ID (?confirm=ID).
func (s *Server) updateVM(w http.ResponseWriter, r *http.Request) {
	vm, err := workload.ParseVM(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err)
		return
	}
	name := r.PathValue("name")
	if vm.Name != name {
		writeError(w, http.StatusBadRequest, "invalid", fmt.Errorf("the spec is for %q, not %q", vm.Name, name))
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("plan") == "true":
		p, release, ok := s.provider(w)
		if !ok {
			return
		}
		defer release()
		if err := notGitManaged(r.Context(), p, name, s.controller(r)); err != nil {
			writeProviderError(w, err)
			return
		}
		plan, err := p.PlanVM(r.Context(), vm)
		if err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
	case q.Get("confirm") != "":
		id := q.Get("confirm")
		controller := s.controller(r)
		s.start(w, r, "update", name, func(ctx context.Context, p VMs) (any, error) {
			if err := notGitManaged(ctx, p, name, controller); err != nil {
				return nil, err
			}
			plan, err := p.UpdateVM(ctx, vm, id, DefaultStopTimeout)
			if err != nil {
				return nil, err //nolint:wrapcheck // Provider errors name the VM and step.
			}
			return plan, nil
		})
	default:
		writeError(w, http.StatusBadRequest, "invalid", errors.New("use ?plan=true to see the plan, then ?confirm=PLAN_ID to apply it"))
	}
}

// deleteVM deletes a VM; ?confirm must repeat its name (typed
// confirmation), and ?stop=true allows stopping a running VM first.
func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	q := r.URL.Query()
	if q.Get("confirm") != name {
		writeError(w, http.StatusBadRequest, "confirmation",
			fmt.Errorf("deleting vm %s removes its root disk: confirm with its name (confirm=%s)", name, name))
		return
	}
	stop := q.Get("stop") == "true"
	controller := s.controller(r)
	s.start(w, r, "delete", name, func(ctx context.Context, p VMs) (any, error) {
		if err := notGitManaged(ctx, p, name, controller); err != nil {
			return nil, err
		}
		kept, err := p.DeleteVM(ctx, name, stop, DefaultStopTimeout)
		if err != nil {
			return nil, err //nolint:wrapcheck // Provider errors name the VM and step.
		}
		return KeptVolumes{KeptVolumes: kept}, nil
	})
}

// notGitManaged refuses changes to a VM that is managed through Git (M3
// decision 6), except by the controller: its spec comes from the
// configuration. Lifecycle actions (start, stop, restart) stay allowed.
func notGitManaged(ctx context.Context, p VMs, name string, controller bool) error {
	if controller {
		return nil
	}
	st, err := p.GetVM(ctx, name)
	if err != nil {
		return err //nolint:wrapcheck // Provider errors name the VM.
	}
	if st.Owner == workload.OwnerGitOps {
		return fmt.Errorf("vm %s: %w: change its declaration in the configuration repository instead", name, workload.ErrGitManaged)
	}
	return nil
}

// getHost returns what the workload project can use on the node.
func (s *Server) getHost(w http.ResponseWriter, r *http.Request) {
	p, release, ok := s.provider(w)
	if !ok {
		return
	}
	defer release()
	h, err := p.Host(r.Context())
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) vmAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	switch action {
	case workload.ActionStart, workload.ActionStop, workload.ActionRestart:
	default:
		writeError(w, http.StatusNotFound, "not_found", fmt.Errorf("unknown action %q", action))
		return
	}
	force := r.URL.Query().Get("force") == "true"
	timeout := DefaultStopTimeout
	if t := r.URL.Query().Get("timeout"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > 3600 {
			writeError(w, http.StatusBadRequest, "invalid", fmt.Errorf("timeout must be 0-3600 seconds, got %q", t))
			return
		}
		timeout = n
	}
	s.start(w, r, action, name, func(ctx context.Context, p VMs) (any, error) {
		return nil, p.VMAction(ctx, name, action, force, timeout)
	})
}

// start runs fn as an operation on VM name and replies 202 with it.
func (s *Server) start(w http.ResponseWriter, r *http.Request, kind, name string, fn func(context.Context, VMs) (any, error)) {
	p, release, ok := s.provider(w)
	if !ok {
		return
	}
	id, err := newID()
	if err != nil {
		release()
		writeError(w, http.StatusInternalServerError, "internal", err)
		return
	}
	op := &Operation{ID: id, Type: kind, Resource: "vms/" + name, Status: StatusPending, CreatedAt: time.Now().UTC(), UID: peerOf(r).uid}

	s.mu.Lock()
	if other, busy := s.busy[name]; busy {
		s.mu.Unlock()
		release()
		writeError(w, http.StatusConflict, "busy", fmt.Errorf("vm %s: operation %s is in progress", name, other))
		return
	}
	s.busy[name] = id
	s.ops[id] = op
	s.order = append(s.order, id)
	s.prune()
	snapshot := *op
	// Operations outlive the request: they run in the server's context
	// (canceled on shutdown), or without the request's cancellation.
	base := s.base //nolint:contextcheck // Deliberate: operations outlive the request (see above).
	if base == nil {
		base = context.WithoutCancel(r.Context())
	}
	s.mu.Unlock()

	go func(base context.Context) {
		defer release()
		ctx, cancel := context.WithTimeout(base, operationTimeout)
		defer cancel()
		s.update(id, func(o *Operation) { o.Status = StatusApplying })
		result, err := fn(ctx, p)
		s.update(id, func(o *Operation) {
			o.CompletedAt = time.Now().UTC()
			if err != nil {
				o.Status, o.Error = StatusFailed, err.Error()
			} else {
				o.Status, o.Result = StatusSucceeded, result
			}
		})
		s.mu.Lock()
		delete(s.busy, name)
		final := *s.ops[id]
		s.mu.Unlock()
		s.Log.InfoContext(ctx, "operation finished", "operation", id, "type", kind, "resource", final.Resource,
			"status", final.Status, "error", final.Error, "uid", final.UID)
	}(base)
	w.Header().Set("X-Strata-Operation", id)
	writeJSON(w, http.StatusAccepted, snapshot)
}

func (s *Server) update(id string, fn func(*Operation)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.ops[id])
}

// prune drops the oldest finished operations beyond keepOperations.
// Called with s.mu held.
func (s *Server) prune() {
	for len(s.order) > keepOperations {
		dropped := false
		for i, id := range s.order {
			if s.ops[id].done() {
				delete(s.ops, id)
				s.order = append(s.order[:i], s.order[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			return
		}
	}
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	op, ok := s.ops[r.PathValue("id")]
	var snapshot Operation
	if ok {
		snapshot = *op
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", errors.New("no such operation"))
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("operation id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ConnContext records the caller's credentials (SO_PEERCRED) of a Unix
// connection in its context; use it as http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context { return withPeer(ctx, c) }

// CallerUID returns the uid of the caller of r, if ConnContext recorded it.
func CallerUID(r *http.Request) (uint32, bool) {
	p := peerOf(r)
	return p.uid, p.ok
}

// controller reports whether the caller of r is the Strata controller.
func (s *Server) controller(r *http.Request) bool {
	uid, ok := CallerUID(r)
	return ok && s.IsController != nil && s.IsController(uid)
}

// controllerOnly refuses r unless its caller is the controller.
func (s *Server) controllerOnly(w http.ResponseWriter, r *http.Request) bool {
	if s.controller(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", errors.New("only the Strata controller may do this"))
	return false
}

// restoreVM puts back what Strata last applied to a drifted VM
// (controller only).
func (s *Server) restoreVM(w http.ResponseWriter, r *http.Request) {
	if !s.controllerOnly(w, r) {
		return
	}
	name := r.PathValue("name")
	s.start(w, r, "restore", name, func(ctx context.Context, p VMs) (any, error) {
		return nil, p.RestoreVM(ctx, name, DefaultStopTimeout)
	})
}

// setMeta changes a VM's owner and protection (controller only: adoption
// and lifecycle changes).
func (s *Server) setMeta(w http.ResponseWriter, r *http.Request) {
	if !s.controllerOnly(w, r) {
		return
	}
	var meta workload.Meta
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", fmt.Errorf("meta: %w", err))
		return
	}
	name := r.PathValue("name")
	s.start(w, r, "meta", name, func(ctx context.Context, p VMs) (any, error) {
		return nil, p.SetMeta(ctx, name, meta)
	})
}

// UserMatcher returns a function reporting whether a uid is the user
// named name (the controller's account). The name is looked up on each
// call, so the user may be created after the agent starts.
func UserMatcher(name string) func(uid uint32) bool {
	return func(uid uint32) bool {
		u, err := user.Lookup(name)
		return err == nil && u.Uid == strconv.FormatUint(uint64(uid), 10)
	}
}
