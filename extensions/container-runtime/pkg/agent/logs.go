package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// ErrLogBusy bounds concurrent reads instead of retaining unbounded queues.
	ErrLogBusy    = errors.New("container log read capacity exhausted")
	ErrLogTimeout = errors.New("container log read timed out")
)

// LogTarget pins an administrator read to one retained workload allocation.
type LogTarget struct {
	Allocation ext.NativeObjectReference `json:"allocation"`
	TaskUID    string                    `json:"taskUID"`
}

// LogWork is an ephemeral read request, never durable desired workload state.
type LogWork struct {
	ID     string          `json:"id"`
	Target LogTarget       `json:"target"`
	Task   rt.TaskIdentity `json:"task"`
}

type logPending struct {
	identity  Identity
	work      LogWork
	result    chan logResult
	completed bool
}
type logResult struct {
	snapshot    raw.LogSnapshot
	unavailable bool
}

// LogBroker holds bounded, short-lived reads in one API process. Losing a read
// across restart is harmless: the caller retries. No log history is retained.
type LogBroker struct {
	mu      sync.Mutex
	pending map[Identity]*logPending
}

func (b *LogBroker) begin(id Identity, work LogWork) (*logPending, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) >= 32 {
		return nil, ErrLogBusy
	}
	for existing := range b.pending {
		if existing.Namespace == id.Namespace && existing.UID == id.UID {
			return nil, ErrLogBusy
		}
	}
	if b.pending == nil {
		b.pending = make(map[Identity]*logPending)
	}
	p := &logPending{identity: id, work: work, result: make(chan logResult, 1)}
	b.pending[id] = p
	return p, nil
}
func (b *LogBroker) forget(p *logPending) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending[p.identity] == p {
		delete(b.pending, p.identity)
	}
}
func (b *LogBroker) poll(id Identity) *LogWork {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pending[id]
	if p == nil || p.completed {
		return nil
	}
	w := p.work
	return &w
}
func (b *LogBroker) complete(id Identity, work LogWork, result logResult) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pending[id]
	if p == nil || p.work != work {
		return ErrIdentity
	}
	if !p.completed {
		result.snapshot.Data = append([]byte(nil), result.snapshot.Data...)
		p.result <- result
		p.completed = true
	}
	return nil
}

// LogService rechecks current allocation ownership at every transport boundary.
// Unlike start admission, it permits diagnostics during suspension or cordon.
type LogService struct {
	Tasks  TaskService
	Broker *LogBroker
}

func (s LogService) resolve(ctx context.Context, target LogTarget) (Identity, *Assignment, error) {
	c := s.Tasks.Reporter.Client
	if c == nil || target.Allocation.Namespace == "" || target.Allocation.Name == "" || target.Allocation.UID == "" || target.TaskUID == "" {
		return Identity{}, nil, ErrIdentity
	}
	var a ext.ContainerAllocation
	if err := c.Get(ctx, client.ObjectKey{Namespace: target.Allocation.Namespace, Name: target.Allocation.Name}, &a); err != nil {
		return Identity{}, nil, err
	}
	if objectRef(&a) != target.Allocation || a.Spec.Task.UID != target.TaskUID || a.Spec.Upgrade != nil {
		return Identity{}, nil, ErrIdentity
	}
	var nodes ext.ContainerNodeList
	if err := c.List(ctx, &nodes, client.InNamespace(a.Namespace)); err != nil {
		return Identity{}, nil, err
	}
	var node *ext.ContainerNode
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if string(n.UID) == a.Spec.NodeRecordUID {
			if node != nil {
				return Identity{}, nil, ErrIdentity
			}
			node = n
		}
	}
	if node == nil {
		return Identity{}, nil, ErrIdentity
	}
	id := Identity{Namespace: node.Namespace, Name: node.Name, UID: string(node.UID), ClusterUID: node.Spec.Cluster.UID, InstanceID: node.Spec.InstanceID, TenantID: tenant.IDOf(node), CredentialID: node.Annotations[CredentialAnnotation]}
	assignment, err := s.forIdentity(ctx, id, target)
	return id, assignment, err
}

