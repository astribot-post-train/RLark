package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func rolloutFixture(t *testing.T) (AgentRolloutReconciler, ctrl.Request, AgentUpgradeReconciler, ctrl.Request) {
	t.Helper()
	upgrades, opReq := upgradeControllerFixture(t)
	ctx := context.Background()
	checkUpgrade(t, upgrades.Client.Delete(ctx, readUpgrade(t, upgrades, opReq)))
	base := upgrades.Client
	upgrades.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if _, ok := o.(*ext.AgentUpgrade); ok {
			o.SetUID("rollout-operation")
			o.SetGeneration(1)
		}
		return c.Create(ctx, o, opts...)
	}})
	var n ext.ContainerNode
	checkUpgrade(t, upgrades.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &n))
	// A target version deliberately makes the source version unschedulable.
	n.Status.Schedulable = false
	n.Status.Capabilities = []string{ext.AgentUpgradeCapability, "agent-platform/linux/amd64"}
	checkUpgrade(t, upgrades.Client.Status().Update(ctx, &n))
	return AgentRolloutReconciler(upgrades), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "agents", Name: "cloud"}}, upgrades, opReq
}

func reconcileRollout(t *testing.T, r AgentRolloutReconciler, req ctrl.Request, count int) {
	t.Helper()
	for range count {
		_, err := r.Reconcile(context.Background(), req)
		checkUpgrade(t, err)
	}
}
func rolloutCluster(t *testing.T, r AgentRolloutReconciler, req ctrl.Request) *ext.ContainerCluster {
	t.Helper()
	var cc ext.ContainerCluster
	checkUpgrade(t, r.Client.Get(context.Background(), req.NamespacedName, &cc))
	return &cc
}
func rolloutReason(t *testing.T, r AgentRolloutReconciler, req ctrl.Request) string {
	t.Helper()
	c := meta.FindStatusCondition(rolloutCluster(t, r, req).Status.Conditions, "AgentRolloutReady")
	if c == nil {
		t.Fatal("missing rollout condition")
	}
	return c.Reason
}
func noRolloutOperation(t *testing.T, r AgentRolloutReconciler, req ctrl.Request) {
	t.Helper()
	if err := r.Client.Get(context.Background(), req.NamespacedName, &ext.AgentUpgrade{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected operation: %v", err)
	}
}

func TestIdleRolloutSelectsUnschedulableSourceButDoesNotGrantInstallation(t *testing.T) {
	r, req, upgrades, opReq := rolloutFixture(t)
	reconcileRollout(t, r, req, 3)
	op := readUpgrade(t, upgrades, opReq)
	if op.Spec.NodeRecord.UID != "node" || op.Spec.FromVersion != "old" || op.Status.Phase != "" || op.Status.Allocation != nil || op.Annotations[rolloutOwnerAnnotation] != rolloutOwner {
		t.Fatalf("unexpected intent: %+v", op)
	}
	var list ext.ContainerAllocationList
	checkUpgrade(t, r.Client.List(context.Background(), &list))
	if len(list.Items) != 0 {
		t.Fatal("selector reserved capacity")
	}
	reconcileUpgrade(t, upgrades, opReq, 1)
	if readUpgrade(t, upgrades, opReq).Status.Phase != "Reserved" {
		t.Fatal("selected intent did not use existing shared lock protocol")
	}
	reconcileRollout(t, AgentRolloutReconciler{Client: r.Client, Now: r.Now}, req, 2)
	if readUpgrade(t, upgrades, opReq).UID != op.UID {
		t.Fatal("restart replaced the operation")
	}
}

func TestDockerIdleRolloutReservesAndRecoversExactOperation(t *testing.T) {
	r, req, upgrades, opReq := rolloutFixture(t)
	ctx := context.Background()
	cc := rolloutCluster(t, r, req)
	cc.Spec.Runtime = "Docker"
	checkUpgrade(t, r.Client.Update(ctx, cc))
	var n ext.ContainerNode
	checkUpgrade(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &n))
	n.Spec.Runtime = "Docker"
	checkUpgrade(t, r.Client.Update(ctx, &n))
	var nn native.Node
	checkUpgrade(t, r.Client.Get(ctx, client.ObjectKey{Namespace: n.Spec.Node.Namespace, Name: n.Spec.Node.Name}, &nn))
	nn.Spec.AgentType = native.AgentTypeDocker
	// Cordon is workload scheduling intent; it does not pause idle rollouts.
	nn.Spec.Unschedulable = true
	checkUpgrade(t, r.Client.Update(ctx, &nn))
	reconcileRollout(t, r, req, 3)
	op := readUpgrade(t, upgrades, opReq)
	if op.Spec.NodeRecord.UID != string(n.UID) || op.Spec.InstanceID != n.Spec.InstanceID {
		t.Fatalf("Docker rollout lost exact Engine/node identity: %+v", op.Spec)
	}
	reconcileUpgrade(t, upgrades, opReq, 1)
	if readUpgrade(t, upgrades, opReq).Status.Phase != "Reserved" {
		t.Fatal("Docker rollout failed to reserve its idle host")
	}
	reconcileRollout(t, AgentRolloutReconciler{Client: r.Client, Now: r.Now}, req, 2)
	if readUpgrade(t, upgrades, opReq).UID != op.UID {
		t.Fatal("controller restart replaced Docker upgrade operation")
	}
	var held ext.ContainerAllocationList
	checkUpgrade(t, r.Client.List(ctx, &held))
	if len(held.Items) != 1 || held.Items[0].Spec.Upgrade == nil || held.Items[0].Spec.Upgrade.Operation.UID != string(op.UID) {
		t.Fatal("Docker rollout did not retain its exact exclusive lock")
	}
}

