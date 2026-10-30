package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type upgradeTransportFixture struct {
	reporter Reporter
	identity Identity
	op       *ext.AgentUpgrade
	alloc    *ext.ContainerAllocation
	base     client.Client
}

func newUpgradeTransportFixture(t *testing.T) upgradeTransportFixture {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("Linux ELF artifact")
	}
	ctx := context.Background()
	r, id, node := observerFixture(t)
	var cc ext.ContainerCluster
	mustAgent(t, r.Client.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: "cloud"}, &cc))
	cc.Spec.TargetAgentRelease = "next"
	cc.Status.ObservedGeneration = cc.Generation
	cc.Status.Conditions = []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: cc.Generation}}
	mustAgent(t, r.Client.Update(ctx, &cc))
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	root := t.TempDir()
	mustAgent(t, os.WriteFile(filepath.Join(root, "agent"), data, 0700))
	release := &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "next", Namespace: node.Namespace, UID: "release", Generation: 1, Labels: map[string]string{tenant.IDLabel: tenant.DefaultID}}, Spec: ext.AgentReleaseSpec{Version: "next", OS: "linux", Architecture: runtime.GOARCH, Protocol: "v1", SHA256: hex.EncodeToString(digest[:]), ArtifactPath: "agent"}, Status: ext.AgentReleaseStatus{Available: true, ObservedGeneration: 1}}
	mustAgent(t, r.Client.Create(ctx, release))
	// The source version is observed before the target release is claimed.
	mustAgent(t, r.Report(ctx, id, Observation{Version: "old", PrivateAddress: node.Spec.PrivateAddress, VisibleGPUUUIDs: node.Spec.AuthorizedGPUUUIDs}))
	var nc native.Cluster
	var nn native.Node
	mustAgent(t, r.Client.Get(ctx, client.ObjectKey{Name: "cloud"}, &nc))
	mustAgent(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn))
	mustAgent(t, r.Client.Get(ctx, client.ObjectKeyFromObject(node), node))
	r.Client = fake.NewClientBuilder().WithScheme(r.Client.Scheme()).WithStatusSubresource(node, &ext.AgentUpgrade{}, &ext.ContainerAllocation{}).WithObjects(&nc, &nn, &cc, node, release).Build()
	op := &ext.AgentUpgrade{ObjectMeta: metav1.ObjectMeta{Name: companion.UpgradeName(node.Spec.Cluster.UID), Namespace: node.Namespace, UID: "operation", Generation: 1, Finalizers: []string{companion.UpgradeFinalizer}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&cc, ext.GroupVersion.WithKind("ContainerCluster"))}}, Spec: ext.AgentUpgradeSpec{Cluster: node.Spec.Cluster, ClusterRecord: upgradeObjectRef(&cc), NodeRecord: upgradeObjectRef(node), Release: upgradeObjectRef(release), ReleaseSpec: release.Spec, TenantID: tenant.DefaultID, InstanceID: node.Spec.InstanceID, CredentialID: id.CredentialID, FromVersion: "old"}}
	mustAgent(t, r.Client.Create(ctx, op))
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if a, ok := obj.(*ext.ContainerAllocation); ok {
			a.UID = "allocation"
			a.Generation = 1
		}
		return c.Create(ctx, obj, opts...)
	}})
	manager := companion.Manager{Client: r.Client, Now: r.Now, HeartbeatTimeout: 45 * time.Second}
	allocation, err := manager.ReserveUpgrade(ctx, companion.UpgradeRequest{Operation: client.ObjectKeyFromObject(op)})
	if err != nil {
		t.Fatal(err)
	}
	ref := upgradeObjectRef(allocation)
	op.Status = ext.AgentUpgradeStatus{Phase: "Reserved", ObservedGeneration: op.Generation, Allocation: &ref}
	mustAgent(t, r.Client.Status().Update(ctx, op))
	return upgradeTransportFixture{reporter: Reporter{Client: r.Client, Now: r.Now, ReleaseStore: mustStore(t, root)}, identity: id, op: op, alloc: allocation, base: base}
}

