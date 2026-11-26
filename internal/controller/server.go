package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
)

// Server is the controller's local API on its Unix socket (ADR-0025):
// the same caller identification, authorization and audit as the
// agent's.
type Server struct {
	Controller *Controller
	// Authorize returns nil if the caller with uid may use the API.
	Authorize func(uid uint32) error
	Log       *slog.Logger
}

// Handler returns the API's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Controller.Status())
	})
	mux.HandleFunc("POST /api/v1/config/sync", func(w http.ResponseWriter, r *http.Request) {
		s.Controller.Sync(r.Context())
		writeJSON(w, http.StatusOK, s.Controller.Status())
	})
	mux.HandleFunc("GET /api/v1/config/plan", func(w http.ResponseWriter, _ *http.Request) {
		p, err := s.Controller.Plan()
		if err != nil {
			writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: err.Error(), Code: "no_plan"})
			return
		}
		writeJSON(w, http.StatusOK, p)
	})
	mux.HandleFunc("POST /api/v1/config/apply", s.apply)
	mux.HandleFunc("POST /api/v1/config/pin", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Commit string `json:"commit"`
		}
		if !decode(w, r, &req) {
			return
		}
		uid, _ := api.CallerUID(r)
		st, err := s.Controller.Pin(r.Context(), req.Commit, uid)
		respond(w, http.StatusOK, st, err)
	})
	mux.HandleFunc("POST /api/v1/config/unpin", func(w http.ResponseWriter, r *http.Request) {
		uid, _ := api.CallerUID(r)
		st, err := s.Controller.Unpin(r.Context(), uid)
		respond(w, http.StatusOK, st, err)
	})
	mux.HandleFunc("POST /api/v1/config/adopt/{name}", func(w http.ResponseWriter, r *http.Request) {
		uid, _ := api.CallerUID(r)
		st, err := s.Controller.Adopt(r.Context(), r.PathValue("name"), uid)
		respond(w, http.StatusOK, st, err)
	})
	mux.HandleFunc("GET /api/v1/deployments", func(w http.ResponseWriter, r *http.Request) {
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit < 0 {
			limit = 0
		}
		writeJSON(w, http.StatusOK, s.Controller.History(limit))
	})
	mux.HandleFunc("GET /api/v1/deployments/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Controller.Deployment(r.PathValue("id"))
		respond(w, http.StatusOK, d, err)
	})
	// Host configuration (ADR-0027, ADR-0028): evaluation and status only.
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Controller.Nodes())
	})
	mux.HandleFunc("GET /api/v1/nodes/{node}/configuration", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Controller.CheckNode(r.PathValue("node")); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.Controller.HostStatus())
	})
	mux.HandleFunc("POST /api/v1/nodes/{node}/configuration/evaluate", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Controller.CheckNode(r.PathValue("node")); err != nil {
			writeError(w, err)
			return
		}
		uid, _ := api.CallerUID(r)
		ev, err := s.Controller.EvaluateHost(uid) //nolint:contextcheck // The evaluation outlives the request.
		respond(w, http.StatusAccepted, ev, err)
	})
	return s.authorize(mux)
}

// apply approves a plan: {"planId": "...", "allowDelete": bool}.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanID      string `json:"planId"`
		AllowDelete bool   `json:"allowDelete"`
	}
	if !decode(w, r, &req) {
		return
	}
	uid, _ := api.CallerUID(r)
	d, err := s.Controller.Apply(req.PlanID, uid, req.AllowDelete) //nolint:contextcheck // The deployment outlives the request.
	respond(w, http.StatusAccepted, d, err)
}

// decode reads a small JSON request body; unknown fields are refused.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "request: " + err.Error(), Code: "invalid"})
		return false
	}
	return true
}

// respond writes v with code, or the error with its status.
func respond(w http.ResponseWriter, code int, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, code, v)
}

// writeError maps controller errors to HTTP replies.
func writeError(w http.ResponseWriter, err error) {
	code, kind := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, ErrNotAwaiting), errors.Is(err, ErrBusy), errors.Is(err, ErrHostBusy):
		code, kind = http.StatusConflict, "conflict"
	case errors.Is(err, ErrAllowDelete):
		code, kind = http.StatusUnprocessableEntity, "allow_delete"
	case errors.Is(err, ErrInvalidAction):
		code, kind = http.StatusUnprocessableEntity, "invalid"
	case errors.Is(err, ErrNoDeployment), errors.Is(err, ErrNoPlan), errors.Is(err, ErrUnknownNode):
		code, kind = http.StatusNotFound, "not_found"
	}
	writeJSON(w, code, api.ErrorResponse{Error: err.Error(), Code: kind})
}

func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := api.CallerUID(r)
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func(ctx context.Context) {
			s.log().InfoContext(ctx, "audit", "uid", uid, "method", r.Method, "path", r.URL.Path, "status", rec.code)
		}(r.Context())
		var err error
		switch {
		case !ok:
			err = errors.New("caller credentials unavailable")
		case s.Authorize != nil:
			err = s.Authorize(uid)
		}
		if err != nil {
			writeJSON(rec, http.StatusForbidden, api.ErrorResponse{Error: err.Error(), Code: "forbidden"})
			return
		}
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve serves the API on l until ctx is canceled. A synchronization can
// take minutes (evaluation), so writes may take that long.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      15 * time.Minute,
		ConnContext:       api.ConnContext,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return fmt.Errorf("controller API: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			return fmt.Errorf("controller API: shutdown: %w", err)
		}
		return nil
	}
}