func TestIdleRolloutWaitsForFreshEnabledIdleUnallocatedNodes(t *testing.T) {
	for _, scenario := range []string{"busy", "stale", "future", "revoked", "disabled", "wrong-platform", "unvalidated", "stale-ready", "missing-gpu", "held-node", "held-address", "held-gpu", "cross-tenant", "old-native-uid"} {
		t.Run(scenario, func(t *testing.T) {
			r, req, _, opReq := rolloutFixture(t)
			ctx := context.Background()
			var n ext.ContainerNode
			checkUpgrade(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &n))
			switch scenario {
			case "busy":
				n.Status.Busy = true
			case "stale":
				n.Status.LastHeartbeatTime = &metav1.Time{Time: r.Now().Add(-time.Minute)}
			case "future":
				n.Status.LastHeartbeatTime = &metav1.Time{Time: r.Now().Add(time.Minute)}
			case "revoked":
				n.Status.AgentCredentialID = "old-credential"
			case "disabled":
				n.Status.Capabilities = nil
			case "wrong-platform":
				n.Status.Capabilities = []string{ext.AgentUpgradeCapability, "agent-platform/linux/arm64"}
			case "unvalidated":
				n.Status.Conditions = n.Status.Conditions[1:]
			case "stale-ready":
				meta.FindStatusCondition(n.Status.Conditions, "Ready").ObservedGeneration = 0
			case "missing-gpu":
				n.Spec.AuthorizedGPUUUIDs = nil
			case "cross-tenant":
				n.Labels = map[string]string{tenant.IDLabel: "other"}
			case "old-native-uid":
				n.Spec.Node.UID = "old"
			}
			if scenario == "missing-gpu" || scenario == "cross-tenant" || scenario == "old-native-uid" {
				checkUpgrade(t, r.Client.Update(ctx, &n))
			} else {
				checkUpgrade(t, r.Client.Status().Update(ctx, &n))
			}
			if scenario == "held-node" || scenario == "held-address" || scenario == "held-gpu" {
				a := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: "another-lock", Namespace: "other"}}
				switch scenario {
				case "held-node":
					a.Spec.Node = n.Spec.Node
				case "held-address":
					a.Spec.PrivateAddress = n.Spec.PrivateAddress
					a.Spec.NetworkScope = n.Spec.NetworkScope
				case "held-gpu":
					a.Spec.GPUUUIDs = n.Spec.AuthorizedGPUUUIDs
				}
				checkUpgrade(t, r.Client.Create(ctx, a))
			}
			reconcileRollout(t, r, req, 2)
			noRolloutOperation(t, r, opReq)
			if currentCondition(rolloutCluster(t, r, req).Status.Conditions, 1, "AgentRolloutReady") {
				t.Fatal("unready node reported complete")
			}
		})
	}
}

