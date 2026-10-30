package provision

import (
	"context"
	"errors"
	"reflect"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func TestCreateClusterRecoveryAndConflicts(t *testing.T) {
	ctx := context.Background()
	s, req := fixture(t)
	lost := false
	base := s.Client
	s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		_, cluster := obj.(*native.Cluster)
		_, ns := obj.(*corev1.Namespace)
		_, cc := obj.(*ext.ContainerCluster)
		if !lost && (cluster || ns || cc) {
			lost = true
			return errors.New("committed response lost")
		}
		return nil
	}})
	cc, err := s.CreateCluster(ctx, req)
	must(t, err)
	if !lost {
		t.Fatal("response loss not exercised")
	}
	again, err := s.CreateCluster(ctx, req)
	must(t, err)
	if !reflect.DeepEqual(cc, again) {
		t.Fatal("retry changed identity")
	}
	for _, mutate := range []func(*ClusterRequest){func(r *ClusterRequest) { r.AgentNamespace = "other-agents" }, func(r *ClusterRequest) { r.Runtime = "Docker" }, func(r *ClusterRequest) { r.TenantID = "other" }} {
		r := req
		mutate(&r)
		if _, err := s.CreateCluster(ctx, r); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed request accepted: %v", err)
		}
	}
	foreign, req2 := fixture(t)
	must(t, foreign.Client.Create(ctx, &native.Cluster{ObjectMeta: metav1.ObjectMeta{Name: req2.Name, UID: "foreign"}, Spec: native.ClusterSpec{AgentNamespace: req2.AgentNamespace}}))
	if _, err := foreign.CreateCluster(ctx, req2); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign Cluster adopted: %v", err)
	}
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

func TestProvisionRecoveryForEveryCreateStage(t *testing.T) {
	for _, stage := range []string{"Cluster", "Namespace", "ContainerCluster", "Node", "ContainerNode"} {
		t.Run(stage, func(t *testing.T) {
			s, req := fixture(t)
			ctx := context.Background()
			base := s.Client
			failed := false
			s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if err := c.Create(ctx, o, opts...); err != nil {
					return err
				}
				kind := reflect.TypeOf(o).Elem().Name()
				if kind == stage && !failed {
					failed = true
					return errors.New("committed write response lost")
				}
				return nil
			}})
			cc, err := s.CreateCluster(ctx, req)
			must(t, err)
			nodeReq := NodeRequest{OperationID: uuid.NewString(), Cluster: cc.Spec.Cluster.Name, ClusterUID: cc.Spec.Cluster.UID, TenantID: req.TenantID, Name: "worker", InstanceID: "container-1", PrivateAddress: "10.0.3.85", Environment: "pi05", NetworkScope: "vpc", AuthorizedGPUUUIDs: []string{"GPU-a"}}
			node, err := s.RegisterNode(ctx, nodeReq)
			must(t, err)
			again, err := s.RegisterNode(ctx, nodeReq)
			must(t, err)
			if !failed || node.UID != again.UID || node.Spec.Node.UID != again.Spec.Node.UID {
				t.Fatal("lost response not recovered with exact identity")
			}
		})
	}
}

func TestProvisionDoesNotRecreateDeletedNativeInstances(t *testing.T) {
	for _, kind := range []string{"Node", "Cluster"} {
		t.Run(kind, func(t *testing.T) {
			s, cn := registered(t)
			ctx := context.Background()
			if kind == "Node" {
				var nn native.Node
				must(t, s.Client.Get(ctx, client.ObjectKey{Namespace: cn.Namespace, Name: cn.Name}, &nn))
				must(t, s.Client.Delete(ctx, &nn))
				_, err := s.RegisterNode(ctx, NodeRequest{OperationID: cn.Annotations[OperationAnnotation], Cluster: cn.Spec.Cluster.Name, ClusterUID: cn.Spec.Cluster.UID, TenantID: tenant.IDOf(cn), Name: cn.Name, InstanceID: cn.Spec.InstanceID, PrivateAddress: cn.Spec.PrivateAddress, Environment: cn.Spec.Environment, NetworkScope: cn.Spec.NetworkScope, AuthorizedGPUUUIDs: cn.Spec.AuthorizedGPUUUIDs})
				if err == nil {
					t.Fatal("replacement Node created")
				}
				if err := s.Client.Get(ctx, client.ObjectKey{Namespace: cn.Namespace, Name: cn.Name}, &native.Node{}); !apierrors.IsNotFound(err) {
					t.Fatal("deleted native Node was recreated")
				}
			} else {
				var c native.Cluster
				must(t, s.Client.Get(ctx, client.ObjectKey{Name: cn.Spec.Cluster.Name}, &c))
				op := c.Annotations[OperationAnnotation]
				must(t, s.Client.Delete(ctx, &c))
				_, err := s.CreateCluster(ctx, ClusterRequest{OperationID: op, Name: c.Name, AgentNamespace: c.Spec.AgentNamespace, TenantID: tenant.IDOf(cn), Runtime: "Raw"})
				if err == nil {
					t.Fatal("replacement Cluster created")
				}
				if err := s.Client.Get(ctx, client.ObjectKey{Name: c.Name}, &native.Cluster{}); !apierrors.IsNotFound(err) {
					t.Fatal("deleted native Cluster was recreated")
				}
			}
		})
	}
}

