// Package raw implements the preview process Runtime protocol inside an existing
// container. The Supervisor boundary owns OS process effects and durable recovery.
package raw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

// ProcessKind is the versioned Raw process manifest schema.
var ProcessKind = rt.ObjectKind{Group: "raw.rlark.io", Version: "v1alpha1", Kind: "Process"}

// ProcessSpec is immutable once an execution has been accepted. Env is an
// explicit allowlist: credentials and the Agent environment must not be inherited.
type ProcessSpec struct {
	Argv      []string `json:"argv"`
	Env       []string `json:"env"`
	Directory string   `json:"directory"`
}

// Record is a durable observed execution, scoped to one management Task UID.
// Cleaned proves that supervision confirmed termination of all owned processes.
type Record struct {
	Task    rt.TaskIdentity  `json:"task"`
	Spec    ProcessSpec      `json:"spec"`
	Phase   rt.InstancePhase `json:"phase"`
	Message string           `json:"message,omitempty"`
	Cleaned bool             `json:"cleaned"`
	Deleted bool             `json:"deleted"`
}

// Supervisor persists intent before starting a process, recovers duplicate
// starts, and never treats an unavailable supervisor as completed cleanup.
// Methods must be safe across independent Provider instances and Agent restarts.
type Supervisor interface {
	Observe(context.Context, rt.TaskIdentity) (Record, error)
	Ensure(context.Context, rt.TaskIdentity, ProcessSpec) (Record, error)
	Stop(context.Context, rt.TaskIdentity) (Record, error)
}

// Admission revalidates node/execution/allocation and complete-group barriers
// immediately before a start. It must fail closed for directly created Tasks.
type Admission interface {
	Authorize(context.Context, rt.TaskIdentity) error
}

// Provider implements the existing Coordinator Runtime boundary.
type Provider struct {
	ID         rt.ID
	Supervisor Supervisor
	Admission  Admission
}

var _ rt.Provider = (*Provider)(nil)
var _ rt.InventoryProvider = (*Provider)(nil)

// Info reports the configured Raw runtime instance.
func (p *Provider) Info(context.Context) (rt.Info, error) {
	if p.ID == "" || p.Supervisor == nil || p.Admission == nil {
		return rt.Info{}, errors.New("raw runtime ID, supervisor and admission are required")
	}
	return rt.Info{ID: p.ID, Kind: "Raw", Capabilities: []rt.Capability{rt.CapabilityWorkloadInventory, rt.CapabilityCandidateInventory}}, nil
}
func validIdentity(t rt.TaskIdentity) error {
	if err := t.Logical.Validate(); err != nil {
		return err
	}
	if t.ObjectUID == "" {
		return errors.New("task UID is required")
	}
	return nil
}
func (p *Provider) ref(t rt.TaskIdentity) rt.ObjectRef {
	return rt.ObjectRef{RuntimeID: p.ID, ID: t.ObjectUID, UID: t.ObjectUID, Namespace: t.Logical.Namespace, Name: t.Logical.Name, LogicalTask: t.Logical}
}
func (p *Provider) object(r Record) (rt.Object, error) {
	data, err := json.Marshal(manifestFor(r.Task, r.Spec))
	if err != nil {
		return rt.Object{}, err
	}
	return rt.Object{Ref: p.ref(r.Task), Manifest: rt.TypedManifest{Type: ProcessKind, Object: data}}, nil
}
func (p *Provider) observed(ctx context.Context, t rt.TaskIdentity) (Record, error) {
	if _, err := p.Info(ctx); err != nil {
		return Record{}, err
	}
	if err := validIdentity(t); err != nil {
		return Record{}, err
	}
	r, err := p.Supervisor.Observe(ctx, t)
	if err != nil {
		return Record{}, err
	}
	if r.Task.ObjectUID != t.ObjectUID || r.Task.Logical != t.Logical {
		return Record{}, fault.New(fault.Conflict, "Raw supervision ownership conflict")
	}
	if r.Deleted && !r.Cleaned {
		return Record{}, fault.New(fault.Conflict, "Raw deletion lacks cleanup proof")
	}
	return r, nil
}

// Get returns only a proven-owned process object.
func (p *Provider) Get(ctx context.Context, ref rt.ObjectRef) (rt.Object, error) {
	if ref.RuntimeID != p.ID || ref.UID == "" || ref.ID != ref.UID || ref.Name != ref.LogicalTask.Name || ref.Namespace != ref.LogicalTask.Namespace {
		return rt.Object{}, fault.New(fault.InvalidArgument, "invalid Raw object reference")
	}
	r, err := p.observed(ctx, rt.TaskIdentity{Logical: ref.LogicalTask, ObjectUID: ref.UID})
	if err != nil {
		return rt.Object{}, err
	}
	if r.Deleted {
		return rt.Object{}, fault.New(fault.NotFound, "Raw process removed")
	}
	return p.object(r)
}

// ListManaged reconstructs process ownership without compiling current Task spec.
func (p *Provider) ListManaged(ctx context.Context, t rt.TaskIdentity) (rt.ObjectSnapshot, error) {
	r, err := p.observed(ctx, t)
	if fault.Is(err, fault.NotFound) {
		return rt.ObjectSnapshot{}, nil
	}
	if err != nil {
		return rt.ObjectSnapshot{}, err
	}
	if r.Deleted {
		return rt.ObjectSnapshot{}, nil
	}
	o, err := p.object(r)
	if err != nil {
		return rt.ObjectSnapshot{}, err
	}
	return rt.ObjectSnapshot{Revision: string(r.Phase), Objects: []rt.Object{o}}, nil
}

