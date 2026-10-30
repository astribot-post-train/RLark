package agent

import (
	"context"
	"slices"
	"sync"
	"time"

	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/google/uuid"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

// DockerDiagnosticsCapability advertises opt-in allocation-bound reads.
const DockerDiagnosticsCapability = "docker-diagnostics-v1"

// DockerDiagnosticWork identifies a transient read of one exact Task instance.
type DockerDiagnosticWork struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Target LogTarget       `json:"target"`
	Task   rt.TaskIdentity `json:"task"`
}

// DockerDiagnosticRequest is the private mTLS poll/complete protocol.
type DockerDiagnosticRequest struct {
	Action      string                       `json:"action"`
	Work        *DockerDiagnosticWork        `json:"work,omitempty"`
	Logs        *raw.LogSnapshot             `json:"logs,omitempty"`
	Stats       *dockerruntime.StatsSnapshot `json:"stats,omitempty"`
	Unavailable bool                         `json:"unavailable,omitempty"`
}
type dockerDiagnosticResult struct {
	logs        raw.LogSnapshot
	stats       dockerruntime.StatsSnapshot
	unavailable bool
}
type dockerDiagnosticPending struct {
	id        Identity
	work      DockerDiagnosticWork
	result    chan dockerDiagnosticResult
	completed bool
}

// DockerDiagnosticBroker bounds in-flight reads to one per node and 32 total.
// Pending reads are transient; a restart requires the administrator to retry.
type DockerDiagnosticBroker struct {
	mu      sync.Mutex
	pending map[Identity]*dockerDiagnosticPending
}

func (b *DockerDiagnosticBroker) begin(id Identity, w DockerDiagnosticWork) (*dockerDiagnosticPending, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) >= 32 {
		return nil, ErrLogBusy
	}
	for e := range b.pending {
		if e.Namespace == id.Namespace && e.UID == id.UID {
			return nil, ErrLogBusy
		}
	}
	if b.pending == nil {
		b.pending = map[Identity]*dockerDiagnosticPending{}
	}
	p := &dockerDiagnosticPending{id: id, work: w, result: make(chan dockerDiagnosticResult, 1)}
	b.pending[id] = p
	return p, nil
}
func (b *DockerDiagnosticBroker) forget(p *dockerDiagnosticPending) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending[p.id] == p {
		delete(b.pending, p.id)
	}
}
func (b *DockerDiagnosticBroker) poll(id Identity) *DockerDiagnosticWork {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pending[id]
	if p == nil || p.completed {
		return nil
	}
	w := p.work
	return &w
}
func (b *DockerDiagnosticBroker) complete(id Identity, w DockerDiagnosticWork, r dockerDiagnosticResult) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pending[id]
	if p == nil || p.work != w {
		return ErrIdentity
	}
	if !p.completed {
		r.logs.Data = append([]byte(nil), r.logs.Data...)
		p.result <- r
		p.completed = true
	}
	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// DockerDiagnosticsService revalidates retained ownership at each boundary.
type DockerDiagnosticsService struct {
	Tasks  TaskService
	Broker *DockerDiagnosticBroker
}

func (s DockerDiagnosticsService) resolve(ctx context.Context, target LogTarget) (Identity, *Assignment, error) {
	if s.Broker == nil {
		return Identity{}, nil, ErrUnavailable
	}
	id, _, err := (LogService{Tasks: s.Tasks}).resolve(ctx, target)
	if err != nil {
		return id, nil, err
	}
	a, err := s.forIdentity(ctx, id, target)
	return id, a, err
}
func (s DockerDiagnosticsService) forIdentity(ctx context.Context, id Identity, target LogTarget) (*Assignment, error) {
	a, err := (LogService{Tasks: s.Tasks}).forIdentity(ctx, id, target)
	if err != nil {
		return nil, err
	}
	n, err := s.Tasks.nodeForCleanup(ctx, id)
	if err != nil {
		return nil, err
	}
	if n.Spec.Runtime != "Docker" || a.Task.Spec.Runtime.Kind != "Docker" || a.Allocation.Status.CleanupConfirmed || a.Task.Generation != a.Allocation.Spec.Generation || !slices.Contains(n.Status.Capabilities, DockerDiagnosticsCapability) || !slices.Contains(n.Status.Capabilities, DockerExecutionCapability) {
		return nil, ErrIdentity
	}
	return a, nil
}

// ReadLogs returns a bounded Docker stdout/stderr snapshot.
func (s DockerDiagnosticsService) ReadLogs(ctx context.Context, target LogTarget) (raw.LogSnapshot, error) {
	r, err := s.read(ctx, target, "logs")
	return r.logs, err
}

// ReadStats returns one Linux resource sample without storing it in Status.
func (s DockerDiagnosticsService) ReadStats(ctx context.Context, target LogTarget) (dockerruntime.StatsSnapshot, error) {
	r, err := s.read(ctx, target, "stats")
	return r.stats, err
}

