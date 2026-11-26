package release

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
)

// holdMembers serializes release writes against upgrade jobs on each member.
func (r *Run) holdMembers(ctx context.Context) (context.Context, func(), error) {
	work, stop := context.WithCancelCause(ctx)
	runID := uuid.NewString()
	members := maps.Clone(r.Desired.Clusters)
	// A release with a tree may also update shared member RBAC.
	if r.Tree != nil {
		if members == nil {
			members = map[string]ClusterState{}
		}
		for cluster := range r.Site.Members {
			members[cluster] = r.Desired.Clusters[cluster]
		}
	}
	var guards []*clusterupgrade.Guard
	release := func() {
		stop(context.Canceled)
		for _, guard := range slices.Backward(guards) {
			call, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if err := guard.Release(call); err != nil {
				r.log.printf("could not release member upgrade lease: %v", err)
			}
			cancel()
		}
	}
	for _, name := range slices.Sorted(maps.Keys(members)) {
		member, ok := r.Site.Members[name]
		if !ok {
			release()
			return ctx, nil, fmt.Errorf("member credentials unavailable for %s", name)
		}
		leases := member.CoordinationV1().Leases(clusterupgrade.Namespace)
		guard, held, err := clusterupgrade.Acquire(work, leases, "rlarkctl-release/"+runID, runID, nil)
		if err != nil {
			release()
			return ctx, nil, fmt.Errorf("cluster %s: %w", name, err)
		}
		guards = append(guards, guard)
		context.AfterFunc(held, func() { stop(context.Cause(held)) })
	}
	return work, release, nil
}