func mustStore(t *testing.T, root string) artifact.Store {
	t.Helper()
	store, err := artifact.NewDirectoryStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func mustAgent(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func upgradeObjectRef(o metav1.Object) ext.NativeObjectReference {
	return ext.NativeObjectReference{Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())}
}

func readUpgradeOperation(t *testing.T, c client.Reader, key client.ObjectKey) ext.AgentUpgrade {
	t.Helper()
	var op ext.AgentUpgrade
	mustAgent(t, c.Get(context.Background(), key, &op))
	return op
}

func TestUpgradeTransportClaimsAndCompletesOnlyWithFreshIdleHeartbeat(t *testing.T) {
	f := newUpgradeTransportFixture(t)
	ctx := context.Background()
	op := readUpgradeOperation(t, f.base, client.ObjectKeyFromObject(f.op))
	allocation := ext.NativeObjectReference{Namespace: f.alloc.Namespace, Name: f.alloc.Name, UID: string(f.alloc.UID)}
	request := UpgradeRequest{Action: "claim", Operation: upgradeObjectRef(&op), Allocation: allocation, AttemptID: "11111111-1111-1111-1111-111111111111", ResourceVersion: op.ResourceVersion}
	claimed, err := f.reporter.Upgrade(ctx, f.identity, request)
	mustAgent(t, err)
	if !claimed.Granted || claimed.Phase != "Applying" || claimed.AttemptID != request.AttemptID {
		t.Fatalf("unexpected grant: %#v", claimed)
	}
	retried, err := f.reporter.Upgrade(ctx, f.identity, request)
	mustAgent(t, err)
	if !retried.Granted || retried.StartedAt == nil {
		t.Fatal("same attempt did not recover committed grant")
	}
	other := request
	other.AttemptID = "22222222-2222-2222-2222-222222222222"
	if _, err := f.reporter.Upgrade(ctx, f.identity, other); !errors.Is(err, ErrUpgradeConflict) {
		t.Fatalf("foreign attempt accepted: %v", err)
	}
	// A completion before a heartbeat strictly newer than StartedAt is denied.
	idle := true
	complete := UpgradeRequest{Action: "complete", Operation: request.Operation, Allocation: allocation, AttemptID: request.AttemptID, Result: "Succeeded", Version: "next", Idle: &idle}
	if _, err := f.reporter.Upgrade(ctx, f.identity, complete); !errors.Is(err, ErrUpgradeUnavailable) {
		t.Fatalf("stale completion accepted: %v", err)
	}
	f.reporter.Now = func() time.Time { return time.Unix(101, 0) }
	var node ext.ContainerNode
	mustAgent(t, f.base.Get(ctx, client.ObjectKey{Namespace: f.identity.Namespace, Name: f.identity.Name}, &node))
	// The target-version heartbeat is the updater's explicit idle proof.
	mustAgent(t, f.reporter.Report(ctx, f.identity, Observation{Version: "next", PrivateAddress: node.Spec.PrivateAddress, VisibleGPUUUIDs: node.Spec.AuthorizedGPUUUIDs, Busy: false}))
	completed, err := f.reporter.Upgrade(ctx, f.identity, complete)
	mustAgent(t, err)
	if completed.Phase != "Applying" || completed.Completion == nil || completed.Completion.HeartbeatAt.IsZero() {
		t.Fatalf("completion receipt missing: %#v", completed)
	}
	var persisted ext.AgentUpgrade
	mustAgent(t, f.base.Get(ctx, client.ObjectKeyFromObject(&op), &persisted))
	if !companion.ValidUpgradeCompletion(&persisted) {
		t.Fatalf("invalid persisted completion: %#v", persisted.Status)
	}
}

func TestUpgradeTransportRecoversLostClaimResponseByAttempt(t *testing.T) {
	f := newUpgradeTransportFixture(t)
	ctx := context.Background()
	op := readUpgradeOperation(t, f.base, client.ObjectKeyFromObject(f.op))
	allocation := ext.NativeObjectReference{Namespace: f.alloc.Namespace, Name: f.alloc.Name, UID: string(f.alloc.UID)}
	request := UpgradeRequest{Action: "claim", Operation: upgradeObjectRef(&op), Allocation: allocation, AttemptID: "33333333-3333-3333-3333-333333333333", ResourceVersion: op.ResourceVersion}
	failed := false
	f.reporter.Client = interceptor.NewClient(f.base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, name string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if name == "status" && obj.GetName() == op.Name && !failed {
			failed = true
			if err := c.SubResource(name).Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			return errors.New("committed response lost")
		}
		return c.SubResource(name).Patch(ctx, obj, patch, opts...)
	}})
	if _, err := f.reporter.Upgrade(ctx, f.identity, request); err == nil {
		t.Fatal("lost claim response was not injected")
	}
	f.reporter.Client = f.base
	recovered, err := f.reporter.Upgrade(ctx, f.identity, request)
	mustAgent(t, err)
	if !recovered.Granted || recovered.AttemptID != request.AttemptID {
		t.Fatal("lost claim response did not recover exact attempt")
	}
}

