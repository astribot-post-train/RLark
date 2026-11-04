package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	coreprovision "github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/provision"
	"github.com/google/uuid"
	tenant "github.com/rlinf/rlark/api/metadata"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type testBundle struct {
	nodes    map[string]*ext.ContainerNode
	calls    int
	gateways map[string]string
}

type cancellingBundle struct {
	bundle     *testBundle
	management client.Client
	operation  client.ObjectKey
}

func (b cancellingBundle) Bundle(ctx context.Context, node *ext.ContainerNode, release ext.AgentReleaseSpec, gateway string) ([]byte, string, error) {
	data, path, err := b.bundle.Bundle(ctx, node, release, gateway)
	if err != nil {
		return nil, "", err
	}
	var op ext.NodeEnrollment
	if err := b.management.Get(ctx, b.operation, &op); err != nil {
		return nil, "", err
	}
	op.Spec.CancelRequested = true
	if err := b.management.Update(ctx, &op); err != nil {
		return nil, "", err
	}
	return data, path, nil
}

func (b *testBundle) Bundle(_ context.Context, n *ext.ContainerNode, _ ext.AgentReleaseSpec, gatewayAddress string) ([]byte, string, error) {
	b.nodes[n.Name] = n.DeepCopy()
	if b.gateways == nil {
		b.gateways = map[string]string{}
	}
	b.gateways[n.Name] = gatewayAddress
	b.calls++
	return []byte("test scoped bundle"), "/opt/test", nil
}

type testRunner struct {
	bundle       *testBundle
	fail         bool
	wrongReceipt bool
	instances    map[string]string
	addresses    map[string]string
	installs     int
}

