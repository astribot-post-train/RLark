package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// StatsPath is the fixed administrator resource snapshot route.
const StatsPath = "/api/v1/container-runtime/stats"

// StatsReader is independent of inventory and workload lifecycle operations.
type StatsReader interface {
	ReadStats(context.Context, agent.LogTarget) (dockerruntime.StatsSnapshot, error)
}

// StatsReadAPI exposes only allocation-bound Docker resource snapshots.
type StatsReadAPI struct {
	Auth   Authenticator
	Reader StatsReader
}

func (a StatsReadAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Request-ID", uuid.NewString())
	if r.URL.Path != StatsPath {
		writeError(w, 404, "not_found", "route not found")
		return
	}
	if a.Auth == nil || a.Reader == nil {
		writeError(w, 503, "unavailable", "resource snapshots unavailable")
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
	snapshot, err := a.Reader.ReadStats(r.Context(), target)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrLogBusy):
			writeError(w, 429, "busy", "a diagnostic read is pending or capacity is full")
		case errors.Is(err, agent.ErrLogTimeout), errors.Is(err, context.DeadlineExceeded):
			writeError(w, 504, "timeout", "Agent diagnostic read timed out; retry")
		case errors.Is(err, agent.ErrIdentity), apierrors.IsNotFound(err):
			writeError(w, 409, "conflict", "allocation or Task identity changed; refresh")
		default:
			writeError(w, 503, "unavailable", "Agent resource snapshot unavailable")
		}
		return
	}
	if _, err := a.Auth.Administrator(r); err != nil {
		writeError(w, 401, "unauthorized", "administrator session expired")
		return
	}
	if snapshot.Validate(time.Now()) != nil {
		writeError(w, 502, "invalid_response", "invalid resource snapshot")
		return
	}
	writeJSON(w, 200, snapshot)
}

// NewStatsProxy permits only the fixed stats path on a literal loopback origin.
func NewStatsProxy(origin string) (http.Handler, error) {
	return newReadProxy(origin, StatsPath, func(data []byte) (any, error) {
		var snapshot dockerruntime.StatsSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, err
		}
		if err := snapshot.Validate(time.Now()); err != nil {
			return nil, err
		}
		return snapshot, nil
	})
}
