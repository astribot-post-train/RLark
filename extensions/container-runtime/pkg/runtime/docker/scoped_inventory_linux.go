//go:build linux

package docker

import (
	"context"
	"errors"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

// verifyRecorded prevents Coordinator inventory from adopting an object through
// its labels alone, bypassing Apply's local creation-record ownership check.
func (p *ScopedProvider) verifyRecorded(task rt.TaskIdentity, objects []rt.Object) error {
	record, err := p.Journal.read(task)
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		return nil
	}
	if record == nil || !record.Creating {
		return fault.New(fault.Conflict, "Docker inventory has no local creation record")
	}
	for _, object := range objects {
		if record.ContainerID != "" && record.ContainerID != object.Ref.ID {
			return fault.New(fault.Conflict, "Docker inventory container identity changed")
		}
	}
	return nil
}

func (p *ScopedProvider) ListManaged(ctx context.Context, task rt.TaskIdentity) (rt.ObjectSnapshot, error) {
	unlock, err := p.Journal.acquire(ctx)
	if err != nil {
		return rt.ObjectSnapshot{}, err
	}
	defer unlock()
	if err := p.checkEngine(ctx); err != nil {
		return rt.ObjectSnapshot{}, err
	}
	objects, err := p.Provider.ListManaged(ctx, task)
	if err != nil {
		return objects, err
	}
	return objects, p.verifyRecorded(task, objects.Objects)
}

func (p *ScopedProvider) ListCandidates(ctx context.Context, q rt.ManagedObjectQuery) (rt.ObjectSnapshot, error) {
	unlock, err := p.Journal.acquire(ctx)
	if err != nil {
		return rt.ObjectSnapshot{}, err
	}
	defer unlock()
	if err := p.checkEngine(ctx); err != nil {
		return rt.ObjectSnapshot{}, err
	}
	objects, err := p.Provider.ListCandidates(ctx, q)
	if err != nil {
		return objects, err
	}
	return objects, p.verifyRecorded(q.Task, objects.Objects)
}

func (p *ScopedProvider) Get(ctx context.Context, ref rt.ObjectRef) (rt.Object, error) {
	unlock, err := p.Journal.acquire(ctx)
	if err != nil {
		return rt.Object{}, err
	}
	defer unlock()
	if err := p.checkEngine(ctx); err != nil {
		return rt.Object{}, err
	}
	object, err := p.Provider.Get(ctx, ref)
	if err != nil {
		return object, err
	}
	manifest, err := decodeManifest(object.Manifest.Object)
	if err != nil {
		return object, err
	}
	task := rt.TaskIdentity{Logical: ref.LogicalTask, ObjectUID: manifest.Metadata.UID}
	return object, p.verifyRecorded(task, []rt.Object{object})
}

func (p *ScopedProvider) ListInstances(ctx context.Context, q rt.InstanceQuery) (rt.InstanceSnapshot, error) {
	unlock, err := p.Journal.acquire(ctx)
	if err != nil {
		return rt.InstanceSnapshot{}, err
	}
	defer unlock()
	if err := p.checkEngine(ctx); err != nil {
		return rt.InstanceSnapshot{}, err
	}
	if len(q.Workloads) == 0 {
		return rt.InstanceSnapshot{}, nil
	}
	if len(q.Workloads) != 1 {
		return rt.InstanceSnapshot{}, errors.New("one Docker workload required")
	}
	instances, err := p.Provider.ListInstances(ctx, q)
	if err != nil {
		return instances, err
	}
	if len(instances.Instances) == 0 {
		return instances, nil
	}
	objects := make([]rt.Object, 0, len(instances.Instances))
	for _, instance := range instances.Instances {
		objects = append(objects, rt.Object{Ref: rt.ObjectRef{RuntimeID: instance.Ref.RuntimeID, ID: instance.Ref.ID, UID: instance.Ref.UID, Namespace: q.Task.Logical.Namespace, Name: q.Task.Logical.Name, LogicalTask: q.Task.Logical}})
	}
	return instances, p.verifyRecorded(q.Task, objects)
}
