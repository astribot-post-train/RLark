package raw

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

type admissionFunc func(context.Context, rt.TaskIdentity) error

func (f admissionFunc) Authorize(ctx context.Context, t rt.TaskIdentity) error { return f(ctx, t) }

type memorySupervisor struct {
	record        *Record
	starts, stops int
	uncertain     bool
	wrongStop     bool
}

func (s *memorySupervisor) Observe(ctx context.Context, _ rt.TaskIdentity) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if s.record == nil {
		return Record{}, fault.New(fault.NotFound, "absent")
	}
	return *s.record, nil
}
func (s *memorySupervisor) Ensure(ctx context.Context, t rt.TaskIdentity, spec ProcessSpec) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if s.record == nil {
		s.starts++
		s.record = &Record{Task: t, Spec: spec, Phase: "Running"}
	}
	if !reflect.DeepEqual(s.record.Spec, spec) {
		return Record{}, fault.New(fault.Conflict, "changed spec")
	}
	return *s.record, nil
}
func (s *memorySupervisor) Stop(ctx context.Context, t rt.TaskIdentity) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	s.stops++
	r := *s.record
	if s.wrongStop {
		r.Task.ObjectUID = "other"
		r.Cleaned = true
		r.Deleted = true
		return r, nil
	}
	if !s.uncertain {
		r.Cleaned = true
		r.Deleted = true
		s.record = &r
	}
	return r, nil
}
func request(t *testing.T) (*Provider, *memorySupervisor, rt.ApplyRequest) {
	t.Helper()
	s := &memorySupervisor{}
	p := &Provider{ID: "raw-node", Supervisor: s, Admission: admissionFunc(func(context.Context, rt.TaskIdentity) error { return nil })}
	task := rt.TaskIdentity{Logical: rt.LogicalTaskID{Namespace: "agents", Name: "worker"}, ObjectUID: "task-uid"}
	data, err := json.Marshal(manifestFor(task, ProcessSpec{Argv: []string{"/usr/bin/python3", "train.py"}, Directory: "/work", Env: []string{"CUDA_VISIBLE_DEVICES=GPU-1"}}))
	if err != nil {
		t.Fatal(err)
	}
	return p, s, rt.ApplyRequest{Task: task, Desired: rt.DesiredObject{LogicalName: "process", Kind: ProcessKind, Manifest: rt.TypedManifest{Type: ProcessKind, Object: data}, Ownership: rt.ResourceOwnership{Scope: rt.IdentityTaskIncarnation, OwnerKey: task.ObjectUID}, Lifecycle: rt.LifecycleDeleteOnTaskCleanup, UpdateMode: rt.UpdateUnsupported}}
}
func TestDuplicateApplyAndProviderRestart(t *testing.T) {
	p, s, req := request(t)
	ctx := context.Background()
	first, err := p.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Provider{ID: p.ID, Supervisor: s, Admission: p.Admission}
	again, err := restarted.Apply(ctx, req)
	if err != nil || first.Ref != again.Ref || s.starts != 1 {
		t.Fatalf("duplicate effect: %v starts=%d", err, s.starts)
	}
	inventory, err := restarted.ListManaged(ctx, req.Task)
	if err != nil || len(inventory.Objects) != 1 {
		t.Fatalf("recovery: %v", err)
	}
	spec := s.record.Spec
	spec.Argv = []string{"/bin/false"}
	req.Desired.Manifest.Object, _ = json.Marshal(manifestFor(req.Task, spec))
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatalf("immutable spec: %v", err)
	}
}
func TestAdmissionDenialHasNoEffect(t *testing.T) {
	p, s, req := request(t)
	denied := fault.New(fault.PermissionDenied, "not reserved")
	p.Admission = admissionFunc(func(context.Context, rt.TaskIdentity) error { return denied })
	if _, err := p.Apply(context.Background(), req); !errors.Is(err, denied) || s.starts != 0 {
		t.Fatalf("admission: %v", err)
	}
}
func TestOwnershipConflictsNeverAuthorizeCleanup(t *testing.T) {
	p, s, req := request(t)
	ctx := context.Background()
	result, err := p.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	forged := result.Ref
	forged.UID = "another"
	if err := p.Delete(ctx, rt.DeleteRequest{Task: req.Task, Ref: forged}); !fault.Is(err, fault.PermissionDenied) || s.stops != 0 {
		t.Fatalf("forged reference: %v", err)
	}
	s.record.Task.ObjectUID = "another"
	if _, err := p.ListManaged(ctx, req.Task); !fault.Is(err, fault.Conflict) {
		t.Fatalf("inventory ownership: %v", err)
	}
	if err := p.Delete(ctx, rt.DeleteRequest{Task: req.Task, Ref: result.Ref}); !fault.Is(err, fault.Conflict) || s.stops != 0 {
		t.Fatalf("foreign record: %v", err)
	}
}
func TestUncertainCleanupAndWrongResultBlockDeletion(t *testing.T) {
	p, s, req := request(t)
	ctx := context.Background()
	result, err := p.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	del := rt.DeleteRequest{Task: req.Task, Ref: result.Ref}
	s.uncertain = true
	if err := p.Delete(ctx, del); !fault.Is(err, fault.Unavailable) {
		t.Fatalf("uncertain cleanup: %v", err)
	}
	snapshot, err := p.ListManaged(ctx, req.Task)
	if err != nil || len(snapshot.Objects) != 1 {
		t.Fatal("uncertain process disappeared")
	}
	s.wrongStop = true
	if err := p.Delete(ctx, del); !fault.Is(err, fault.Conflict) {
		t.Fatalf("wrong stop result: %v", err)
	}
	s.wrongStop = false
	s.uncertain = false
	if err := p.Delete(ctx, del); err != nil {
		t.Fatal(err)
	}
	before := s.stops
	if err := p.Delete(ctx, del); err != nil || s.stops != before {
		t.Fatalf("duplicate cleanup: %v", err)
	}
	snapshot, err = p.ListManaged(ctx, req.Task)
	if err != nil || len(snapshot.Objects) != 0 {
		t.Fatal("cleaned process still present")
	}
	s.record.Cleaned = false
	if _, err := p.ListManaged(ctx, req.Task); !fault.Is(err, fault.Conflict) {
		t.Fatalf("unproven tombstone: %v", err)
	}
}
func TestInvalidManifestsDoNotStart(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `{"argv":["python3"],"directory":"/work"}`, `{"argv":["/bin/true"],"directory":"relative"}`, `{"argv":["/bin/true"],"directory":"/work","extra":true}`, `{"argv":["/bin/true"],"directory":"/work"} {}`, `{"argv":["/bin/true"],"directory":"/work","env":["A=1","A=2"]}`} {
		t.Run(data, func(t *testing.T) {
			p, s, req := request(t)
			req.Desired.Manifest.Object = json.RawMessage(data)
			if _, err := p.Apply(context.Background(), req); !fault.Is(err, fault.InvalidArgument) || s.starts != 0 {
				t.Fatalf("invalid manifest accepted: %v", err)
			}
		})
	}
}
func TestCancellationDoesNotStart(t *testing.T) {
	p, s, req := request(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Apply(ctx, req); !errors.Is(err, context.Canceled) || s.starts != 0 {
		t.Fatalf("cancellation: %v", err)
	}
}