func TestUpgradeTransportRejectsTargetChangeBeforeClaim(t *testing.T) {
	f := newUpgradeTransportFixture(t)
	ctx := context.Background()
	var cc ext.ContainerCluster
	mustAgent(t, f.base.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cc))
	cc.Spec.TargetAgentRelease = "other"
	mustAgent(t, f.base.Update(ctx, &cc))
	op := readUpgradeOperation(t, f.base, client.ObjectKeyFromObject(f.op))
	request := UpgradeRequest{Action: "claim", Operation: upgradeObjectRef(&op), Allocation: upgradeObjectRef(f.alloc), AttemptID: "44444444-4444-4444-4444-444444444444", ResourceVersion: op.ResourceVersion}
	if _, err := f.reporter.Upgrade(ctx, f.identity, request); err == nil {
		t.Fatal("target replacement received an upgrade grant")
	}
	var current ext.ContainerAllocation
	mustAgent(t, f.base.Get(ctx, client.ObjectKeyFromObject(f.alloc), &current))
	if current.Status.CleanupConfirmed {
		t.Fatal("denied target change released upgrade lock")
	}
}

func upgradeClaim(f upgradeTransportFixture) UpgradeRequest {
	return UpgradeRequest{Action: "claim", Operation: upgradeObjectRef(f.op), Allocation: upgradeObjectRef(f.alloc), AttemptID: "55555555-5555-5555-5555-555555555555", ResourceVersion: f.op.ResourceVersion}
}

func TestUpgradeClaimRejectsUnreadyObservationsAndForeignIdentity(t *testing.T) {
	for _, scenario := range []string{"busy", "stale", "future", "version", "credential", "tenant", "uid", "allocation", "resource-version"} {
		t.Run(scenario, func(t *testing.T) {
			f := newUpgradeTransportFixture(t)
			ctx := context.Background()
			req := upgradeClaim(f)
			var n ext.ContainerNode
			mustAgent(t, f.base.Get(ctx, client.ObjectKey{Namespace: f.identity.Namespace, Name: f.identity.Name}, &n))
			switch scenario {
			case "busy":
				n.Status.Busy = true
			case "stale":
				n.Status.LastHeartbeatTime = &metav1.Time{Time: time.Unix(1, 0)}
			case "future":
				n.Status.LastHeartbeatTime = &metav1.Time{Time: time.Unix(200, 0)}
			case "version":
				n.Status.AgentVersion = "unexpected"
			case "credential":
				f.identity.CredentialID = "other"
			case "tenant":
				f.identity.TenantID = "other"
			case "uid":
				f.identity.UID = "other"
			case "allocation":
				req.Allocation.UID = "other"
			case "resource-version":
				req.ResourceVersion = "stale"
			}
			mustAgent(t, f.base.Status().Update(ctx, &n))
			if _, err := f.reporter.Upgrade(ctx, f.identity, req); err == nil {
				t.Fatal("invalid claim accepted")
			}
			op := readUpgradeOperation(t, f.base, client.ObjectKeyFromObject(f.op))
			if op.Status.Phase != "Reserved" {
				t.Fatal("denial changed installation phase")
			}
		})
	}
}

