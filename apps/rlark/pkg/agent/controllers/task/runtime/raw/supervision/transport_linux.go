//go:build linux

package supervision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/raw"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

type request struct {
	Task rt.TaskIdentity  `json:"task"`
	Spec *raw.ProcessSpec `json:"spec,omitempty"`
}
type response struct {
	Record raw.Record `json:"record"`
	Code   fault.Code `json:"code,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// Serve accepts the local, trusted Agent protocol on a private Unix socket.
// The socket lives in the supervisor's private state directory. It is never a
// public workload API; Gateway authorization must occur before Agent requests.
// Existing sockets are never unlinked or adopted on startup.
func (s *Service) Serve(ctx context.Context) error {
	socket := filepath.Join(s.config.StateDirectory, "supervisor.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }() // Serve/Shutdown normally close it first.
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, Handler: http.HandlerFunc(s.handle)}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx) // Closing the listener below is the fallback.
		case <-done:
		}
	}()
	err = server.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid supervision request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "exactly one request required", http.StatusBadRequest)
		return
	}
	if req.Task.ObjectUID == "" || req.Task.Logical.Validate() != nil {
		http.Error(w, "Task identity required", http.StatusBadRequest)
		return
	}
	var record raw.Record
	var err error
	switch r.URL.Path {
	case "/v1/observe":
		record, err = s.Observe(r.Context(), req.Task)
	case "/v1/ensure":
		if req.Spec == nil {
			http.Error(w, "process spec required", http.StatusBadRequest)
			return
		}
		record, err = s.Ensure(r.Context(), req.Task, *req.Spec)
	case "/v1/stop":
		record, err = s.Stop(r.Context(), req.Task)
	default:
		http.NotFound(w, r)
		return
	}
	result := response{Record: record}
	if err != nil {
		// Never return process arguments, environment or local filesystem errors.
		result.Code, result.Error = fault.Unavailable, "Raw supervision operation failed"
		for _, code := range []fault.Code{fault.NotFound, fault.Conflict, fault.PermissionDenied, fault.InvalidArgument} {
			if fault.Is(err, code) {
				result.Code = code
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	// A disconnected Agent can safely repeat the request from persisted state.
	_ = json.NewEncoder(w).Encode(result)
}

// Client reconnects to an independent supervisor without sharing process state.
type Client struct{ http *http.Client }

var _ raw.Supervisor = (*Client)(nil)

// NewClient constructs a context-aware transport; it does not contact the service.
func NewClient(socket string) (*Client, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return nil, errors.New("canonical absolute supervision socket required")
	}
	return &Client{http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}}, nil
}

func (c *Client) call(ctx context.Context, path string, req request) (raw.Record, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return raw.Record{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://supervisor"+path, bytes.NewReader(data))
	if err != nil {
		return raw.Record{}, err
	}
	res, err := c.http.Do(r)
	if err != nil {
		return raw.Record{}, fault.Wrap(fault.Unavailable, "contact Raw supervisor", err)
	}
	defer func() { _ = res.Body.Close() }() // No reusable request body or owned external effect.
	if res.StatusCode != http.StatusOK {
		return raw.Record{}, fault.New(fault.Unavailable, fmt.Sprintf("Raw supervisor HTTP status %d", res.StatusCode))
	}
	var result response
	if err := json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&result); err != nil {
		return raw.Record{}, err
	}
	if result.Error != "" {
		return raw.Record{}, fault.New(result.Code, result.Error)
	}
	return result.Record, nil
}

// Observe recovers the service's current observation after Agent restart.
func (c *Client) Observe(ctx context.Context, t rt.TaskIdentity) (raw.Record, error) {
	return c.call(ctx, "/v1/observe", request{Task: t})
}

// Ensure starts only once for an immutable Task incarnation.
func (c *Client) Ensure(ctx context.Context, t rt.TaskIdentity, spec raw.ProcessSpec) (raw.Record, error) {
	return c.call(ctx, "/v1/ensure", request{Task: t, Spec: &spec})
}

// Stop requests cleanup without accepting a caller-supplied PID or path.
func (c *Client) Stop(ctx context.Context, t rt.TaskIdentity) (raw.Record, error) {
	return c.call(ctx, "/v1/stop", request{Task: t})
}
