// Package scheduler contains the first durable Task-to-node scheduling seam.
// It deliberately schedules one Task at a time; group barriers and Ray
// membership are layered on top of this single-target primitive.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrNoCandidate means that no currently eligible node can accept the Task.
var ErrNoCandidate = errors.New("no eligible container node")

// Request identifies the already-created typed Job binding and native Task.
// The scheduler never accepts a user-selected node or GPU subset.
type Request struct {
	Binding client.ObjectKey
	Task    client.ObjectKey
}

// Planner persists a single target before delegating exclusive reservation to
// the companion allocation Manager. Allocator.Client must be uncached.
type Planner struct {
	Allocator companion.Manager
}

func (p Planner) targetNode(ctx context.Context, binding *ext.ContainerJob, target *ext.ContainerAllocationSpec) (client.ObjectKey, error) {
	var nodes ext.ContainerNodeList
	if err := p.Allocator.Client.List(ctx, &nodes, client.InNamespace(binding.Namespace)); err != nil {
		return client.ObjectKey{}, err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if string(node.UID) == target.NodeRecordUID && node.Spec.Node.UID == target.Node.UID && node.Spec.Cluster == target.Cluster {
			return client.ObjectKeyFromObject(node), nil
		}
	}
	return client.ObjectKey{}, fmt.Errorf("%w: persisted target node is absent", companion.ErrIdentity)
}

func (p Planner) candidates(ctx context.Context, binding *ext.ContainerJob, task *native.Task) ([]ext.ContainerNode, error) {
	var nodes ext.ContainerNodeList
	if err := p.Allocator.Client.List(ctx, &nodes, client.InNamespace(binding.Namespace)); err != nil {
		return nil, err
	}
	result := make([]ext.ContainerNode, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if node.Spec.Cluster != binding.Spec.Cluster || node.Spec.Runtime != binding.Spec.Runtime || task.Spec.Runtime.Kind != native.RuntimeKind(binding.Spec.Runtime) || task.Spec.Placement.Environment != node.Spec.Environment {
			continue
		}
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return string(result[i].UID) < string(result[j].UID)
	})
	return result, nil
}

// Acquire chooses a deterministic eligible node, pins the Task target through
// resourceVersion CAS, and then acquires the durable node lock. Once a target
// exists, every retry uses only that node; even a lost Create response cannot
// cause a second Task or a different node to be selected.
func (p Planner) Acquire(ctx context.Context, req Request) (*ext.ContainerAllocation, error) {
	if p.Allocator.Client == nil {
		return nil, errors.New("uncached allocator client is required")
	}
	var binding ext.ContainerJob
	if err := p.Allocator.Client.Get(ctx, req.Binding, &binding); err != nil {
		return nil, err
	}
	var task native.Task
	if err := p.Allocator.Client.Get(ctx, req.Task, &task); err != nil {
		return nil, err
	}
	target, err := companion.Target(&task)
	if err != nil {
		return nil, err
	}
	if target != nil {
		nodeKey, err := p.targetNode(ctx, &binding, target)
		if err != nil {
			return nil, err
		}
		return p.Allocator.Acquire(ctx, companion.Request{Binding: req.Binding, Node: nodeKey, Task: req.Task})
	}
	candidates, err := p.candidates(ctx, &binding, &task)
	if err != nil {
		return nil, err
	}
	for i := range candidates {
		nodeKey := client.ObjectKeyFromObject(&candidates[i])
		allocation, acquireErr := p.Allocator.Acquire(ctx, companion.Request{Binding: req.Binding, Node: nodeKey, Task: req.Task})
		if acquireErr == nil {
			return allocation, nil
		}
		var refreshed native.Task
		if err := p.Allocator.Client.Get(ctx, req.Task, &refreshed); err != nil {
			return nil, err
		}
		if pinned, err := companion.Target(&refreshed); err != nil {
			return nil, err
		} else if pinned != nil {
			return nil, acquireErr
		}
		if !errors.Is(acquireErr, companion.ErrUnavailable) {
			return nil, acquireErr
		}
	}
	return nil, ErrNoCandidate
}
