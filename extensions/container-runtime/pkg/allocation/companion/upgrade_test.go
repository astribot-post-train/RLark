package companion

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func upgradeFixture(t *testing.T) (Manager, Request, UpgradeRequest, *ext.AgentUpgrade) {
	t.Helper()
	m, task := fixture(t)
	ctx := context.Background()
	var node ext.ContainerNode
	mustUpgrade(t, m.Client.Get(ctx, task.Node, &node))
	node.Status.AgentVersion = "old"
	node.Status.Schedulable = false // A target-version fence must not prevent upgrading.
	node.Status.Conditions = append(node.Status.Conditions, metav1.Condition{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: node.Generation})
	mustUpgrade(t, m.Client.Status().Update(ctx, &node))
	var cc ext.ContainerCluster
	mustUpgrade(t, m.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "gpu"}, &cc))
	cc.Spec.TargetAgentRelease = "next"
	cc.Status.ObservedGeneration = cc.Generation
	cc.Status.Conditions = []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: cc.Generation}}
	mustUpgrade(t, m.Client.Update(ctx, &cc))
	release := &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "next", Namespace: "agents", UID: "release-uid", Generation: 1}, Spec: ext.AgentReleaseSpec{Version: "next", OS: "linux", Architecture: "amd64", Protocol: "v1", SHA256: strings.Repeat("0", 64), ArtifactPath: "agent"}, Status: ext.AgentReleaseStatus{Available: true, ObservedGeneration: 1}}
	mustUpgrade(t, m.Client.Create(ctx, release))
	op := &ext.AgentUpgrade{ObjectMeta: metav1.ObjectMeta{Name: UpgradeName(cc.Spec.Cluster.UID), Namespace: cc.Namespace, UID: "operation-uid", Generation: 1, Finalizers: []string{UpgradeFinalizer}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&cc, ext.GroupVersion.WithKind("ContainerCluster"))}}, Spec: ext.AgentUpgradeSpec{Cluster: cc.Spec.Cluster, ClusterRecord: reference(&cc), NodeRecord: reference(&node), Release: reference(release), ReleaseSpec: release.Spec, TenantID: tenant.DefaultID, InstanceID: node.Spec.InstanceID, CredentialID: node.Annotations[ext.AgentCredentialAnnotation], FromVersion: "old"}}
	mustUpgrade(t, m.Client.Create(ctx, op))
	return m, task, UpgradeRequest{Operation: client.ObjectKeyFromObject(op)}, op
}
func mustUpgrade(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeReserveRecoversExactLockAndBlocksTaskRelease(t *testing.T) {
	m, _, req, op := upgradeFixture(t)
	ctx := context.Background()
	first, err := m.ReserveUpgrade(ctx, req)
	mustUpgrade(t, err)
	if first.Spec.Upgrade == nil || first.Spec.Task.UID != "" || first.Name != Name(first.Spec.Node.UID) {
		t.Fatalf("invalid upgrade lock: %#v", first)
	}
	again, err := (Manager{Client: m.Client, Now: m.Now}).ReserveUpgrade(ctx, req)
	mustUpgrade(t, err)
	if first.UID != again.UID {
		t.Fatal("restart did not recover exact lock")
	}
	if err := m.Release(ctx, client.ObjectKeyFromObject(first), string(first.UID)); !errors.Is(err, ErrIdentity) {
		t.Fatalf("Task cleanup released upgrade lock: %v", err)
	}
	if err := m.ReleaseUpgrade(ctx, client.ObjectKeyFromObject(first), string(first.UID), string(op.UID)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unproved upgrade cleanup released lock: %v", err)
	}
}

func TestUpgradeReserveDeniesInvalidAndStaleInputs(t *testing.T) {
	for _, mode := range []string{"foreign tenant", "wrong operation name", "wrong node UID", "credential rotation", "busy", "stale heartbeat", "future heartbeat", "missing validation", "target replacement", "missing release", "foreign release", "invalid digest", "wrong cluster owner", "applying", "duplicate address"} {
		t.Run(mode, func(t *testing.T) {
			m, task, req, op := upgradeFixture(t)
			ctx := context.Background()
			var node ext.ContainerNode
			mustUpgrade(t, m.Client.Get(ctx, task.Node, &node))
			switch mode {
			case "foreign tenant":
				op.Spec.TenantID = "other"
				mustUpgrade(t, m.Client.Update(ctx, op))
			case "wrong operation name":
				mustUpgrade(t, m.Client.Delete(ctx, op))
				op.ResourceVersion = ""
				op.Finalizers = nil
				op.Name = "other"
				mustUpgrade(t, m.Client.Create(ctx, op))
				req.Operation = client.ObjectKeyFromObject(op)
			case "wrong node UID":
				op.Spec.NodeRecord.UID = "old"
				mustUpgrade(t, m.Client.Update(ctx, op))
			case "credential rotation":
				node.Annotations[ext.AgentCredentialAnnotation] = "rotated"
				mustUpgrade(t, m.Client.Update(ctx, &node))
			case "busy":
				node.Status.Busy = true
				mustUpgrade(t, m.Client.Status().Update(ctx, &node))
			case "stale heartbeat":
				node.Status.LastHeartbeatTime = &metav1.Time{Time: m.now().Add(-time.Minute)}
				mustUpgrade(t, m.Client.Status().Update(ctx, &node))
			case "future heartbeat":
				node.Status.LastHeartbeatTime = &metav1.Time{Time: m.now().Add(time.Minute)}
				mustUpgrade(t, m.Client.Status().Update(ctx, &node))
			case "missing validation":
				node.Status.Conditions = node.Status.Conditions[:1]
				mustUpgrade(t, m.Client.Status().Update(ctx, &node))
			case "target replacement":
				var cc ext.ContainerCluster
				mustUpgrade(t, m.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "gpu"}, &cc))
				cc.Spec.TargetAgentRelease = "other"
				mustUpgrade(t, m.Client.Update(ctx, &cc))
			case "missing release":
				mustUpgrade(t, m.Client.Delete(ctx, &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "next"}}))
			case "foreign release":
				var release ext.AgentRelease
				mustUpgrade(t, m.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "next"}, &release))
				release.Labels = map[string]string{tenant.IDLabel: "other"}
				mustUpgrade(t, m.Client.Update(ctx, &release))
			case "invalid digest":
				op.Spec.ReleaseSpec.SHA256 = "bad"
				mustUpgrade(t, m.Client.Update(ctx, op))
			case "wrong cluster owner":
				op.OwnerReferences[0].UID = "old"
				mustUpgrade(t, m.Client.Update(ctx, op))
			case "applying":
				op.Status.Phase = "Applying"
				mustUpgrade(t, m.Client.Update(ctx, op))
			case "duplicate address":
				other := node.DeepCopy()
				other.Name = "duplicate"
				other.UID = "duplicate"
				other.ResourceVersion = ""
				mustUpgrade(t, m.Client.Create(ctx, other))
			}
			if _, err := m.ReserveUpgrade(ctx, req); err == nil {
				t.Fatal("invalid upgrade reserved capacity")
			}
			var held ext.ContainerAllocationList
			mustUpgrade(t, m.Client.List(ctx, &held))
			if len(held.Items) != 0 {
				t.Fatal("denied upgrade created a lock")
			}
		})
	}
}