func TestProvisionRejectsForeignNamespaceAndDuplicateEntitlement(t *testing.T) {
	ctx := context.Background()
	s, req := fixture(t)
	must(t, s.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: req.AgentNamespace}}))
	if _, err := s.CreateCluster(ctx, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign Namespace adopted: %v", err)
	}
	s, cn := registered(t)
	for _, mode := range []string{"GPU", "IP", "operation", "incarnation", "tenant", "cluster UID"} {
		t.Run(mode, func(t *testing.T) {
			n := NodeRequest{OperationID: cn.Annotations[OperationAnnotation], Cluster: cn.Spec.Cluster.Name, ClusterUID: cn.Spec.Cluster.UID, TenantID: tenant.IDOf(cn), Name: cn.Name, InstanceID: cn.Spec.InstanceID, PrivateAddress: cn.Spec.PrivateAddress, Environment: cn.Spec.Environment, NetworkScope: cn.Spec.NetworkScope, AuthorizedGPUUUIDs: cn.Spec.AuthorizedGPUUUIDs}
			switch mode {
			case "GPU":
				n.Name = "second"
				n.OperationID = uuid.NewString()
				n.PrivateAddress = "10.0.3.86"
			case "IP":
				n.Name = "third"
				n.OperationID = uuid.NewString()
				n.AuthorizedGPUUUIDs = []string{"GPU-b"}
			case "operation":
				n.OperationID = uuid.NewString()
			case "incarnation":
				n.InstanceID = "replacement"
			case "tenant":
				n.TenantID = "other"
			case "cluster UID":
				n.ClusterUID = "old"
			}
			if _, err := s.RegisterNode(ctx, n); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflicting request accepted: %v", err)
			}
		})
	}
}

func TestProvisionRetryAfterCreateAndRecoveryReadResponsesLost(t *testing.T) {
	s, req := fixture(t)
	ctx := context.Background()
	created, lostRead := false, false
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*native.Cluster); ok && !created {
				created = true
				return errors.New("committed create response lost")
			}
			return nil
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if created && !lostRead {
				lostRead = true
				return errors.New("recovery read unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if _, err := s.CreateCluster(ctx, req); err == nil {
		t.Fatal("lost create and read responses reported success")
	}
	var retained native.Cluster
	must(t, s.Client.Get(ctx, client.ObjectKey{Name: req.Name}, &retained))
	cc, err := (Service{Client: s.Client}).CreateCluster(ctx, req)
	must(t, err)
	if cc.Spec.Cluster.UID != string(retained.UID) || !created || !lostRead {
		t.Fatal("restart did not retain committed identity")
	}
}

func TestRegistrationRetryDoesNotReplaceRotatedCredential(t *testing.T) {
	s, cn := registered(t)
	ctx := context.Background()
	cn.Annotations[agent.CredentialAnnotation] = "rotated"
	must(t, s.Client.Update(ctx, cn))
	_, err := s.RegisterNode(ctx, NodeRequest{OperationID: cn.Annotations[OperationAnnotation], Cluster: cn.Spec.Cluster.Name, ClusterUID: cn.Spec.Cluster.UID, TenantID: tenant.IDOf(cn), Name: cn.Name, InstanceID: cn.Spec.InstanceID, PrivateAddress: cn.Spec.PrivateAddress, Environment: cn.Spec.Environment, NetworkScope: cn.Spec.NetworkScope, AuthorizedGPUUUIDs: cn.Spec.AuthorizedGPUUUIDs})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("rotated credential overwritten: %v", err)
	}
	must(t, s.Client.Get(ctx, client.ObjectKeyFromObject(cn), cn))
	if cn.Annotations[agent.CredentialAnnotation] != "rotated" {
		t.Fatal("registration changed active credential")
	}
}

func TestPartialProvisioningPinsOriginalIntent(t *testing.T) {
	ctx := context.Background()
	t.Run("cluster runtime", func(t *testing.T) {
		s, req := fixture(t)
		base := s.Client
		s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Namespace); ok {
				return errors.New("namespace API unavailable")
			}
			return c.Create(ctx, obj, opts...)
		}})
		if _, err := s.CreateCluster(ctx, req); err == nil {
			t.Fatal("partial create succeeded")
		}
		s.Client = base
		changed := req
		changed.Runtime = "Docker"
		if _, err := s.CreateCluster(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("partial creation changed runtime: %v", err)
		}
		_, err := s.CreateCluster(ctx, req)
		must(t, err)
	})
	t.Run("node environment", func(t *testing.T) {
		s, req := fixture(t)
		cc, err := s.CreateCluster(ctx, req)
		must(t, err)
		nodeReq := NodeRequest{OperationID: uuid.NewString(), Cluster: cc.Spec.Cluster.Name, ClusterUID: cc.Spec.Cluster.UID, TenantID: req.TenantID, Name: "worker", InstanceID: "container-1", PrivateAddress: "10.0.3.85", Environment: "pi05", NetworkScope: "ksyun", AuthorizedGPUUUIDs: []string{"GPU-a"}}
		base := s.Client
		s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*ext.ContainerNode); ok {
				return errors.New("companion API unavailable")
			}
			return c.Create(ctx, obj, opts...)
		}})
		if _, err := s.RegisterNode(ctx, nodeReq); err == nil {
			t.Fatal("partial create succeeded")
		}
		s.Client = base
		changed := nodeReq
		changed.Environment = "replacement-environment"
		if _, err := s.RegisterNode(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("partial creation changed environment: %v", err)
		}
		_, err = s.RegisterNode(ctx, nodeReq)
		must(t, err)
	})
}

