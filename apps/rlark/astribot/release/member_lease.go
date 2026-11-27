package release

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	coordinationv1 "k8s.io/api/coordination/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// holdMembers serializes release writes against upgrade jobs on each member.
func (r *Run) holdMembers(ctx context.Context) (context.Context, func(), error) {
	work, stop := context.WithCancelCause(ctx)
	runID := uuid.NewString()
	members := maps.Clone(r.Desired.Clusters)
	if members == nil {
		members = map[string]ClusterState{}
	}
	// A release with a tree may also update shared member RBAC.
	if r.Tree != nil {
		for cluster := range r.Site.Members {
			members[cluster] = r.Desired.Clusters[cluster]
		}
	}
	if station := r.Site.Environment.Station.Cluster; station != "" &&
		(r.Desired.StationAPI != "" || r.Desired.StationController != "" || r.Desired.Installer != "") {
		members[station] = r.Desired.Clusters[station]
	}
	var guards []*clusterupgrade.Guard
	r.memberGuards = make(map[string]*clusterupgrade.Guard, len(members))
	release := func() {
		stop(context.Canceled)
		defer clear(r.memberGuards)
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
		if err := prepareMemberLease(work, member); err != nil {
			release()
			return ctx, nil, fmt.Errorf("cluster %s: prepare member release lease: %w", name, err)
		}
		leases := member.CoordinationV1().Leases(clusterupgrade.Namespace)
		guard, held, err := clusterupgrade.Acquire(work, leases, "rlarkctl-release/"+runID, runID, func(ctx context.Context, holder, executor string) (bool, error) {
			return clusterupgrade.PreviousUpgradeExecutorExited(ctx, member, holder, executor)
		})
		if err != nil {
			release()
			return ctx, nil, fmt.Errorf("cluster %s: %w", name, err)
		}
		guards = append(guards, guard)
		r.memberGuards[name] = guard
		context.AfterFunc(held, func() { stop(context.Cause(held)) })
	}
	return work, release, nil
}

// prepareMemberLease runs under the site lock, before any member workload
// writes. Old release identities can update this Role using their existing
// RBAC delegation. The member Job never bootstraps or grants itself access.
func prepareMemberLease(ctx context.Context, member kubernetes.Interface) error {
	namespace := clusterupgrade.Namespace
	leases := member.CoordinationV1().Leases(namespace)
	_, err := leases.Get(ctx, clusterupgrade.LeaseName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsForbidden(err) && !apierrors.IsNotFound(err) {
		return err
	}
	roles := member.RbacV1().Roles(namespace)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		role, err := roles.Get(ctx, "rlark-release", metav1.GetOptions{})
		if err != nil {
			return err
		}
		rules := []rbacv1.PolicyRule{
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{clusterupgrade.LeaseName}, Verbs: []string{"get", "update"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		}
		before := len(role.Rules)
		for _, rule := range rules {
			if !slices.ContainsFunc(role.Rules, func(current rbacv1.PolicyRule) bool {
				return slices.Equal(current.APIGroups, rule.APIGroups) && slices.Equal(current.Resources, rule.Resources) &&
					slices.Equal(current.ResourceNames, rule.ResourceNames) && slices.Equal(current.Verbs, rule.Verbs)
			}) {
				role.Rules = append(role.Rules, rule)
			}
		}
		if before == len(role.Rules) {
			return nil
		}
		_, err = roles.Update(ctx, role, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}
	return wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := leases.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: clusterupgrade.LeaseName, Namespace: namespace,
		}}, metav1.CreateOptions{})
		switch {
		case err == nil || apierrors.IsAlreadyExists(err):
			return true, nil
		case apierrors.IsForbidden(err) && ctx.Err() == nil:
			return false, nil
		default:
			return false, err
		}
	})
}

func (r *Run) checkMemberLease(ctx context.Context, name string) error {
	guard := r.memberGuards[name]
	if guard == nil {
		return fmt.Errorf("missing member release lease for %s", name)
	}
	if err := guard.Check(ctx); err != nil {
		return fmt.Errorf("cluster %s: %w", name, err)
	}
	return nil
}
