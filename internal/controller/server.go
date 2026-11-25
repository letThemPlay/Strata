package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	return s.authorize(mux)
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