func TestUpgradeCompletionRecoveryAfterTargetChangeAndDeletion(t *testing.T) {
	for _, result := range []string{"Succeeded", "RolledBack"} {
		t.Run(result, func(t *testing.T) {
			f := newUpgradeTransportFixture(t)
			ctx := context.Background()
			claim := upgradeClaim(f)
			_, err := f.reporter.Upgrade(ctx, f.identity, claim)
			mustAgent(t, err)
			version := "next"
			if result == "RolledBack" {
				version = "old"
			}
			f.reporter.Now = func() time.Time { return time.Unix(101, 0) }
			var n ext.ContainerNode
			mustAgent(t, f.base.Get(ctx, client.ObjectKey{Namespace: f.identity.Namespace, Name: f.identity.Name}, &n))
			mustAgent(t, f.reporter.Report(ctx, f.identity, Observation{Version: version, PrivateAddress: n.Spec.PrivateAddress, VisibleGPUUUIDs: n.Spec.AuthorizedGPUUUIDs}))
			var cc ext.ContainerCluster
			mustAgent(t, f.base.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cc))
			cc.Spec.TargetAgentRelease = "removed"
			mustAgent(t, f.base.Update(ctx, &cc))
			op := readUpgradeOperation(t, f.base, client.ObjectKeyFromObject(f.op))
			mustAgent(t, f.base.Delete(ctx, &op))
			idle := true
			complete := UpgradeRequest{Action: "complete", Operation: claim.Operation, Allocation: claim.Allocation, AttemptID: claim.AttemptID, Result: result, Version: version, Idle: &idle}
			failed := false
			f.reporter.Client = interceptor.NewClient(f.base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, name string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if name == "status" && obj.GetName() == op.Name && !failed {
					failed = true
					if err := c.SubResource(name).Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					return errors.New("completion response lost")
				}
				return c.SubResource(name).Patch(ctx, obj, patch, opts...)
			}})
			if _, err := f.reporter.Upgrade(ctx, f.identity, complete); err == nil {
				t.Fatal("lost response not injected")
			}
			f.reporter.Client = f.base
			recovered, err := f.reporter.Upgrade(ctx, f.identity, complete)
			mustAgent(t, err)
			if recovered.Completion == nil || recovered.Phase != "Applying" {
				t.Fatal("completion receipt did not recover")
			}
			complete.Result = "Succeeded"
			complete.Version = "conflicting"
			if _, err := f.reporter.Upgrade(ctx, f.identity, complete); err == nil {
				t.Fatal("conflicting completion accepted")
			}
			// Rotation revokes even idempotent completion reads.
			mustAgent(t, f.base.Get(ctx, client.ObjectKeyFromObject(&n), &n))
			n.Annotations[CredentialAnnotation] = "rotated"
			mustAgent(t, f.base.Update(ctx, &n))
			complete.Result = result
			complete.Version = version
			if _, err := f.reporter.Upgrade(ctx, f.identity, complete); err == nil {
				t.Fatal("revoked identity accepted")
			}
		})
	}
}

type upgradeStoreHook struct {
	store  artifact.Store
	before func()
}

func (s upgradeStoreHook) Load(ctx context.Context, spec ext.AgentReleaseSpec) ([]byte, error) {
	s.before()
	return s.store.Load(ctx, spec)
}

func TestUpgradeClaimRevalidatesAfterArtifactIO(t *testing.T) {
	for _, scenario := range []string{"cancel", "target", "credential", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			f := newUpgradeTransportFixture(t)
			ctx := context.Background()
			req := upgradeClaim(f)
			f.reporter.ReleaseStore = upgradeStoreHook{store: f.reporter.ReleaseStore, before: func() {
				switch scenario {
				case "cancel":
					op := readUpgradeOperation(t, f.base, client.ObjectKeyFromObject(f.op))
					op.Status.Phase = "Cancelled"
					mustAgent(t, f.base.Status().Update(ctx, &op))
				case "target":
					var cc ext.ContainerCluster
					mustAgent(t, f.base.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cc))
					cc.Spec.TargetAgentRelease = "other"
					mustAgent(t, f.base.Update(ctx, &cc))
				case "credential":
					var n ext.ContainerNode
					mustAgent(t, f.base.Get(ctx, client.ObjectKey{Namespace: f.identity.Namespace, Name: f.identity.Name}, &n))
					n.Annotations[CredentialAnnotation] = "rotated"
					mustAgent(t, f.base.Update(ctx, &n))
				}
			}}
			if scenario == "corrupt" {
				f.reporter.ReleaseStore = failingUpgradeStore{}
			}
			if _, err := f.reporter.Upgrade(ctx, f.identity, req); err == nil {
				t.Fatal("changed authorization/artifact granted installation")
			}
			var a ext.ContainerAllocation
			mustAgent(t, f.base.Get(ctx, client.ObjectKeyFromObject(f.alloc), &a))
			if a.Status.CleanupConfirmed {
				t.Fatal("denied claim released reservation")
			}
		})
	}
}