func TestIdleRolloutRejectsUnavailableTargetAndInvalidPolicy(t *testing.T) {
	for _, scenario := range []string{"missing-release", "unavailable", "release-tenant", "stale-release", "stale-cluster", "max-unavailable", "paused", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			r, req, _, opReq := rolloutFixture(t)
			ctx := context.Background()
			cc := rolloutCluster(t, r, req)
			var release ext.AgentRelease
			checkUpgrade(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "next"}, &release))
			switch scenario {
			case "missing-release":
				checkUpgrade(t, r.Client.Delete(ctx, &release))
			case "unavailable":
				release.Status.Available = false
				checkUpgrade(t, r.Client.Status().Update(ctx, &release))
			case "release-tenant":
				release.Labels = map[string]string{tenant.IDLabel: "other"}
				checkUpgrade(t, r.Client.Update(ctx, &release))
			case "stale-release":
				release.Status.ObservedGeneration = 0
				checkUpgrade(t, r.Client.Status().Update(ctx, &release))
			case "stale-cluster":
				cc.Status.ObservedGeneration = 0
				checkUpgrade(t, r.Client.Status().Update(ctx, cc))
			case "max-unavailable":
				cc.Spec.MaxUnavailable = 2
				checkUpgrade(t, r.Client.Update(ctx, cc))
			case "paused":
				cc.Spec.TargetAgentRelease = ""
				checkUpgrade(t, r.Client.Update(ctx, cc))
			case "empty":
				checkUpgrade(t, r.Client.Delete(ctx, &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents"}}))
			}
			reconcileRollout(t, r, req, 2)
			noRolloutOperation(t, r, opReq)
			if currentCondition(rolloutCluster(t, r, req).Status.Conditions, 1, "AgentRolloutReady") {
				t.Fatal("unavailable target reported complete")
			}
		})
	}
}

func TestIdleRolloutRequiresAllCurrentMembersFreshAtTarget(t *testing.T) {
	r, req, _, opReq := rolloutFixture(t)
	ctx := context.Background()
	var n ext.ContainerNode
	checkUpgrade(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &n))
	n.Status.AgentVersion = "next"
	checkUpgrade(t, r.Client.Status().Update(ctx, &n))
	reconcileRollout(t, r, req, 2)
	noRolloutOperation(t, r, opReq)
	if rolloutReason(t, r, req) != "AllNodesObserved" {
		t.Fatal("target version observation not recognized")
	}
	r.Now = func() time.Time { return n.Status.LastHeartbeatTime.Add(time.Minute) }
	reconcileRollout(t, r, req, 1)
	if currentCondition(rolloutCluster(t, r, req).Status.Conditions, 1, "AgentRolloutReady") {
		t.Fatal("lost heartbeats retained current rollout readiness")
	}
}

func TestIdleRolloutRecoversLostCommittedCreateAndStatus(t *testing.T) {
	for _, stage := range []string{"create", "status"} {
		t.Run(stage, func(t *testing.T) {
			r, req, upgrades, opReq := rolloutFixture(t)
			base := r.Client
			failed := false
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
					err := c.Create(ctx, o, opts...)
					if stage == "create" && !failed && err == nil {
						failed = true
						return errors.New("committed create response lost")
					}
					return err
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, name string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
					err := c.SubResource(name).Patch(ctx, o, p, opts...)
					if stage == "status" && !failed && err == nil {
						failed = true
						return errors.New("committed status response lost")
					}
					return err
				},
			})
			if _, err := r.Reconcile(context.Background(), req); err == nil || !failed {
				t.Fatal("lost response not injected")
			}
			op := readUpgrade(t, upgrades, opReq)
			reconcileRollout(t, AgentRolloutReconciler{Client: base, Now: r.Now}, req, 2)
			if readUpgrade(t, upgrades, opReq).UID != op.UID {
				t.Fatal("uncertain response created another operation")
			}
		})
	}
}

