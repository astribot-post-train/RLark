package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func upgradeControllerFixture(t *testing.T) (AgentUpgradeReconciler, ctrl.Request) {
	t.Helper()
	now := time.Now()
	cluster := &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster"}, Spec: native.ClusterSpec{AgentNamespace: "agents"}}
	clusterRef := ext.NativeObjectReference{Name: cluster.Name, UID: string(cluster.UID)}
	cc := &ext.ContainerCluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Namespace: "agents", UID: "cc", Generation: 1}, Spec: ext.ContainerClusterSpec{Cluster: clusterRef, Runtime: "Raw", AgentNamespace: "agents", TargetAgentRelease: "next"}, Status: ext.ContainerClusterStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	nativeNode := &native.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "native-node"}, Spec: native.NodeSpec{AgentType: native.AgentTypeRaw}}
	node := &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "agents", UID: "node", Generation: 1, Annotations: map[string]string{ext.AgentCredentialAnnotation: "credential"}}, Spec: ext.ContainerNodeSpec{Cluster: clusterRef, Node: ext.NativeObjectReference{Name: nativeNode.Name, Namespace: nativeNode.Namespace, UID: string(nativeNode.UID)}, Runtime: "Raw", InstanceID: "instance", PrivateAddress: "10.0.3.85", Environment: "prepared", NetworkScope: "cloud", AuthorizedGPUUUIDs: []string{"GPU-a"}}, Status: ext.ContainerNodeStatus{ObservedGeneration: 1, AgentCredentialID: "credential", AgentVersion: "old", LastHeartbeatTime: &metav1.Time{Time: now}, Conditions: []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: 1}, {Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	release := &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "next", Namespace: "agents", UID: "release", Generation: 1}, Spec: ext.AgentReleaseSpec{Version: "next", OS: "linux", Architecture: "amd64", Protocol: "v1", SHA256: strings.Repeat("0", 64), ArtifactPath: "agent"}, Status: ext.AgentReleaseStatus{Available: true, ObservedGeneration: 1}}
	op := &ext.AgentUpgrade{ObjectMeta: metav1.ObjectMeta{Name: companion.UpgradeName(string(cluster.UID)), Namespace: "agents", UID: "operation", Generation: 1, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cc, ext.GroupVersion.WithKind("ContainerCluster"))}}, Spec: ext.AgentUpgradeSpec{Cluster: clusterRef, ClusterRecord: ext.NativeObjectReference{Name: cc.Name, Namespace: cc.Namespace, UID: string(cc.UID)}, NodeRecord: ext.NativeObjectReference{Name: node.Name, Namespace: node.Namespace, UID: string(node.UID)}, Release: ext.NativeObjectReference{Name: release.Name, Namespace: release.Namespace, UID: string(release.UID)}, ReleaseSpec: release.Spec, TenantID: tenant.DefaultID, InstanceID: node.Spec.InstanceID, CredentialID: "credential", FromVersion: "old"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(op, node, cc, release, &ext.ContainerAllocation{}).WithObjects(cluster, nativeNode, cc, node, release, op).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*ext.ContainerAllocation); ok {
			obj.SetUID(types.UID("allocation"))
			obj.SetGeneration(1)
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	return AgentUpgradeReconciler{Client: c, Now: func() time.Time { return now }}, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}
}
func checkUpgrade(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func reconcileUpgrade(t *testing.T, r AgentUpgradeReconciler, req ctrl.Request, count int) {
	t.Helper()
	for range count {
		_, err := r.Reconcile(context.Background(), req)
		checkUpgrade(t, err)
	}
}
func readUpgrade(t *testing.T, r AgentUpgradeReconciler, req ctrl.Request) *ext.AgentUpgrade {
	t.Helper()
	var op ext.AgentUpgrade
	checkUpgrade(t, r.Client.Get(context.Background(), req.NamespacedName, &op))
	return &op
}

func TestUpgradeControllerReservesWithoutStartingAndRecovers(t *testing.T) {
	r, req := upgradeControllerFixture(t)
	reconcileUpgrade(t, r, req, 2)
	op := readUpgrade(t, r, req)
	if op.Status.Phase != "Reserved" || op.Status.Allocation == nil || op.Status.Allocation.UID != "allocation" || op.Status.StartedAt != nil {
		t.Fatalf("reservation implied installation: %#v", op.Status)
	}
	reconcileUpgrade(t, AgentUpgradeReconciler{Client: r.Client, Now: r.Now}, req, 2)
	var list ext.ContainerAllocationList
	checkUpgrade(t, r.Client.List(context.Background(), &list))
	if len(list.Items) != 1 || list.Items[0].Spec.Upgrade.Operation.UID != string(op.UID) {
		t.Fatal("restart did not recover lock")
	}
}

func TestUpgradeCancellationFindsLockAfterLostStatusResponse(t *testing.T) {
	r, req := upgradeControllerFixture(t)
	ctx := context.Background()
	base := r.Client
	failed := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, name string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if _, ok := obj.(*ext.AgentUpgrade); ok && name == "status" && !failed {
			failed = true
			return errors.New("status write lost before commit")
		}
		return c.SubResource(name).Patch(ctx, obj, patch, opts...)
	}})
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("lost status not injected")
	}
	r.Client = base
	op := readUpgrade(t, r, req)
	if op.Status.Allocation != nil {
		t.Fatal("test did not leave unwitnessed allocation")
	}
	checkUpgrade(t, r.Client.Delete(ctx, op))
	reconcileUpgrade(t, r, req, 4)
	if err := r.Client.Get(ctx, req.NamespacedName, &ext.AgentUpgrade{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cancelled operation retained: %v", err)
	}
	var held ext.ContainerAllocationList
	checkUpgrade(t, r.Client.List(ctx, &held))
	if len(held.Items) != 0 {
		t.Fatal("cancelled upgrade retained lock")
	}
}