func (r *testRunner) Run(_ context.Context, t Target, _ string, input io.Reader) ([]byte, error) {
	if r.fail {
		return nil, errors.New("unavailable")
	}
	if input == nil {
		return json.Marshal(map[string]string{"instanceID": r.instances[t.Node.Name], "privateAddress": r.addresses[t.Node.Name]})
	}
	r.installs++
	n := r.bundle.nodes[t.Node.Name]
	services := []string{"agent", "task-manager"}
	if n.Spec.Role == ext.NodeRoleGateway {
		services = append(services, "gateway")
	}
	if r.wrongReceipt {
		services = []string{"agent"}
	}
	return json.Marshal(map[string]any{"identity": string(n.UID) + "/" + n.Spec.InstanceID + "/" + strings.Repeat("a", 64), "services": services})
}
func enrollmentFixture(t *testing.T) (Reconciler, ctrl.Request, *testRunner) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{ext.AddToScheme, native.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ext.NodeEnrollment{}, &ext.AgentRelease{}, &ext.ContainerNode{}, &native.Node{}, &native.Cluster{}).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if o.GetUID() == "" {
			o.SetUID(types.UID(uuid.NewString()))
			o.SetGeneration(1)
		}
		return c.Create(ctx, o, opts...)
	}})
	cc, err := (coreprovision.Service{Client: c}).CreateCluster(context.Background(), coreprovision.ClusterRequest{OperationID: uuid.NewString(), Name: "cloud", TenantID: "team-a", AgentNamespace: "cloud-agents", Runtime: "Raw"})
	if err != nil {
		t.Fatal(err)
	}
	release := &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: cc.Namespace, Labels: map[string]string{tenant.IDLabel: "team-a"}}, Spec: ext.AgentReleaseSpec{Version: "v1", OS: "linux", Architecture: "amd64", Protocol: "v1", ArtifactPath: "releases/v1", SHA256: strings.Repeat("a", 64)}}
	if err := c.Create(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	release.Status.Available = true
	release.Status.ObservedGeneration = release.Generation
	if err := c.Status().Update(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	op, err := (coreprovision.Service{Client: c}).CreateEnrollment(context.Background(), coreprovision.EnrollmentRequest{OperationID: uuid.NewString(), Namespace: cc.Namespace, Cluster: cc.Name, ClusterUID: string(cc.UID), TenantID: "team-a", Nodes: []ext.EnrollmentNode{{Name: "a", Role: ext.NodeRoleGateway, Address: "203.0.113.2", Port: 22}, {Name: "b", Role: ext.NodeRoleWorker, Address: "10.1.2.3", Port: 22}}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := &testBundle{nodes: map[string]*ext.ContainerNode{}}
	runner := &testRunner{bundle: bundle, instances: map[string]string{"a": strings.Repeat("a", 64), "b": strings.Repeat("b", 64)}, addresses: map[string]string{"a": "10.1.2.2", "b": "10.1.2.3"}}
	now := time.Now()
	return Reconciler{Client: c, Runner: runner, Packages: bundle, Profile: Profile{ClusterUID: string(cc.UID), Environment: "fleet", NetworkScope: "ksyun", Python: "/usr/bin/python3", AllowedGatewayCIDRs: []string{"203.0.113.0/24"}, AllowedWorkerCIDRs: []string{"10.0.0.0/8"}}, Now: func() time.Time { return now }}, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}, runner
}
func reconcileEnrollment(t *testing.T, r Reconciler, req ctrl.Request, n int) *ext.NodeEnrollment {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	op := &ext.NodeEnrollment{}
	if err := r.Client.Get(context.Background(), req.NamespacedName, op); err != nil {
		t.Fatal(err)
	}
	return op
}
func readyEnrollment(t *testing.T, r Reconciler) {
	t.Helper()
	nodes := &ext.ContainerNodeList{}
	if err := r.Client.List(context.Background(), nodes); err != nil {
		t.Fatal(err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		n.Status.AgentVersion = "v1"
		n.Status.ObservedGeneration = n.Generation
		n.Status.AgentCredentialID = n.Annotations[ext.AgentCredentialAnnotation]
		n.Status.LastHeartbeatTime = &metav1.Time{Time: r.now()}
		meta.SetStatusCondition(&n.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Observed", ObservedGeneration: n.Generation})
		if err := r.Client.Status().Update(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
}
func TestEnrollmentWaitsForHeartbeatAndNeverReinstallsCompletedOperation(t *testing.T) {
	r, req, runner := enrollmentFixture(t)
	op := reconcileEnrollment(t, r, req, 10)
	if op.Status.Phase != "Waiting" || runner.installs != 2 {
		t.Fatalf("want two starts awaiting heartbeat: %#v installs=%d", op.Status, runner.installs)
	}
	readyEnrollment(t, r)
	op = reconcileEnrollment(t, r, req, 2)
	if op.Status.Phase != "Succeeded" {
		t.Fatalf("want success: %#v", op.Status)
	}
	later := r.now().Add(time.Minute)
	r.Now = func() time.Time { return later }
	runner.fail = true
	op = reconcileEnrollment(t, r, req, 2)
	if op.Status.Phase != "Succeeded" || runner.installs != 2 || op.Status.Nodes[0].Reachability != "Unreachable" {
		t.Fatalf("completed operation restarted or concealed outage: %#v", op.Status)
	}
}
func TestEnrollmentUsesObservedGatewayInsteadOfSSHAddress(t *testing.T) {
	r, req, runner := enrollmentFixture(t)
	runner.addresses["a"] = "10.1.3.214"
	runner.addresses["b"] = "10.1.2.82"
	reconcileEnrollment(t, r, req, 10)
	for _, name := range []string{"a", "b"} {
		if runner.bundle.gateways[name] != "10.1.3.214" {
			t.Fatalf("%s package did not use authenticated gateway discovery: %v", name, runner.bundle.gateways)
		}
	}
}

func TestEnrollmentWaitsForCurrentReadyCondition(t *testing.T) {
	r, req, _ := enrollmentFixture(t)
	reconcileEnrollment(t, r, req, 10)
	readyEnrollment(t, r)
	var node ext.ContainerNode
	if err := r.Client.Get(context.Background(), client.ObjectKey{Namespace: req.Namespace, Name: "b"}, &node); err != nil {
		t.Fatal(err)
	}
	meta.FindStatusCondition(node.Status.Conditions, "Ready").ObservedGeneration = node.Generation - 1
	if err := r.Client.Status().Update(context.Background(), &node); err != nil {
		t.Fatal(err)
	}
	if op := reconcileEnrollment(t, r, req, 1); op.Status.Phase != "Waiting" {
		t.Fatalf("stale Ready condition completed enrollment: %#v", op.Status)
	}
	readyEnrollment(t, r)
	if op := reconcileEnrollment(t, r, req, 1); op.Status.Phase != "Succeeded" {
		t.Fatalf("fresh Ready condition failed to recover: %#v", op.Status)
	}
}

func TestEnrollmentSSHFailureCannotProceedUntilSuccessfulReprobe(t *testing.T) {
	r, req, runner := enrollmentFixture(t)
	runner.fail = true
	op := reconcileEnrollment(t, r, req, 2)
	if op.Status.Phase != "Waiting" || runner.bundle.calls != 0 {
		t.Fatal("SSH outage installed services")
	}
	runner.fail = false
	reconcileEnrollment(t, r, req, 2)
	if runner.bundle.calls != 0 {
		t.Fatal("used failed probe")
	}
	later := r.now().Add(time.Minute)
	r.Now = func() time.Time { return later }
	reconcileEnrollment(t, r, req, 12)
	if runner.installs != 2 {
		t.Fatal("did not recover")
	}
}

func TestEnrollmentCancellationRetainsJoinedNodesAndStopsEffects(t *testing.T) {
	r, req, runner := enrollmentFixture(t)
	op := reconcileEnrollment(t, r, req, 10)
	installs := runner.installs
	var before ext.ContainerNodeList
	if err := r.Client.List(context.Background(), &before); err != nil {
		t.Fatal(err)
	}
	op.Spec.CancelRequested = true
	if err := r.Client.Update(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	op = reconcileEnrollment(t, r, req, 3)
	if op.Status.Phase != "Cancelled" || runner.installs != installs {
		t.Fatal("cancelled operation performed installation")
	}
	for _, old := range before.Items {
		var current ext.ContainerNode
		if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(&old), &current); err != nil || current.UID != old.UID {
			t.Fatal("cancellation changed a joined node", err)
		}
	}
}

func TestCancellationDuringPackageBuildFencesInstall(t *testing.T) {
	r, req, runner := enrollmentFixture(t)
	r.Packages = cancellingBundle{bundle: runner.bundle, management: r.Client, operation: req.NamespacedName}
	op := reconcileEnrollment(t, r, req, 10)
	if runner.installs != 0 || op.Status.Phase != "Cancelled" {
		t.Fatalf("cancelled package reached SSH: %d, %#v", runner.installs, op.Status)
	}
}
func TestEnrollmentDetectsNotebookAndPrivateAddressReplacement(t *testing.T) {
	for _, address := range []bool{false, true} {
		r, req, runner := enrollmentFixture(t)
		reconcileEnrollment(t, r, req, 10)
		later := r.now().Add(time.Minute)
		r.Now = func() time.Time { return later }
		want := "NotebookReplaced"
		if address {
			runner.addresses["b"] = "10.1.2.4"
			want = "PrivateAddressChanged"
		} else {
			runner.instances["b"] = strings.Repeat("c", 64)
		}
		op := reconcileEnrollment(t, r, req, 1)
		cond := meta.FindStatusCondition(op.Status.Conditions, "Completed")
		if op.Status.Phase != "Failed" || cond.Reason != want {
			t.Fatalf("replacement accepted: %#v", op.Status)
		}
	}
}
func TestEnrollmentValidatesServiceReceipt(t *testing.T) {
	r, req, runner := enrollmentFixture(t)
	runner.wrongReceipt = true
	op := reconcileEnrollment(t, r, req, 5)
	if op.Status.Phase != "Waiting" || meta.FindStatusCondition(op.Status.Conditions, "Completed").Reason != "ServiceReceiptInvalid" {
		t.Fatalf("bad receipt accepted: %#v", op.Status)
	}
}
func TestEnrollmentRejectsChangedClusterReleaseAndTenant(t *testing.T) {
	for _, which := range []string{"cluster", "release", "tenant", "node"} {
		t.Run(which, func(t *testing.T) {
			r, req, _ := enrollmentFixture(t)
			reconcileEnrollment(t, r, req, 10)
			var op ext.NodeEnrollment
			if err := r.Client.Get(context.Background(), req.NamespacedName, &op); err != nil {
				t.Fatal(err)
			}
			switch which {
			case "tenant":
				op.Labels[tenant.IDLabel] = "foreign"
				if err := r.Client.Update(context.Background(), &op); err != nil {
					t.Fatal(err)
				}
			case "cluster":
				var cc ext.ContainerCluster
				r.Client.Get(context.Background(), client.ObjectKey{Namespace: op.Namespace, Name: "cloud"}, &cc)
				cc.UID = "replacement"
				r.Client.Update(context.Background(), &cc)
			case "release":
				var release ext.AgentRelease
				r.Client.Get(context.Background(), client.ObjectKey{Namespace: op.Namespace, Name: "v1"}, &release)
				release.UID = "replacement"
				r.Client.Update(context.Background(), &release)
			case "node":
				var n ext.ContainerNode
				r.Client.Get(context.Background(), client.ObjectKey{Namespace: op.Namespace, Name: "b"}, &n)
				n.UID = "replacement"
				r.Client.Update(context.Background(), &n)
			}
			op2 := reconcileEnrollment(t, r, req, 1)
			if op2.Status.Phase != "Failed" {
				t.Fatalf("identity conflict accepted: %#v", op2.Status)
			}
		})
	}
}
func TestEnrollmentSerializesOperationsForSameCluster(t *testing.T) {
	r, req, runner := enrollmentFixture(t)
	var first ext.NodeEnrollment
	if err := r.Client.Get(context.Background(), req.NamespacedName, &first); err != nil {
		t.Fatal(err)
	}
	other := first.DeepCopy()
	other.Name = "enroll-" + uuid.NewString()
	other.Spec.OperationID = strings.TrimPrefix(other.Name, "enroll-")
	other.UID = ""
	other.ResourceVersion = ""
	other.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Hour))
	if err := r.Client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	op := reconcileEnrollment(t, r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(other)}, 1)
	if op.Status.Phase != "Waiting" || meta.FindStatusCondition(op.Status.Conditions, "Completed").Reason != "EarlierOperationPending" || runner.installs != 0 {
		t.Fatal("concurrent operation executed")
	}
}