func autoMark(t *testing.T, r AgentUpgradeReconciler, req ctrl.Request) {
	t.Helper()
	op := readUpgrade(t, r, req)
	op.Annotations = map[string]string{rolloutOwnerAnnotation: rolloutOwner}
	checkUpgrade(t, r.Client.Update(context.Background(), op))
	op.Status.AcknowledgedAttemptID = op.Status.AttemptID
	checkUpgrade(t, r.Client.Status().Update(context.Background(), op))
}

func TestIdleRolloutNeverAdoptsManualOrForeignOperations(t *testing.T) {
	for _, scenario := range []string{"manual", "owner-uid", "cluster-record", "tenant", "annotation"} {
		t.Run(scenario, func(t *testing.T) {
			up, opReq := completedUpgradeFixture(t, "Succeeded")
			reconcileUpgrade(t, up, opReq, 2)
			op := readUpgrade(t, up, opReq)
			if scenario != "manual" {
				op.Annotations = map[string]string{rolloutOwnerAnnotation: rolloutOwner}
			}
			switch scenario {
			case "owner-uid":
				op.OwnerReferences[0].UID = "foreign"
			case "cluster-record":
				op.Spec.ClusterRecord.UID = "foreign"
			case "tenant":
				op.Labels = map[string]string{tenant.IDLabel: "other"}
			case "annotation":
				op.Annotations[rolloutOwnerAnnotation] = "foreign"
			}
			checkUpgrade(t, up.Client.Update(context.Background(), op))
			r := AgentRolloutReconciler(up)
			req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "agents", Name: "cloud"}}
			reconcileRollout(t, r, req, 2)
			op = readUpgrade(t, up, opReq)
			if !op.DeletionTimestamp.IsZero() || rolloutCluster(t, r, req).Status.LastAgentUpgrade != nil || rolloutReason(t, r, req) != "OperationConflict" {
				t.Fatal("foreign/manual operation adopted or deleted")
			}
		})
	}
}

func TestIdleRolloutTargetChangeCancelsOnlyUnclaimedOwnedOperation(t *testing.T) {
	for _, phase := range []string{"Reserved", "Applying", "Failed", "RolledBack"} {
		t.Run(phase, func(t *testing.T) {
			r, req, up, opReq := rolloutFixture(t)
			reconcileRollout(t, r, req, 1)
			reconcileUpgrade(t, up, opReq, 1)
			op := readUpgrade(t, up, opReq)
			op.Status.Phase = phase
			checkUpgrade(t, r.Client.Status().Update(context.Background(), op))
			cc := rolloutCluster(t, r, req)
			cc.Spec.TargetAgentRelease = ""
			checkUpgrade(t, r.Client.Update(context.Background(), cc))
			reconcileRollout(t, r, req, 2)
			op = readUpgrade(t, up, opReq)
			if !op.DeletionTimestamp.IsZero() {
				t.Fatalf("incorrect target-change deletion for %s", phase)
			}
		})
	}
}