func TestUpgradeLostCreateResponseRecoversWithoutDuplicate(t *testing.T) {
	m, _, req, _ := upgradeFixture(t)
	ctx := context.Background()
	base := m.Client
	once := true
	m.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		err := c.Create(ctx, obj, opts...)
		if err == nil && once {
			once = false
			return errors.New("lost create response")
		}
		return err
	}})
	if _, err := m.ReserveUpgrade(ctx, req); err == nil {
		t.Fatal("injected lost response was not seen")
	}
	a, err := m.ReserveUpgrade(ctx, req)
	mustUpgrade(t, err)
	var held ext.ContainerAllocationList
	mustUpgrade(t, base.List(ctx, &held))
	if len(held.Items) != 1 || a.UID != held.Items[0].UID {
		t.Fatal("lost response created duplicate lock")
	}
}

func TestUpgradeRevalidatesCredentialAfterCreate(t *testing.T) {
	m, task, req, _ := upgradeFixture(t)
	ctx := context.Background()
	base := m.Client
	m.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		var n ext.ContainerNode
		if err := c.Get(ctx, task.Node, &n); err != nil {
			return err
		}
		n.Annotations[ext.AgentCredentialAnnotation] = "rotated"
		return c.Update(ctx, &n)
	}})
	if _, err := m.ReserveUpgrade(ctx, req); !errors.Is(err, ErrIdentity) {
		t.Fatalf("changed credential granted lock: %v", err)
	}
	var held ext.ContainerAllocationList
	mustUpgrade(t, base.List(ctx, &held))
	if len(held.Items) != 1 {
		t.Fatal("uncertain create was incorrectly released")
	}
}

func TestWorkloadAndUpgradeCompeteOnSameCreate(t *testing.T) {
	m, task, req, _ := upgradeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cc ext.ContainerCluster
	key := client.ObjectKey{Namespace: "agents", Name: "gpu"}
	mustUpgrade(t, m.Client.Get(ctx, key, &cc))
	cc.Spec.TargetAgentRelease = ""
	mustUpgrade(t, m.Client.Update(ctx, &cc))
	var node ext.ContainerNode
	mustUpgrade(t, m.Client.Get(ctx, task.Node, &node))
	node.Status.Schedulable = true
	mustUpgrade(t, m.Client.Status().Update(ctx, &node))
	reached := make(chan struct{})
	unblock := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(unblock) }) }
	defer release()
	base := m.Client
	m.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if a, ok := obj.(*ext.ContainerAllocation); ok && a.Spec.Upgrade == nil {
			close(reached)
			select {
			case <-unblock:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return c.Create(ctx, obj, opts...)
	}})
	result := make(chan error, 1)
	go func() { _, err := m.Acquire(ctx, task); result <- err }()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("Task did not reach stale-state Create")
	}
	mustUpgrade(t, base.Get(ctx, key, &cc))
	cc.Spec.TargetAgentRelease = "next"
	mustUpgrade(t, base.Update(ctx, &cc))
	upgraded, err := m.ReserveUpgrade(ctx, req)
	mustUpgrade(t, err)
	if upgraded.Spec.Upgrade == nil {
		t.Fatal("upgrade did not own node lock")
	}
	release()
	if err := <-result; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale workload did not lose shared Create: %v", err)
	}
	var held ext.ContainerAllocationList
	mustUpgrade(t, base.List(ctx, &held))
	if len(held.Items) != 1 || held.Items[0].Spec.Upgrade == nil {
		t.Fatal("concurrent workload changed upgrade ownership")
	}

}