func (s LogService) forIdentity(ctx context.Context, id Identity, target LogTarget) (*Assignment, error) {
	a, err := s.Tasks.assignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil || objectRef(&a.Allocation) != target.Allocation || string(a.Task.UID) != target.TaskUID {
		return nil, ErrIdentity
	}
	c := s.Tasks.Reporter.Client
	var job native.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: a.Allocation.Spec.Job.Namespace, Name: a.Allocation.Spec.Job.Name}, &job); err != nil {
		return nil, err
	}
	if objectRef(&job) != a.Allocation.Spec.Job || tenant.IDOf(&job) != id.TenantID || job.Annotations[nativejob.ControllerOwnerAnnotation] != ext.Group {
		return nil, ErrIdentity
	}
	var bindings ext.ContainerJobList
	if err := c.List(ctx, &bindings); err != nil {
		return nil, err
	}
	count := 0
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if b.Spec.Job.Namespace != job.Namespace || b.Spec.Job.Name != job.Name {
			continue
		}
		count++
		if string(b.UID) != a.Allocation.Spec.JobBindingUID || b.Namespace != a.Task.Namespace || ownership.JobBindingMatches(b, &job, id.TenantID) != nil || tenant.IDOf(b) != id.TenantID || string(b.Spec.Runtime) != string(a.Task.Spec.Runtime.Kind) || b.Spec.Cluster != a.Allocation.Spec.Cluster {
			return nil, ErrIdentity
		}
	}
	if count != 1 {
		return nil, ErrIdentity
	}
	var clusters ext.ContainerClusterList
	if err := c.List(ctx, &clusters); err != nil {
		return nil, err
	}
	count = 0
	for _, cc := range clusters.Items {
		if cc.Namespace != id.Namespace && cc.Spec.Cluster != a.Allocation.Spec.Cluster {
			continue
		}
		count++
		if cc.UID == "" || cc.Namespace != id.Namespace || cc.Spec.Cluster != a.Allocation.Spec.Cluster || cc.Spec.AgentNamespace != id.Namespace || string(cc.Spec.Runtime) != string(a.Task.Spec.Runtime.Kind) || tenant.IDOf(&cc) != id.TenantID {
			return nil, ErrIdentity
		}
	}
	if count != 1 {
		return nil, ErrIdentity
	}
	var nativeClusters native.ClusterList
	if err := c.List(ctx, &nativeClusters); err != nil {
		return nil, err
	}
	for _, cc := range nativeClusters.Items {
		if cc.Spec.AgentNamespace == id.Namespace && objectRef(&cc) != a.Allocation.Spec.Cluster {
			return nil, ErrIdentity
		}
	}
	return a, nil
}

// Read waits at most twenty seconds and revalidates before releasing any bytes.
func (s LogService) Read(ctx context.Context, target LogTarget) (raw.LogSnapshot, error) {
	if s.Broker == nil {
		return raw.LogSnapshot{}, ErrUnavailable
	}
	id, a, err := s.resolve(ctx, target)
	if err == nil && a.Task.Spec.Runtime.Kind != "Raw" {
		err = ErrIdentity
	}
	if err != nil {
		return raw.LogSnapshot{}, err
	}
	w := LogWork{ID: uuid.NewString(), Target: target, Task: rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: a.Task.Namespace, Name: a.Task.Name}, ObjectUID: string(a.Task.UID), Generation: a.Allocation.Spec.Generation}}
	p, err := s.Broker.begin(id, w)
	if err != nil {
		return raw.LogSnapshot{}, err
	}
	defer s.Broker.forget(p)
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return raw.LogSnapshot{}, ctx.Err()
	case <-timer.C:
		return raw.LogSnapshot{}, ErrLogTimeout
	case result := <-p.result:
		if _, err := s.forIdentity(ctx, id, target); err != nil {
			return raw.LogSnapshot{}, err
		}
		if result.unavailable {
			return raw.LogSnapshot{}, ErrUnavailable
		}
		return result.snapshot, nil
	}
}

// LogRequest is the node-only poll/complete protocol; it carries no paths.
type LogRequest struct {
	Action      string           `json:"action"`
	Work        *LogWork         `json:"work,omitempty"`
	Snapshot    *raw.LogSnapshot `json:"snapshot,omitempty"`
	Unavailable bool             `json:"unavailable,omitempty"`
}

func (s LogService) Handle(ctx context.Context, id Identity, req LogRequest) (*LogWork, error) {
	if s.Broker == nil {
		return nil, ErrUnavailable
	}
	if _, err := s.Tasks.nodeForCleanup(ctx, id); err != nil {
		return nil, err
	}
	switch req.Action {
	case "poll":
		if req.Work != nil || req.Snapshot != nil || req.Unavailable {
			return nil, ErrCapability
		}
		w := s.Broker.poll(id)
		if w == nil {
			return nil, nil
		}
		if a, err := s.forIdentity(ctx, id, w.Target); err != nil {
			return nil, err
		} else if a.Task.Spec.Runtime.Kind != "Raw" {
			return nil, ErrIdentity
		}
		return w, nil
	case "complete":
		if req.Work == nil || req.Work.ID == "" || (req.Snapshot == nil) == !req.Unavailable || req.Snapshot != nil && len(req.Snapshot.Data) > raw.MaxLogBytes {
			return nil, ErrCapability
		}
		if a, err := s.forIdentity(ctx, id, req.Work.Target); err != nil {
			return nil, err
		} else if a.Task.Spec.Runtime.Kind != "Raw" {
			return nil, ErrIdentity
		}
		result := logResult{unavailable: req.Unavailable}
		if req.Snapshot != nil {
			result.snapshot = *req.Snapshot
		}
		return nil, s.Broker.complete(id, *req.Work, result)
	default:
		return nil, ErrCapability
	}
}
