package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
)

// LogPath is a single read-only route. It cannot proxy arbitrary API resources.
const LogPath = "/api/v1/container-runtime/logs"

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

func logTarget(r *http.Request) (agent.LogTarget, bool) {
	if len(r.URL.RawQuery) > 4096 {
		return agent.LogTarget{}, false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 4 {
		return agent.LogTarget{}, false
	}
	for _, k := range []string{"namespace", "allocation", "allocationUID", "taskUID"} {
		if len(q[k]) != 1 || q.Get(k) == "" || len(q.Get(k)) > 253 {
			return agent.LogTarget{}, false
		}
	}
	if len(validation.IsDNS1123Label(q.Get("namespace"))) != 0 || len(validation.IsDNS1123Subdomain(q.Get("allocation"))) != 0 {
		return agent.LogTarget{}, false
	}
	return agent.LogTarget{Allocation: ext.NativeObjectReference{Namespace: q.Get("namespace"), Name: q.Get("allocation"), UID: q.Get("allocationUID")}, TaskUID: q.Get("taskUID")}, true
}

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

// NewLogProxy accepts only a configured loopback HTTP origin. The API read
// listener is private to the control plane and revalidates the browser JWT.
func NewLogProxy(origin string) (http.Handler, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("loopback log API HTTP origin required")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("log API must use an explicit loopback IP")
	}
	u.Path = LogPath
	c := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, 405, "method_not_allowed", "GET required")
			return
		}
		if _, ok := logTarget(r); !ok {
			writeError(w, 400, "invalid_request", "exact allocation and Task identity required")
			return
		}
		u := *u
		u.RawQuery = r.URL.RawQuery
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
		if err != nil {
			writeError(w, 503, "unavailable", "log API unavailable")
			return
		}
		req.Header.Set("Authorization", r.Header.Get("Authorization"))
		res, err := c.Do(req)
		if err != nil {
			writeError(w, 503, "unavailable", "log API unavailable")
			return
		}
		defer func() { _ = res.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(res.Body, 128<<10))
		if err != nil || len(data) >= 128<<10 {
			writeError(w, 502, "invalid_response", "invalid log API response")
			return
		}
		if res.StatusCode == 200 {
			var snapshot raw.LogSnapshot
			d := json.Unmarshal(data, &snapshot)
			if d != nil || len(snapshot.Data) > raw.MaxLogBytes {
				writeError(w, 502, "invalid_response", "invalid log API response")
				return
			}
			if snapshot.Data == nil {
				snapshot.Data = []byte{}
			}
			writeJSON(w, 200, snapshot)
			return
		}
		// Never relay arbitrary upstream bodies, cookies, redirects or headers.
		status := res.StatusCode
		if status != 400 && status != 401 && status != 403 && status != 409 && status != 429 && status != 504 {
			status = 503
		}
		writeError(w, status, "log_read_failed", http.StatusText(status))
	}), nil
}