func TestPartialNamespacePreventsDeletedClusterRecreation(t *testing.T) {
	s, req := fixture(t)
	ctx := context.Background()
	base := s.Client
	s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*ext.ContainerCluster); ok {
			return errors.New("companion API unavailable")
		}
		return c.Create(ctx, obj, opts...)
	}})
	if _, err := s.CreateCluster(ctx, req); err == nil {
		t.Fatal("partial create succeeded")
	}
	s.Client = base
	var cluster native.Cluster
	must(t, s.Client.Get(ctx, client.ObjectKey{Name: req.Name}, &cluster))
	must(t, s.Client.Delete(ctx, &cluster))
	if _, err := s.CreateCluster(ctx, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("lost native instance accepted: %v", err)
	}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: req.Name}, &native.Cluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted Cluster recreated: %v", err)
	}
}

func TestReadNodeRejectsConflictingTenantAndNamespaceBindings(t *testing.T) {
	for _, field := range []string{"native tenant", "native UID", "native namespace", "companion tenant", "companion namespace"} {
		t.Run(field, func(t *testing.T) {
			s, cn := registered(t)
			ctx := context.Background()
			id := nodeID(cn)
			switch field {
			case "native tenant":
				var nn native.Node
				must(t, s.Client.Get(ctx, client.ObjectKey{Namespace: cn.Namespace, Name: cn.Name}, &nn))
				nn.Labels[tenant.IDLabel] = "other-tenant"
				must(t, s.Client.Update(ctx, &nn))
			case "native UID":
				cn.Spec.Node.UID = "replacement"
				must(t, s.Client.Update(ctx, cn))
			case "native namespace":
				cn.Spec.Node.Namespace = "another-namespace"
				must(t, s.Client.Update(ctx, cn))
			case "companion tenant", "companion namespace":
				var cc ext.ContainerCluster
				must(t, s.Client.Get(ctx, client.ObjectKey{Namespace: cn.Namespace, Name: cn.Spec.Cluster.Name}, &cc))
				if field == "companion tenant" {
					cc.Labels[tenant.IDLabel] = "other-tenant"
				} else {
					cc.Spec.AgentNamespace = "another-namespace"
				}
				must(t, s.Client.Update(ctx, &cc))
			}
			if _, err := s.ReadNode(ctx, id); err == nil {
				t.Fatal("conflicting binding accepted")
			}
		})
	}
}

func TestDockerRegistrationRecoversLostResponseAndRejectsChangedIdentity(t *testing.T) {
	s, req := fixture(t)
	req.Runtime = "Docker"
	ctx := context.Background()
	cc, err := s.CreateCluster(ctx, req)
	must(t, err)
	nr := NodeRequest{OperationID: uuid.NewString(), Cluster: cc.Spec.Cluster.Name, ClusterUID: cc.Spec.Cluster.UID, TenantID: req.TenantID, Name: "docker-worker", InstanceID: "engine-1", PrivateAddress: "10.0.3.85", Environment: "docker-gpu", NetworkScope: "cloud", AuthorizedGPUUUIDs: []string{"GPU-a"}}
	lost := false
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, o, opts...); err != nil {
			return err
		}
		if _, ok := o.(*ext.ContainerNode); ok && !lost {
			lost = true
			return errors.New("response lost")
		}
		return nil
	}})
	node, err := s.RegisterNode(ctx, nr)
	must(t, err)
	retry, err := s.RegisterNode(ctx, nr)
	must(t, err)
	if !lost || node.UID != retry.UID || retry.Spec.Runtime != "Docker" {
		t.Fatal("Docker registration changed identity on retry")
	}
	_, err = s.ReadNode(ctx, nodeID(node))
	must(t, err)
	changed := nr
	changed.InstanceID = "replacement-engine"
	if _, err := s.RegisterNode(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("Engine identity replaced: %v", err)
	}
	wrong := nodeID(node)
	wrong.TenantID = "another-team"
	if _, err := s.ReadNode(ctx, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("tenant boundary bypassed: %v", err)
	}
	var nn native.Node
	must(t, s.Client.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: node.Name}, &nn))
	if nn.Spec.AgentType != native.AgentTypeDocker {
		t.Fatal("native Node runtime is not Docker")
	}
	nn.Spec.AgentType = native.AgentTypeRaw
	must(t, s.Client.Update(ctx, &nn))
	if _, err := s.ReadNode(ctx, nodeID(node)); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched native runtime accepted: %v", err)
	}
}