func TestUpgradeCancellationWaitsForObservedAllocationDeletion(t *testing.T) {
	r, req := upgradeControllerFixture(t)
	ctx := context.Background()
	reconcileUpgrade(t, r, req, 1)
	op := readUpgrade(t, r, req)
	var allocation ext.ContainerAllocation
	checkUpgrade(t, r.Client.Get(ctx, client.ObjectKey{Namespace: op.Status.Allocation.Namespace, Name: op.Status.Allocation.Name}, &allocation))
	allocation.Finalizers = append(allocation.Finalizers, "example.com/other-owner")
	checkUpgrade(t, r.Client.Update(ctx, &allocation))
	checkUpgrade(t, r.Client.Delete(ctx, op))
	reconcileUpgrade(t, r, req, 1)
	if _, err := r.Reconcile(ctx, req); !errors.Is(err, companion.ErrUnavailable) {
		t.Fatalf("did not wait for exact deletion: %v", err)
	}
	op = readUpgrade(t, r, req)
	if op.Status.Phase != "Cancelled" {
		t.Fatal("cancellation intent not durable")
	}
	checkUpgrade(t, r.Client.Get(ctx, client.ObjectKeyFromObject(&allocation), &allocation))
	if allocation.DeletionTimestamp.IsZero() {
		t.Fatal("allocation deletion not requested")
	}
	allocation.Finalizers = nil
	checkUpgrade(t, r.Client.Update(ctx, &allocation))
	reconcileUpgrade(t, r, req, 1)
	if err := r.Client.Get(ctx, req.NamespacedName, &ext.AgentUpgrade{}); !apierrors.IsNotFound(err) {
		t.Fatalf("observed deletion did not release operation: %v", err)
	}
}

func TestApplyingUpgradeDeletionNeverReleasesUncertainInstallation(t *testing.T) {
	r, req := upgradeControllerFixture(t)
	ctx := context.Background()
	reconcileUpgrade(t, r, req, 1)
	op := readUpgrade(t, r, req)
	op.Status.Phase = "Applying"
	checkUpgrade(t, r.Client.Status().Update(ctx, op))
	checkUpgrade(t, r.Client.Delete(ctx, op))
	reconcileUpgrade(t, r, req, 3)
	op = readUpgrade(t, r, req)
	if op.Status.Phase != "Applying" {
		t.Fatal("controller reset Applying")
	}
	var held ext.ContainerAllocationList
	checkUpgrade(t, r.Client.List(ctx, &held))
	if len(held.Items) != 1 {
		t.Fatal("deletion released an applying upgrade")
	}
}

func TestUpgradeOperationOwnershipIsValidatedBeforeFinalizer(t *testing.T) {
	r, req := upgradeControllerFixture(t)
	ctx := context.Background()
	op := readUpgrade(t, r, req)
	op.OwnerReferences[0].UID = "foreign"
	checkUpgrade(t, r.Client.Update(ctx, op))
	if _, err := r.Reconcile(ctx, req); !errors.Is(err, companion.ErrIdentity) {
		t.Fatalf("foreign operation adopted: %v", err)
	}
	op = readUpgrade(t, r, req)
	if len(op.Finalizers) != 0 {
		t.Fatal("foreign operation was protected")
	}
}