// ListCandidates accepts only the exact identity-derived process location.
func (p *Provider) ListCandidates(ctx context.Context, q rt.ManagedObjectQuery) (rt.ObjectSnapshot, error) {
	for _, c := range q.Candidates {
		if c.Kind != ProcessKind || c.Name != q.Task.Logical.Name || c.Namespace != q.Task.Logical.Namespace || len(c.Labels) != 0 || c.Ownership.Scope != rt.IdentityTaskIncarnation || c.Ownership.OwnerKey != q.Task.ObjectUID || c.Ownership.IncarnationPolicy != "" || c.Lifecycle != rt.LifecycleDeleteOnTaskCleanup {
			return rt.ObjectSnapshot{}, fault.New(fault.PermissionDenied, "Raw candidate outside Task ownership")
		}
	}
	if len(q.Candidates) == 0 {
		return rt.ObjectSnapshot{}, nil
	}
	return p.ListManaged(ctx, q.Task)
}

// Apply starts an admitted immutable execution or recovers its existing result.
func (p *Provider) Apply(ctx context.Context, req rt.ApplyRequest) (rt.ApplyResult, error) {
	if _, err := p.Info(ctx); err != nil {
		return rt.ApplyResult{}, err
	}
	if err := validIdentity(req.Task); err != nil {
		return rt.ApplyResult{}, err
	}
	if req.Current != nil && *req.Current != p.ref(req.Task) {
		return rt.ApplyResult{}, fault.New(fault.PermissionDenied, "Raw current reference outside Task ownership")
	}
	d := req.Desired
	if err := p.ValidateDesired(d); err != nil {
		return rt.ApplyResult{}, err
	}
	manifest, err := decodeManifest(d.Manifest.Object)
	if err != nil {
		return rt.ApplyResult{}, err
	}
	if manifest.Metadata.Name != req.Task.Logical.Name || manifest.Metadata.Namespace != req.Task.Logical.Namespace || manifest.Metadata.UID != req.Task.ObjectUID || d.Ownership.OwnerKey != req.Task.ObjectUID {
		return rt.ApplyResult{}, fault.New(fault.PermissionDenied, "Raw manifest outside Task ownership")
	}
	spec := manifest.Spec
	if err := p.Admission.Authorize(ctx, req.Task); err != nil {
		return rt.ApplyResult{}, fmt.Errorf("admit Raw process: %w", err)
	}
	r, err := p.Supervisor.Ensure(ctx, req.Task, spec)
	if err != nil {
		return rt.ApplyResult{}, err
	}
	if r.Task.ObjectUID != req.Task.ObjectUID || r.Task.Logical != req.Task.Logical || !reflect.DeepEqual(r.Spec, spec) || r.Deleted {
		return rt.ApplyResult{}, fault.New(fault.Conflict, "Raw immutable execution conflict")
	}
	o, err := p.object(r)
	return rt.ApplyResult{Ref: o.Ref, Object: o}, err
}

// Delete requests scoped cleanup. Repeated calls remain in progress until the
// independent supervisor confirms cleanup; no PID supplied by a caller is used.
func (p *Provider) Delete(ctx context.Context, req rt.DeleteRequest) error {
	if req.Ref != p.ref(req.Task) {
		return fault.New(fault.PermissionDenied, "Raw cleanup reference outside Task ownership")
	}
	r, err := p.observed(ctx, req.Task)
	if fault.Is(err, fault.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Deleted {
		if !r.Cleaned {
			return fault.New(fault.Conflict, "Raw deletion lacks cleanup proof")
		}
		return nil
	}
	r, err = p.Supervisor.Stop(ctx, req.Task)
	if err != nil {
		return err
	}
	if r.Task.ObjectUID != req.Task.ObjectUID || r.Task.Logical != req.Task.Logical {
		return fault.New(fault.Conflict, "Raw cleanup result ownership conflict")
	}
	if !r.Cleaned || !r.Deleted {
		return fault.New(fault.Unavailable, "Raw cleanup still in progress")
	}
	return nil
}

// ListInstances reports only instances rooted in this Task's process object.
func (p *Provider) ListInstances(ctx context.Context, q rt.InstanceQuery) (rt.InstanceSnapshot, error) {
	if q.Options != nil {
		return rt.InstanceSnapshot{}, fault.New(fault.InvalidArgument, "Raw instance options unsupported")
	}
	for _, ref := range q.Workloads {
		if ref != p.ref(q.Task) {
			return rt.InstanceSnapshot{}, fault.New(fault.PermissionDenied, "Raw instance query outside Task ownership")
		}
	}
	if len(q.Workloads) == 0 {
		return rt.InstanceSnapshot{}, nil
	}
	r, err := p.observed(ctx, q.Task)
	if fault.Is(err, fault.NotFound) {
		return rt.InstanceSnapshot{}, nil
	}
	if err != nil {
		return rt.InstanceSnapshot{}, err
	}
	if r.Deleted {
		return rt.InstanceSnapshot{}, nil
	}
	return rt.InstanceSnapshot{Revision: string(r.Phase), Instances: []rt.Instance{{Ref: rt.InstanceRef{RuntimeID: p.ID, ID: r.Task.ObjectUID, UID: r.Task.ObjectUID, Parent: p.ref(r.Task)}, Task: r.Task, Name: r.Task.Logical.Name, Phase: r.Phase, Message: r.Message}}}, nil
}
