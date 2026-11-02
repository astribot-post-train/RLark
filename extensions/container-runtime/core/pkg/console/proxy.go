package console

import (
	"encoding/json"
	"errors"
	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/diagnostics"
	"io"
	"k8s.io/apimachinery/pkg/util/validation"
	"net"
	"net/http"
	"net/url"
	"time"
)

const LogPath = "/api/v1/container-runtime/logs"
const StatsPath = "/api/v1/container-runtime/stats"

var logTarget = ParseDiagnosticTarget

func ParseDiagnosticTarget(r *http.Request) (diagnostics.Target, bool) {
	if len(r.URL.RawQuery) > 4096 {
		return diagnostics.Target{}, false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 4 {
		return diagnostics.Target{}, false
	}
	for _, k := range []string{"namespace", "allocation", "allocationUID", "taskUID"} {
		if len(q[k]) != 1 || q.Get(k) == "" || len(q.Get(k)) > 253 {
			return diagnostics.Target{}, false
		}
	}
	if len(validation.IsDNS1123Label(q.Get("namespace"))) != 0 || len(validation.IsDNS1123Subdomain(q.Get("allocation"))) != 0 {
		return diagnostics.Target{}, false
	}
	return diagnostics.Target{Allocation: ext.NativeObjectReference{Namespace: q.Get("namespace"), Name: q.Get("allocation"), UID: q.Get("allocationUID")}, TaskUID: q.Get("taskUID")}, true
}

// NewLogProxy accepts only a configured loopback HTTP origin. The API read
// listener is private to the control plane and revalidates the browser JWT.
func NewLogProxy(origin string) (http.Handler, error) {
	return newReadProxy(origin, LogPath, func(data []byte) (any, error) {
		var snapshot diagnostics.LogSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil || len(snapshot.Data) > diagnostics.MaxLogBytes {
			return nil, errors.New("invalid log snapshot")
		}
		if snapshot.Data == nil {
			snapshot.Data = []byte{}
		}
		return snapshot, nil
	})
}

func newReadProxy(origin, path string, decode func([]byte) (any, error)) (http.Handler, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("loopback diagnostic API HTTP origin required")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("diagnostic API must use an explicit loopback IP")
	}
	u.Path = path
	c := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != path {
			writeError(w, 404, "not_found", "route not found")
			return
		}
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
			writeError(w, 503, "unavailable", "diagnostic API unavailable")
			return
		}
		req.Header.Set("Authorization", r.Header.Get("Authorization"))
		res, err := c.Do(req)
		if err != nil {
			writeError(w, 503, "unavailable", "diagnostic API unavailable")
			return
		}
		defer func() { _ = res.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(res.Body, 128<<10))
		if err != nil || len(data) >= 128<<10 {
			writeError(w, 502, "invalid_response", "invalid diagnostic API response")
			return
		}
		if res.StatusCode == 200 {
			snapshot, err := decode(data)
			if err != nil {
				writeError(w, 502, "invalid_response", "invalid diagnostic API response")
				return
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

// NewStatsProxy permits only the fixed stats path on a literal loopback origin.
func NewStatsProxy(origin string) (http.Handler, error) {
	return newReadProxy(origin, StatsPath, func(data []byte) (any, error) {
		var snapshot diagnostics.StatsSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, err
		}
		if err := snapshot.Validate(time.Now()); err != nil {
			return nil, err
		}
		return snapshot, nil
	})
}
