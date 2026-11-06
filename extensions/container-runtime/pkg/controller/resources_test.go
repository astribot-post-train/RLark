package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/allocation"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/noderesources"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func resourceFixture(t *testing.T) (*ResourcesReconciler, client.Client, *native.Node, *ext.ContainerNode) {
	now := time.Now().UTC().Truncate(time.Second)
	_, c, nn, cn, _ := connectivityFixture(t, now)
	cn.Status.ResourceObservedAt = &metav1.Time{Time: now}
	cn.Status.Capacity = map[string]string{"cpu": "2500m", "memory": "8Gi", "nvidia.com/gpu": "1"}
	cn.Status.AuthorizedGPUUUIDs = append([]string{}, cn.Spec.AuthorizedGPUUUIDs...)
	cn.Status.Schedulable = true
	cn.Status.SystemInfo = &ext.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64", KernelVersion: "test-kernel"}
	require.NoError(t, c.Status().Update(context.Background(), cn))
	return &ResourcesReconciler{Client: c, Now: func() time.Time { return now }}, c, nn, cn
}
func reconcileResources(t *testing.T, r *ResourcesReconciler, c client.Client, nn *native.Node, cn *ext.ContainerNode) *native.Node {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cn)})
	require.NoError(t, err)
	var result native.Node
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(nn), &result))
	return &result
}
func resourceLock(nn *native.Node, cn *ext.ContainerNode) *ext.ContainerAllocation {
	return &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: allocation.Name(string(nn.UID)), Namespace: cn.Namespace, UID: "allocation-uid", Labels: cn.Labels}, Spec: ext.ContainerAllocationSpec{Cluster: cn.Spec.Cluster, Node: cn.Spec.Node, TenantID: "team-a", NodeRecordUID: string(cn.UID), NodeInstanceID: cn.Spec.InstanceID, PrivateAddress: cn.Spec.PrivateAddress, NetworkScope: cn.Spec.NetworkScope, GPUUUIDs: cn.Spec.AuthorizedGPUUUIDs}}
}
func TestResourceProjectionPreservesNativeOwnershipAndCordons(t *testing.T) {
	r, c, nn, cn := resourceFixture(t)
	nn.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "CoreConnected", LastTransitionTime: metav1.Now()}}
	nn.Status.Phase = native.NodePhase("Online")
	require.NoError(t, c.Status().Update(context.Background(), nn))
	got := reconcileResources(t, r, c, nn, cn)
	require.True(t, got.Spec.Unschedulable)
	require.Equal(t, "Online", string(got.Status.Phase))
	require.Equal(t, "CoreConnected", meta.FindStatusCondition(got.Status.Conditions, "Ready").Reason)
	require.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, noderesources.ResourcesReady))
	require.Equal(t, "NodeCordoned", meta.FindStatusCondition(got.Status.Conditions, noderesources.SchedulingReady).Reason)
	require.Equal(t, resource.MustParse("2500m"), got.Status.Capacity[corev1.ResourceCPU])
	require.Equal(t, got.Status.Capacity, got.Status.Allocatable)
	require.True(t, got.Status.Used.Cpu().IsZero())
	require.Equal(t, "test-kernel", got.Status.NodeInfo.KernelVersion)
	require.Equal(t, cn.Spec.PrivateAddress, got.Status.Addresses[0].Address)
	// A fresh controller reconstructs the same read-only view.
	r = &ResourcesReconciler{Client: c, Now: r.Now}
	again := reconcileResources(t, r, c, nn, cn)
	require.Equal(t, got.Status, again.Status)
}
func TestResourceReservationsPersistUntilAllocationAbsent(t *testing.T) {
	for _, kind := range []string{"workload", "terminal", "cleanup-confirmed", "deleting", "stale", "maintenance", "external-busy"} {
		t.Run(kind, func(t *testing.T) {
			r, c, nn, cn := resourceFixture(t)
			ctx := context.Background()
			lock := resourceLock(nn, cn)
			switch kind {
			case "terminal":
				lock.Status.Phase = "Succeeded"
			case "cleanup-confirmed":
				lock.Status.CleanupConfirmed = true
			case "deleting":
				lock.Finalizers = []string{"hold"}
				lock.DeletionTimestamp = &metav1.Time{Time: r.Now()}
			case "stale":
				now := r.Now()
				r.Now = func() time.Time { return now.Add(time.Minute) }
			case "maintenance":
				lock.Spec.Upgrade = &ext.UpgradeAllocation{}
			case "external-busy":
				cn.Status.Busy = true
				require.NoError(t, c.Status().Update(ctx, cn))
			}
			if kind != "external-busy" {
				require.NoError(t, c.Create(ctx, lock))
			}
			got := reconcileResources(t, r, c, nn, cn)
			if kind == "maintenance" || kind == "external-busy" {
				require.True(t, got.Status.Used.Cpu().IsZero())
			} else {
				require.Equal(t, got.Status.Capacity, got.Status.Used)
			}
			require.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, noderesources.SchedulingReady))
			if kind == "stale" {
				require.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, noderesources.ResourcesReady))
			}
			if kind == "workload" {
				require.NoError(t, c.Delete(ctx, lock))
				got = reconcileResources(t, r, c, nn, cn)
				require.True(t, got.Status.Used.Cpu().IsZero())
			}
		})
	}
}
func TestResourceUnknownInventoryAndRecovery(t *testing.T) {
	r, c, nn, cn := resourceFixture(t)
	ctx := context.Background()
	lock := resourceLock(nn, cn)
	require.NoError(t, c.Create(ctx, lock))
	got := reconcileResources(t, r, c, nn, cn)
	require.Equal(t, got.Status.Capacity, got.Status.Used)
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		if _, ok := l.(*ext.ContainerAllocationList); ok {
			return errors.New("inventory unavailable")
		}
		return cl.List(ctx, l, opts...)
	}})
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cn)})
	require.Error(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(nn), got))
	require.Equal(t, got.Status.Capacity, got.Status.Used)
	require.Equal(t, "AllocationInventoryUnavailable", meta.FindStatusCondition(got.Status.Conditions, noderesources.ResourcesReady).Reason)
	r.Client = c
	require.NoError(t, c.Delete(ctx, lock))
	got = reconcileResources(t, r, c, nn, cn)
	require.True(t, got.Status.Used.Cpu().IsZero())
}
func TestResourceProjectionRefusesForeignOwnerAndOldAgent(t *testing.T) {
	for _, kind := range []string{"foreign-owner", "unowned-capacity", "old-agent", "credential", "canonical-conflict", "spec-generation"} {
		t.Run(kind, func(t *testing.T) {
			r, c, nn, cn := resourceFixture(t)
			ctx := context.Background()
			switch kind {
			case "foreign-owner":
				nn.Annotations = map[string]string{noderesources.ProjectionAnnotation: "someone-else"}
				require.NoError(t, c.Update(ctx, nn))
			case "unowned-capacity":
				nn.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("32")}
				require.NoError(t, c.Status().Update(ctx, nn))
			case "old-agent":
				cn.Status.ResourceObservedAt = nil
				require.NoError(t, c.Status().Update(ctx, cn))
			case "credential":
				cn.Status.AgentCredentialID = "revoked"
				require.NoError(t, c.Status().Update(ctx, cn))
			case "canonical-conflict":
				lock := resourceLock(nn, cn)
				lock.Spec.Node.UID = "other"
				require.NoError(t, c.Create(ctx, lock))
			case "spec-generation":
				cn.Generation++
				require.NoError(t, c.Update(ctx, cn))
			}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cn)})
			if kind == "foreign-owner" || kind == "unowned-capacity" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			got := &native.Node{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(nn), got))
			require.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, noderesources.ResourcesReady))
			require.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, noderesources.SchedulingReady))
		})
	}
}

func TestResourceStatusConflictRepairsAfterPartialProjection(t *testing.T) {
	r, c, nn, cn := resourceFixture(t)
	conflict := true
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if _, ok := obj.(*native.Node); ok && conflict {
			conflict = false
			return errors.New("simulated status conflict")
		}
		return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
	}})
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cn)})
	require.Error(t, err)
	var got native.Node
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(nn), &got))
	require.Equal(t, string(cn.UID), got.Annotations[noderesources.ProjectionAnnotation])
	require.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, noderesources.ResourcesReady))
	gotp := reconcileResources(t, r, c, nn, cn)
	require.True(t, meta.IsStatusConditionTrue(gotp.Status.Conditions, noderesources.ResourcesReady))
	require.Equal(t, resource.MustParse("8Gi"), gotp.Status.Capacity[corev1.ResourceMemory])
}