func (s DockerDiagnosticsService) read(ctx context.Context, target LogTarget, kind string) (dockerDiagnosticResult, error) {
	id, a, err := s.resolve(ctx, target)
	if err != nil {
		return dockerDiagnosticResult{}, err
	}
	w := DockerDiagnosticWork{ID: uuid.NewString(), Kind: kind, Target: target, Task: rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: a.Task.Namespace, Name: a.Task.Name}, ObjectUID: string(a.Task.UID), Generation: a.Allocation.Spec.Generation}}
	p, err := s.Broker.begin(id, w)
	if err != nil {
		return dockerDiagnosticResult{}, err
	}
	defer s.Broker.forget(p)
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return dockerDiagnosticResult{}, ctx.Err()
	case <-timer.C:
		return dockerDiagnosticResult{}, ErrLogTimeout
	case r := <-p.result:
		if err := ctx.Err(); err != nil {
			return dockerDiagnosticResult{}, err
		}
		if _, err := s.forIdentity(ctx, id, target); err != nil {
			return dockerDiagnosticResult{}, err
		}
		if r.unavailable {
			return dockerDiagnosticResult{}, ErrUnavailable
		}
		return r, nil
	}
}

// Handle accepts only current-node polls and exact, validated completions.
func (s DockerDiagnosticsService) Handle(ctx context.Context, id Identity, req DockerDiagnosticRequest) (*DockerDiagnosticWork, error) {
	if s.Broker == nil {
		return nil, ErrUnavailable
	}
	if _, err := s.Tasks.nodeForCleanup(ctx, id); err != nil {
		return nil, err
	}
	switch req.Action {
	case "poll":
		if req.Work != nil || req.Logs != nil || req.Stats != nil || req.Unavailable {
			return nil, ErrCapability
		}
		w := s.Broker.poll(id)
		if w == nil {
			return nil, nil
		}
		if _, err := s.forIdentity(ctx, id, w.Target); err != nil {
			return nil, err
		}
		return w, nil
	case "complete":
		if req.Work == nil || req.Work.ID == "" || (boolToInt(req.Logs != nil)+boolToInt(req.Stats != nil)+boolToInt(req.Unavailable)) != 1 {
			return nil, ErrCapability
		}
		if req.Work.Kind != "logs" && req.Work.Kind != "stats" {
			return nil, ErrCapability
		}
		if req.Work.Kind == "logs" && (req.Stats != nil || req.Logs != nil && len(req.Logs.Data) > raw.MaxLogBytes) {
			return nil, ErrCapability
		}
		if req.Work.Kind == "stats" && (req.Logs != nil || req.Stats != nil && req.Stats.Validate(time.Now()) != nil) {
			return nil, ErrCapability
		}
		if _, err := s.forIdentity(ctx, id, req.Work.Target); err != nil {
			return nil, err
		}
		r := dockerDiagnosticResult{unavailable: req.Unavailable}
		if req.Logs != nil {
			r.logs = *req.Logs
		}
		if req.Stats != nil {
			r.stats = *req.Stats
		}
		return nil, s.Broker.complete(id, *req.Work, r)
	default:
		return nil, ErrCapability
	}
}

// RespondDockerDiagnostics independently polls the mTLS endpoint and reads the
// exact journal container; no local error/path is returned to the control plane.
func RespondDockerDiagnostics(ctx context.Context, c *Client, reader func(context.Context, string, rt.TaskIdentity) (raw.LogSnapshot, dockerruntime.StatsSnapshot, error)) error {
	w, err := c.DockerDiagnostic(ctx, DockerDiagnosticRequest{Action: "poll"})
	if err != nil || w == nil {
		return err
	}
	if reader == nil || w.ID == "" || w.Task.ObjectUID != w.Target.TaskUID || w.Task.ObjectUID == "" || w.Task.Logical.Validate() != nil || w.Task.Generation < 1 || w.Target.Allocation.Namespace != w.Task.Logical.Namespace || (w.Kind != "logs" && w.Kind != "stats") {
		return ErrCapability
	}
	logs, stats, readErr := reader(ctx, w.Kind, w.Task)
	req := DockerDiagnosticRequest{Action: "complete", Work: w, Unavailable: readErr != nil}
	if readErr == nil {
		switch w.Kind {
		case "logs":
			req.Logs = &logs
		case "stats":
			req.Stats = &stats
		default:
			return ErrCapability
		}
	}
	_, err = c.DockerDiagnostic(ctx, req)
	return err
}

// RuntimeLogService routes log reads only after resolving current ownership.
type RuntimeLogService struct {
	Raw    LogService
	Docker DockerDiagnosticsService
}

// Read dispatches by the validated Task runtime, preserving Raw log semantics.
func (s RuntimeLogService) Read(ctx context.Context, target LogTarget) (raw.LogSnapshot, error) {
	_, a, err := s.Raw.resolve(ctx, target)
	if err != nil {
		return raw.LogSnapshot{}, err
	}
	switch a.Task.Spec.Runtime.Kind {
	case "Raw":
		return s.Raw.Read(ctx, target)
	case "Docker":
		return s.Docker.ReadLogs(ctx, target)
	default:
		return raw.LogSnapshot{}, ErrCapability
	}
}