func TestIdleRolloutRecordsSuccessBeforeDeleteAndIgnoresLaterWorkloadLock(t *testing.T) {
	up, opReq := completedUpgradeFixture(t, "Succeeded")
	autoMark(t, up, opReq)
	reconcileUpgrade(t, up, opReq, 2)
	r := AgentRolloutReconciler(up)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "agents", Name: "cloud"}}
	ctx := context.Background()
	op := readUpgrade(t, up, opReq)
	// A later Job may acquire the same node key after the exact old allocation
	// disappeared, before the previous operation is reaped.
	replacement := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: op.Status.Allocation.Name, Namespace: op.Status.Allocation.Namespace, Finalizers: []string{"example.com/workload"}}, Spec: ext.ContainerAllocationSpec{ExecutionID: "later-job"}}
	checkUpgrade(t, r.Client.Create(ctx, replacement))
	replacement.UID = types.UID("later-workload")
	checkUpgrade(t, r.Client.Update(ctx, replacement))
	reconcileRollout(t, r, req, 1)
	receipt := rolloutCluster(t, r, req).Status.LastAgentUpgrade
	if receipt == nil || receipt.Operation.UID != string(op.UID) || receipt.Release.UID != op.Spec.Release.UID {
		t.Fatal("exact success not recorded")
	}
	if !readUpgrade(t, up, opReq).DeletionTimestamp.IsZero() {
		t.Fatal("delete requested before receipt was durable")
	}
	reconcileRollout(t, r, req, 1)
	if readUpgrade(t, up, opReq).DeletionTimestamp.IsZero() {
		t.Fatal("exact successful operation not deleted")
	}
	reconcileRollout(t, r, req, 1)
	if rolloutReason(t, r, req) != "WaitingForOperationDeletion" {
		t.Fatal("advanced before observed deletion")
	}
	reconcileUpgrade(t, up, opReq, 1)
	noRolloutOperation(t, r, opReq)
	var held ext.ContainerAllocation
	checkUpgrade(t, r.Client.Get(ctx, client.ObjectKeyFromObject(replacement), &held))
	if held.UID != replacement.UID || held.Status.CleanupConfirmed || !held.DeletionTimestamp.IsZero() {
		t.Fatal("terminal reaping touched later workload")
	}
}

func TestIdleRolloutRetainsUnprovenAndRolledBackCompletion(t *testing.T) {
	for _, result := range []string{"Succeeded", "RolledBack"} {
		t.Run(result, func(t *testing.T) {
			up, opReq := completedUpgradeFixture(t, result)
			autoMark(t, up, opReq)
			op := readUpgrade(t, up, opReq)
			op.Status.Phase = result
			checkUpgrade(t, up.Client.Status().Update(context.Background(), op))
			r := AgentRolloutReconciler(up)
			req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "agents", Name: "cloud"}}
			reconcileRollout(t, r, req, 2)
			if !readUpgrade(t, up, opReq).DeletionTimestamp.IsZero() || rolloutCluster(t, r, req).Status.LastAgentUpgrade != nil {
				t.Fatal("unreleased/rolled back operation was reaped")
			}
			var allocations ext.ContainerAllocationList
			checkUpgrade(t, r.Client.List(context.Background(), &allocations))
			if len(allocations.Items) != 1 {
				t.Fatal("unproven lock released")
			}
		})
	}
}

func addRolloutWorker(t *testing.T, r AgentRolloutReconciler, name string) *ext.ContainerNode {
	t.Helper()
	ctx := context.Background()
	var n ext.ContainerNode
	checkUpgrade(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &n))
	var nn native.Node
	checkUpgrade(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn))
	nn.Name = name
	nn.UID = types.UID(name + "-native")
	nn.ResourceVersion = ""
	checkUpgrade(t, r.Client.Create(ctx, &nn))
	n.Name = name
	n.UID = types.UID(name + "-companion")
	n.ResourceVersion = ""
	n.Spec.Node = objectRef(&nn)
	n.Spec.PrivateAddress = "10.0.3.86"
	n.Spec.AuthorizedGPUUUIDs = []string{"GPU-b"}
	checkUpgrade(t, r.Client.Create(ctx, &n))
	return &n
}