type failingUpgradeStore struct{}

func (failingUpgradeStore) Load(context.Context, ext.AgentReleaseSpec) ([]byte, error) {
	return nil, artifact.ErrDigest
}

func TestUpgradeJournalAcknowledgementIsScopedAndRecoversLostResponse(t *testing.T) {
	f := newUpgradeTransportFixture(t)
	ctx := context.Background()
	op := readUpgradeOperation(t, f.base, client.ObjectKeyFromObject(f.op))
	claim := UpgradeRequest{Action: "claim", Operation: upgradeObjectRef(&op), Allocation: upgradeObjectRef(f.alloc), AttemptID: "11111111-1111-1111-1111-111111111111", ResourceVersion: op.ResourceVersion}
	_, err := f.reporter.Upgrade(ctx, f.identity, claim)
	mustAgent(t, err)
	idle := true
	ack := UpgradeRequest{Action: "acknowledge", Operation: claim.Operation, Allocation: claim.Allocation, AttemptID: claim.AttemptID, Result: "Succeeded", Version: "next", Idle: &idle}
	if _, err := f.reporter.Upgrade(ctx, f.identity, ack); !errors.Is(err, ErrUpgradeConflict) {
		t.Fatalf("acknowledged incomplete operation: %v", err)
	}
	f.reporter.Now = func() time.Time { return time.Unix(101, 0) }
	var node ext.ContainerNode
	mustAgent(t, f.base.Get(ctx, client.ObjectKey{Namespace: f.identity.Namespace, Name: f.identity.Name}, &node))
	mustAgent(t, f.reporter.Report(ctx, f.identity, Observation{Version: "next", PrivateAddress: node.Spec.PrivateAddress, VisibleGPUUUIDs: node.Spec.AuthorizedGPUUUIDs}))
	complete := ack
	complete.Action = "complete"
	_, err = f.reporter.Upgrade(ctx, f.identity, complete)
	mustAgent(t, err)
	for _, change := range []func(*UpgradeRequest){func(r *UpgradeRequest) { r.AttemptID = "22222222-2222-2222-2222-222222222222" }, func(r *UpgradeRequest) { r.Allocation.UID = "replacement" }, func(r *UpgradeRequest) { r.Operation.UID = "replacement" }, func(r *UpgradeRequest) { r.Version = "other" }, func(r *UpgradeRequest) { r.Result = "RolledBack" }} {
		wrong := ack
		change(&wrong)
		if _, err := f.reporter.Upgrade(ctx, f.identity, wrong); err == nil {
			t.Fatal("foreign/mismatched acknowledgement accepted")
		}
	}
	foreign := f.identity
	foreign.CredentialID = "revoked"
	if _, err := f.reporter.Upgrade(ctx, foreign, ack); err == nil {
		t.Fatal("revoked credential acknowledged")
	}
	lost := false
	f.reporter.Client = interceptor.NewClient(f.base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, name string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if err := c.SubResource(name).Patch(ctx, o, p, opts...); err != nil {
			return err
		}
		if !lost {
			lost = true
			return errors.New("committed acknowledgement response lost")
		}
		return nil
	}})
	if _, err := f.reporter.Upgrade(ctx, f.identity, ack); err == nil || !lost {
		t.Fatal("lost acknowledgement not injected")
	}
	f.reporter.Client = f.base
	result, err := f.reporter.Upgrade(ctx, f.identity, ack)
	mustAgent(t, err)
	if result.AcknowledgedAttemptID != claim.AttemptID {
		t.Fatal("same attempt did not recover acknowledgement")
	}
}
