package console

import (
	"context"
	"errors"
	management "github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/console"
	"net/http"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// LogPath is a single read-only route. It cannot proxy arbitrary API resources.
const LogPath = management.LogPath

// LogReader is injected separately from inventory and process control.
type LogReader interface {
	Read(context.Context, agent.LogTarget) (raw.LogSnapshot, error)
}

// LogReadAPI serves the API process's loopback-only administrator log listener.
// The main Agent listener retains mandatory mTLS and has no browser JWT route.
type LogReadAPI struct {
	Auth   Authenticator
	Reader LogReader
}

var logTarget = management.ParseDiagnosticTarget

func (a LogReadAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Request-ID", uuid.NewString())
	if r.URL.Path != LogPath {
		writeError(w, 404, "not_found", "route not found")
		return
	}
	if a.Auth == nil || a.Reader == nil {
		writeError(w, 503, "unavailable", "log reads unavailable")
		return
	}
	if _, err := a.Auth.Administrator(r); err != nil {
		status := 401
		if errors.Is(err, errForbidden) {
			status = 403
		}
		writeError(w, status, "unauthorized", "administrator access required")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, 405, "method_not_allowed", "GET required")
		return
	}
	target, ok := logTarget(r)
	if !ok {
		writeError(w, 400, "invalid_request", "exact allocation and Task identity required")
		return
	}
	snapshot, err := a.Reader.Read(r.Context(), target)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrLogBusy):
			writeError(w, 429, "busy", "a log read is already pending or read capacity is full")
		case errors.Is(err, agent.ErrLogTimeout), errors.Is(err, context.DeadlineExceeded):
			writeError(w, 504, "timeout", "Agent log read timed out; retry the read")
		case errors.Is(err, agent.ErrIdentity), apierrors.IsNotFound(err):
			writeError(w, 409, "conflict", "allocation or Task identity changed; refresh")
		default:
			writeError(w, 503, "unavailable", "Agent logs unavailable")
		}
		return
	}
	// Waiting cannot extend JWT lifetime or return payload under a revoked token.
	if _, err := a.Auth.Administrator(r); err != nil {
		writeError(w, 401, "unauthorized", "administrator session expired")
		return
	}
	if len(snapshot.Data) > raw.MaxLogBytes {
		writeError(w, 502, "invalid_response", "invalid log response")
		return
	}
	if snapshot.Data == nil {
		snapshot.Data = []byte{}
	}
	writeJSON(w, 200, snapshot)
}