func TestUpgradeCancellationDoesNotRequireOldCredentialOrAvailableTarget(t *testing.T) {
	r, req := upgradeControllerFixture(t)
	ctx := context.Background()
	reconcileUpgrade(t, r, req, 1)
	var node ext.ContainerNode
	key := client.ObjectKey{Namespace: "agents", Name: "worker"}
	checkUpgrade(t, r.Client.Get(ctx, key, &node))
	node.Annotations[ext.AgentCredentialAnnotation] = "rotated"
	checkUpgrade(t, r.Client.Update(ctx, &node))
	checkUpgrade(t, r.Client.Delete(ctx, &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "next"}}))
	op := readUpgrade(t, r, req)
	checkUpgrade(t, r.Client.Delete(ctx, op))
	reconcileUpgrade(t, r, req, 3)
	if err := r.Client.Get(ctx, req.NamespacedName, &ext.AgentUpgrade{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cancel blocked on revoked authorization: %v", err)
	}
}

func TestUpgradeIntentProtectsNodeAndClusterBeforeAllocation(t *testing.T) {
	r, req := upgradeControllerFixture(t)
	ctx := context.Background()
	// The operation exists but has not attempted a node reservation yet.
	nodeController := ContainerNodeReconciler(r)
	nodeKey := client.ObjectKey{Namespace: "agents", Name: "worker"}
	_, err := nodeController.Reconcile(ctx, ctrl.Request{NamespacedName: nodeKey})
	checkUpgrade(t, err)
	var node ext.ContainerNode
	checkUpgrade(t, r.Client.Get(ctx, nodeKey, &node))
	checkUpgrade(t, r.Client.Delete(ctx, &node))
	_, err = nodeController.Reconcile(ctx, ctrl.Request{NamespacedName: nodeKey})
	checkUpgrade(t, err)
	checkUpgrade(t, r.Client.Get(ctx, nodeKey, &node))
	if node.Status.Schedulable || !controllerutil.ContainsFinalizer(&node, nodeFinalizer) {
		t.Fatal("node was deleted while upgrade intent referenced its exact UID")
	}
	ccKey := client.ObjectKey{Namespace: "agents", Name: "cloud"}
	clusterController := ContainerClusterReconciler{Client: r.Client}
	_, err = clusterController.Reconcile(ctx, ctrl.Request{NamespacedName: ccKey})
	checkUpgrade(t, err)
	var cc ext.ContainerCluster
	checkUpgrade(t, r.Client.Get(ctx, ccKey, &cc))
	checkUpgrade(t, r.Client.Delete(ctx, &cc))
	_, err = clusterController.Reconcile(ctx, ctrl.Request{NamespacedName: ccKey})
	checkUpgrade(t, err)
	checkUpgrade(t, r.Client.Get(ctx, ccKey, &cc))
	if !controllerutil.ContainsFinalizer(&cc, clusterFinalizer) {
		t.Fatal("cluster lost protection before upgrade cleanup")
	}
	if err := r.Client.Get(ctx, req.NamespacedName, &ext.AgentUpgrade{}); err != nil {
		t.Fatal(err)
	}
}

func completedUpgradeFixture(t *testing.T, result string) (AgentUpgradeReconciler, ctrl.Request) {
	t.Helper()
	r, req := upgradeControllerFixture(t)
	reconcileUpgrade(t, r, req, 1)
	op := readUpgrade(t, r, req)
	start := metav1.NewTime(r.Now().Add(-2 * time.Second))
	heartbeat := metav1.NewTime(r.Now().Add(-time.Second))
	done := metav1.NewTime(r.Now())
	version := op.Spec.ReleaseSpec.Version
	if result == "RolledBack" {
		version = op.Spec.FromVersion
	}
	op.Status.Phase = "Applying"
	op.Status.AttemptID = "77777777-7777-7777-7777-777777777777"
	op.Status.StartedAt = &start
	op.Status.Completion = &ext.AgentUpgradeCompletion{AttemptID: op.Status.AttemptID, Result: result, Version: version, HeartbeatAt: heartbeat, CompletedAt: done}
	condition(&op.Status.Conditions, op.Generation, "ApplicationGranted", true, "ScopedUpgradeClaimed", "Exact grant")
	condition(&op.Status.Conditions, op.Generation, "ApplicationComplete", true, result, "Exact completion")
	checkUpgrade(t, r.Client.Status().Update(context.Background(), op))
	return r, req
}