func completeRolloutNode(t *testing.T, up AgentUpgradeReconciler, req ctrl.Request) {
	t.Helper()
	reconcileUpgrade(t, up, req, 1)
	op := readUpgrade(t, up, req)
	var n ext.ContainerNode
	checkUpgrade(t, up.Client.Get(context.Background(), client.ObjectKey{Namespace: op.Spec.NodeRecord.Namespace, Name: op.Spec.NodeRecord.Name}, &n))
	n.Status.AgentVersion = op.Spec.ReleaseSpec.Version
	checkUpgrade(t, up.Client.Status().Update(context.Background(), &n))
	start := metav1.NewTime(up.Now().Add(-2 * time.Second))
	heartbeat := metav1.NewTime(up.Now().Add(-time.Second))
	done := metav1.NewTime(up.Now())
	op.Status.Phase = "Applying"
	op.Status.AttemptID = "77777777-7777-7777-7777-777777777777"
	op.Status.StartedAt = &start
	op.Status.Completion = &ext.AgentUpgradeCompletion{AttemptID: op.Status.AttemptID, Result: "Succeeded", Version: op.Spec.ReleaseSpec.Version, HeartbeatAt: heartbeat, CompletedAt: done}
	op.Status.AcknowledgedAttemptID = op.Status.AttemptID
	condition(&op.Status.Conditions, op.Generation, "ApplicationGranted", true, "ScopedUpgradeClaimed", "Exact grant")
	condition(&op.Status.Conditions, op.Generation, "ApplicationComplete", true, "Succeeded", "Exact completion")
	checkUpgrade(t, up.Client.Status().Update(context.Background(), op))
	reconcileUpgrade(t, up, req, 2)
}

func TestIdleRolloutAdvancesTwoNodesInStableOrderAfterExactDeletion(t *testing.T) {
	r, req, up, opReq := rolloutFixture(t)
	second := addRolloutWorker(t, r, "z-worker")
	reconcileRollout(t, r, req, 2)
	op := readUpgrade(t, up, opReq)
	if op.Spec.NodeRecord.Name != "worker" {
		t.Fatal("node order was not stable")
	}
	completeRolloutNode(t, up, opReq)
	reconcileRollout(t, r, req, 1)
	reconcileRollout(t, r, req, 1)
	if readUpgrade(t, up, opReq).Spec.NodeRecord.Name != "worker" {
		t.Fatal("advanced before prior finalizer/deletion")
	}
	reconcileUpgrade(t, up, opReq, 1)
	noRolloutOperation(t, r, opReq)
	reconcileRollout(t, r, req, 2)
	op = readUpgrade(t, up, opReq)
	if op.Spec.NodeRecord.UID != string(second.UID) {
		t.Fatalf("did not advance to exact second node: %+v", op.Spec)
	}
	completeRolloutNode(t, up, opReq)
	reconcileRollout(t, r, req, 2)
	reconcileUpgrade(t, up, opReq, 1)
	reconcileRollout(t, r, req, 2)
	noRolloutOperation(t, r, opReq)
	cc := rolloutCluster(t, r, req)
	if !currentCondition(cc.Status.Conditions, cc.Generation, "AgentRolloutReady") || cc.Status.LastAgentUpgrade.NodeRecord.UID != string(second.UID) {
		t.Fatal("full rollout did not converge")
	}
}

func TestIdleRolloutSuccessReapingRecoversLostResponses(t *testing.T) {
	for _, stage := range []string{"receipt-before", "receipt-after", "delete-before", "delete-after"} {
		t.Run(stage, func(t *testing.T) {
			up, opReq := completedUpgradeFixture(t, "Succeeded")
			autoMark(t, up, opReq)
			reconcileUpgrade(t, up, opReq, 2)
			r := AgentRolloutReconciler(up)
			req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "agents", Name: "cloud"}}
			base := r.Client
			failed := false
			if stage == "delete-before" || stage == "delete-after" {
				reconcileRollout(t, r, req, 1)
			}
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, name string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
					if !failed && (stage == "receipt-before" || stage == "receipt-after") {
						failed = true
						if stage == "receipt-after" {
							checkUpgrade(t, c.SubResource(name).Patch(ctx, o, p, opts...))
						}
						return errors.New("receipt response lost")
					}
					return c.SubResource(name).Patch(ctx, o, p, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
					if !failed && (stage == "delete-before" || stage == "delete-after") {
						failed = true
						if stage == "delete-after" {
							checkUpgrade(t, c.Delete(ctx, o, opts...))
						}
						return errors.New("delete response lost")
					}
					return c.Delete(ctx, o, opts...)
				},
			})
			if _, err := r.Reconcile(context.Background(), req); err == nil || !failed {
				t.Fatal("lost response not injected")
			}
			r.Client = base
			reconcileRollout(t, r, req, 2)
			if rolloutCluster(t, r, req).Status.LastAgentUpgrade == nil {
				t.Fatal("receipt missing after restart")
			}
			if readUpgrade(t, up, opReq).DeletionTimestamp.IsZero() {
				t.Fatal("deletion not recovered")
			}
			reconcileUpgrade(t, up, opReq, 1)
			noRolloutOperation(t, r, opReq)
		})
	}
}

