package provision

import (
	"context"
	"errors"
	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	"github.com/google/uuid"
	tenant "github.com/rlinf/rlark/api/metadata"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"testing"
)

func fixture(t *testing.T) (Service, ClusterRequest) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, native.AddToScheme, ext.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&native.Node{}, &native.Cluster{}, &ext.ContainerNode{}, &ext.ContainerCluster{}).Build()
	client := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetUID() == "" {
			obj.SetUID(types.UID(uuid.NewString()))
			obj.SetGeneration(1)
		}
		return c.Create(ctx, obj, opts...)
	}})
	return Service{Client: client}, ClusterRequest{OperationID: uuid.NewString(), Name: "cloud", TenantID: "team-a", AgentNamespace: "cloud-agents", Runtime: "Raw"}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func registered(t *testing.T) (Service, *ext.ContainerNode) {
	t.Helper()
	return registeredRuntime(t, "Raw")
}
func registeredRuntime(t *testing.T, runtime ext.Runtime) (Service, *ext.ContainerNode) {
	t.Helper()
	s, req := fixture(t)
	req.Runtime = runtime
	cc, err := s.CreateCluster(context.Background(), req)
	must(t, err)
	cn, err := s.RegisterNode(context.Background(), NodeRequest{OperationID: uuid.NewString(), Cluster: cc.Spec.Cluster.Name, ClusterUID: cc.Spec.Cluster.UID, TenantID: req.TenantID, Name: "worker", InstanceID: "container-1", PrivateAddress: "10.0.3.85", Environment: "pi05", NetworkScope: "ksyun", AuthorizedGPUUUIDs: []string{"GPU-a"}})
	must(t, err)
	return s, cn
}
func nodeID(cn *ext.ContainerNode) NodeIdentity {
	return NodeIdentity{Namespace: cn.Namespace, Name: cn.Name, UID: string(cn.UID), TenantID: tenant.IDOf(cn), CredentialID: cn.Annotations[ext.AgentCredentialAnnotation]}
}

func TestNodeRegistrationAndReadIdentity(t *testing.T) {
	s, cn := registered(t)
	ctx := context.Background()
	if cn.Status.Schedulable || len(cn.Status.Conditions) != 0 {
		t.Fatal("registration granted readiness")
	}
	req := NodeRequest{OperationID: uuid.NewString(), Cluster: cn.Spec.Cluster.Name, ClusterUID: cn.Spec.Cluster.UID, TenantID: tenant.IDOf(cn), Name: "worker-2", InstanceID: "container-2", PrivateAddress: "10.0.3.86", Environment: cn.Spec.Environment, NetworkScope: cn.Spec.NetworkScope, AuthorizedGPUUUIDs: []string{"GPU-b"}}
	if _, err := s.RegisterNode(ctx, req); err != nil {
		t.Fatal(err)
	}
	id := nodeID(cn)
	if _, err := s.ReadNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*NodeIdentity){func(i *NodeIdentity) { i.UID = "old" }, func(i *NodeIdentity) { i.TenantID = "other" }, func(i *NodeIdentity) { i.CredentialID = "old" }} {
		bad := id
		change(&bad)
		if _, err := s.ReadNode(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale identity accepted: %v", err)
		}
	}
	var node native.Node
	must(t, s.Client.Get(ctx, client.ObjectKey{Namespace: cn.Namespace, Name: cn.Name}, &node))
	node.Spec.Unschedulable = true
	must(t, s.Client.Update(ctx, &node))
	if _, err := s.RegisterNode(ctx, NodeRequest{OperationID: cn.Annotations[OperationAnnotation], Cluster: cn.Spec.Cluster.Name, ClusterUID: cn.Spec.Cluster.UID, TenantID: tenant.IDOf(cn), Name: cn.Name, InstanceID: cn.Spec.InstanceID, PrivateAddress: cn.Spec.PrivateAddress, Environment: cn.Spec.Environment, NetworkScope: cn.Spec.NetworkScope, AuthorizedGPUUUIDs: cn.Spec.AuthorizedGPUUUIDs}); err != nil {
		t.Fatal(err)
	}
	must(t, s.Client.Get(ctx, client.ObjectKeyFromObject(&node), &node))
	if !node.Spec.Unschedulable {
		t.Fatal("registration removed cordon")
	}
	_, err := (controller.ContainerClusterReconciler{Client: s.Client}).Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cn.Spec.Cluster.Name, Namespace: cn.Namespace}})
	must(t, err)
	_, err = (controller.ContainerNodeReconciler{Client: s.Client}).Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cn)})
	must(t, err)
	id = nodeID(cn)
	err = (agent.Reporter{Client: s.Client}).Report(ctx, agent.Identity{Namespace: id.Namespace, Name: id.Name, UID: id.UID, ClusterUID: cn.Spec.Cluster.UID, InstanceID: cn.Spec.InstanceID, TenantID: id.TenantID, CredentialID: id.CredentialID}, agent.Observation{Version: "preview", PrivateAddress: cn.Spec.PrivateAddress, VisibleGPUUUIDs: cn.Spec.AuthorizedGPUUUIDs})
	must(t, err)
	must(t, s.Client.Get(ctx, client.ObjectKeyFromObject(cn), cn))
	if !cn.Status.Schedulable {
		t.Fatal("authenticated node did not become schedulable")
	}
}