func TestUpgradeCompletionWaitsForDeletionAndRetainsTerminalOperation(t *testing.T) {
	for _, result := range []string{"Succeeded", "RolledBack"} {
		t.Run(result, func(t *testing.T) {
			r, req := completedUpgradeFixture(t, result)
			ctx := context.Background()
			op := readUpgrade(t, r, req)
			var a ext.ContainerAllocation
			key := client.ObjectKey{Namespace: op.Status.Allocation.Namespace, Name: op.Status.Allocation.Name}
			checkUpgrade(t, r.Client.Get(ctx, key, &a))
			a.Finalizers = append(a.Finalizers, "example.com/delay")
			checkUpgrade(t, r.Client.Update(ctx, &a))
			reconcileUpgrade(t, r, req, 2)
			op = readUpgrade(t, r, req)
			if op.Status.Phase != "Applying" {
				t.Fatal("terminal before allocation deletion")
			}
			checkUpgrade(t, r.Client.Get(ctx, key, &a))
			if !a.Status.CleanupConfirmed || a.DeletionTimestamp.IsZero() {
				t.Fatal("completion cleanup not initiated")
			}
			a.Finalizers = nil
			checkUpgrade(t, r.Client.Update(ctx, &a))
			// Reconstruct the controller after deletion; the durable receipt suffices.
			r = AgentUpgradeReconciler{Client: r.Client, Now: r.Now}
			reconcileUpgrade(t, r, req, 2)
			op = readUpgrade(t, r, req)
			if op.Status.Phase != result || !controllerutil.ContainsFinalizer(op, companion.UpgradeFinalizer) {
				t.Fatal("terminal operation not retained")
			}
			checkUpgrade(t, r.Client.Delete(ctx, op))
			reconcileUpgrade(t, r, req, 1)
			if err := r.Client.Get(ctx, req.NamespacedName, &ext.AgentUpgrade{}); !apierrors.IsNotFound(err) {
				t.Fatalf("explicit terminal deletion blocked: %v", err)
			}
		})
	}
}

func TestUpgradeCompletionRecoversLostEffects(t *testing.T) {
	for _, stage := range []string{"allocation-status", "delete", "finalizer", "operation-status"} {
		t.Run(stage, func(t *testing.T) {
			r, req := completedUpgradeFixture(t, "Succeeded")
			base := r.Client
			failed := false
			lost := func(err error) error {
				if err != nil {
					return err
				}
				failed = true
				return errors.New("committed response lost")
			}
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, name string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					_, isAllocation := obj.(*ext.ContainerAllocation)
					_, isOperation := obj.(*ext.AgentUpgrade)
					if !failed && name == "status" && (stage == "allocation-status" && isAllocation || stage == "operation-status" && isOperation) {
						return lost(c.SubResource(name).Patch(ctx, obj, patch, opts...))
					}
					return c.SubResource(name).Patch(ctx, obj, patch, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*ext.ContainerAllocation); ok && !failed && stage == "delete" {
						return lost(c.Delete(ctx, obj, opts...))
					}
					return c.Delete(ctx, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*ext.ContainerAllocation); ok && !failed && stage == "finalizer" {
						return lost(c.Patch(ctx, obj, patch, opts...))
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			if _, err := r.Reconcile(context.Background(), req); err == nil || !failed {
				t.Fatal("lost effect not injected")
			}
			r.Client = base
			reconcileUpgrade(t, r, req, 3)
			op := readUpgrade(t, r, req)
			if op.Status.Phase != "Succeeded" {
				t.Fatal("committed effect did not recover")
			}
		})
	}
}

func TestUpgradeCompletionNeverTouchesReplacementOrInvalidProof(t *testing.T) {
	for _, scenario := range []string{"replacement", "wrong-attempt", "stale-heartbeat"} {
		t.Run(scenario, func(t *testing.T) {
			r, req := completedUpgradeFixture(t, "Succeeded")
			ctx := context.Background()
			op := readUpgrade(t, r, req)
			key := client.ObjectKey{Namespace: op.Status.Allocation.Namespace, Name: op.Status.Allocation.Name}
			var a ext.ContainerAllocation
			checkUpgrade(t, r.Client.Get(ctx, key, &a))
			if scenario == "replacement" {
				a.Finalizers = nil
				checkUpgrade(t, r.Client.Update(ctx, &a))
				checkUpgrade(t, r.Client.Delete(ctx, &a))
				a.ResourceVersion = ""
				a.UID = "replacement"
				a.DeletionTimestamp = nil
				a.Status = ext.ContainerAllocationStatus{}
				a.Finalizers = []string{"example.com/foreign"}
				checkUpgrade(t, r.Client.Create(ctx, &a))
				// The fixture assigns UIDs on Create; simulate the API's next UID.
				a.UID = "replacement"
				checkUpgrade(t, r.Client.Update(ctx, &a))
			} else {
				if scenario == "wrong-attempt" {
					op.Status.Completion.AttemptID = "other"
				} else {
					op.Status.Completion.HeartbeatAt = *op.Status.StartedAt
				}
				checkUpgrade(t, r.Client.Status().Update(ctx, op))
			}
			_, err := r.Reconcile(ctx, req)
			if scenario != "replacement" && !errors.Is(err, companion.ErrIdentity) {
				t.Fatalf("invalid proof accepted: %v", err)
			}
			checkUpgrade(t, r.Client.Get(ctx, key, &a))
			if a.Status.CleanupConfirmed || !a.DeletionTimestamp.IsZero() {
				t.Fatal("replacement/invalid proof released allocation")
			}
		})
	}
}