func TestIdleRolloutWaitsForDurableAgentJournalBeforeReaping(t *testing.T) {
	up, opReq := completedUpgradeFixture(t, "Succeeded")
	autoMark(t, up, opReq)
	reconcileUpgrade(t, up, opReq, 2)
	op := readUpgrade(t, up, opReq)
	op.Status.AcknowledgedAttemptID = ""
	checkUpgrade(t, up.Client.Status().Update(context.Background(), op))
	r := AgentRolloutReconciler(up)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "agents", Name: "cloud"}}
	reconcileRollout(t, r, req, 2)
	if !readUpgrade(t, up, opReq).DeletionTimestamp.IsZero() || rolloutCluster(t, r, req).Status.LastAgentUpgrade != nil || rolloutReason(t, r, req) != "WaitingForAgentJournal" {
		t.Fatal("operation reaped before durable Agent acknowledgement")
	}
	op = readUpgrade(t, up, opReq)
	op.Status.AcknowledgedAttemptID = op.Status.AttemptID
	checkUpgrade(t, up.Client.Status().Update(context.Background(), op))
	reconcileRollout(t, r, req, 2)
	if readUpgrade(t, up, opReq).DeletionTimestamp.IsZero() {
		t.Fatal("acknowledged completion not reaped")
	}
}

func TestIdleRolloutCancelsChangedTargetBeforeExposingAssignment(t *testing.T) {
	r, req, _, opReq := rolloutFixture(t)
	reconcileRollout(t, r, req, 1)
	cc := rolloutCluster(t, r, req)
	cc.Spec.TargetAgentRelease = ""
	checkUpgrade(t, r.Client.Update(context.Background(), cc))
	reconcileRollout(t, r, req, 1)
	noRolloutOperation(t, r, opReq)
}

func TestIdleRolloutDeletionUsesExactObjectPreconditions(t *testing.T) {
	up, opReq := completedUpgradeFixture(t, "Succeeded")
	autoMark(t, up, opReq)
	reconcileUpgrade(t, up, opReq, 2)
	r := AgentRolloutReconciler(up)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "agents", Name: "cloud"}}
	reconcileRollout(t, r, req, 1)
	base := r.Client
	op := readUpgrade(t, up, opReq)
	checked := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
		options := &client.DeleteOptions{}
		for _, option := range opts {
			option.ApplyToDelete(options)
		}
		if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil || *options.Preconditions.UID != op.UID || *options.Preconditions.ResourceVersion != op.ResourceVersion {
			t.Fatal("deletion omitted exact UID/resourceVersion")
		}
		checked = true
		return apierrors.NewConflict(ext.GroupVersion.WithResource("agentupgrades").GroupResource(), o.GetName(), errors.New("operation replaced"))
	}})
	if _, err := r.Reconcile(context.Background(), req); !apierrors.IsConflict(err) || !checked {
		t.Fatalf("delete conflict not preserved: %v", err)
	}
	if !readUpgrade(t, up, opReq).DeletionTimestamp.IsZero() {
		t.Fatal("conflicting deletion removed operation")
	}
}
