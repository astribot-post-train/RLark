//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	ray "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/workload/ray/docker"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type groupEngine struct {
	dockerruntime.EngineClient
	id                       string
	current                  *types.ContainerJSON
	creates, starts, removes int
}

func (e *groupEngine) ServerVersion(context.Context) (types.Version, error) {
	return types.Version{Version: "26"}, nil
}

//nolint:staticcheck // EngineClient currently follows the Docker SDK compatibility interface.
func (e *groupEngine) Info(context.Context) (types.Info, error) { return types.Info{ID: e.id}, nil }
func (e *groupEngine) ContainerInspect(_ context.Context, _ string) (types.ContainerJSON, error) {
	if e.current == nil {
		return types.ContainerJSON{}, errdefs.NotFound(errors.New("absent"))
	}
	return *e.current, nil
}
func (e *groupEngine) ImageInspectWithRaw(context.Context, string) (types.ImageInspect, []byte, error) {
	return types.ImageInspect{ID: "image"}, nil, nil
}
func (e *groupEngine) ContainerCreate(_ context.Context, c *container.Config, h *container.HostConfig, _ *network.NetworkingConfig, _ *specs.Platform, name string) (container.CreateResponse, error) {
	e.creates++
	e.current = &types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{ID: e.id + "-container", Name: "/" + name, State: &types.ContainerState{Status: "created"}, HostConfig: h}, Config: c}
	return container.CreateResponse{ID: e.current.ID}, nil
}

//nolint:staticcheck // EngineClient currently follows the Docker SDK compatibility interface.
func (e *groupEngine) ContainerStart(context.Context, string, types.ContainerStartOptions) error {
	e.starts++
	e.current.State.Status = "running"
	e.current.State.Running = true
	return nil
}
func (e *groupEngine) ContainerStop(context.Context, string, container.StopOptions) error {
	if e.current != nil {
		e.current.State.Status = "exited"
		e.current.State.Running = false
	}
	return nil
}
func (e *groupEngine) ContainerKill(context.Context, string, string) error { return nil }

//nolint:staticcheck // EngineClient currently follows the Docker SDK compatibility interface.
func (e *groupEngine) ContainerRemove(context.Context, string, types.ContainerRemoveOptions) error {
	e.removes++
	e.current = nil
	return nil
}

//nolint:staticcheck // EngineClient currently follows the Docker SDK compatibility interface.
func (e *groupEngine) ContainerList(context.Context, types.ContainerListOptions) ([]types.Container, error) {
	if e.current == nil {
		return nil, nil
	}
	return []types.Container{{ID: e.current.ID}}, nil
}

type rayFixtureState struct {
	s       TaskService
	r       controller.JobReservationReconciler
	req     ctrl.Request
	ids     []Identity
	agents  []DockerExecutionObserver
	engines []*groupEngine
}

func rayFixture(t *testing.T, nodeCount int) *rayFixtureState {
	t.Helper()
	s, id, _ := dockerTaskFixture(t)
	ctx := context.Background()
	var cluster native.Cluster
	var cc ext.ContainerCluster
	var b ext.ContainerJob
	var nn native.Node
	var n ext.ContainerNode
	var j native.Job
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Name: "cloud"}, &cluster))
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cc))
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, &b))
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn))
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &n))
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Name: "job"}, &j))
	j.Spec.Tasks = nil
	j.Status = native.JobStatus{}
	j.ResourceVersion = ""
	j.Finalizers = nil
	for _, head := range []bool{true, false} {
		name, params := "worker", `{"role":"worker"}`
		if head {
			name, params = "head", `{"role":"head","run":{"inline":"echo train"}}`
		}
		j.Spec.Tasks = append(j.Spec.Tasks, native.JobTaskTemplate{Name: name, Head: head, TaskSpec: native.TaskSpec{Runtime: native.TaskRuntimeSpec{Kind: "Docker"}, Placement: native.TaskPlacementSpec{Environment: "fleet"}, Workload: native.TaskWorkloadSpec{Kind: "rlinf.io/ray", Version: "v1", Parameters: apiextensionsv1.JSON{Raw: []byte(params)}, Carrier: native.TaskWorkloadCarrier{Type: dockerruntime.CarrierType, Version: "v1"}, Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Name: "main", Image: "image"}}}}}}})
	}
	b.ResourceVersion = ""
	b.Status = ext.ContainerJobStatus{}
	cluster.ResourceVersion = ""
	cc.ResourceVersion = ""
	objects := []client.Object{&cluster, &cc, &b, &j}
	f := &rayFixtureState{}
	for i := 0; i < nodeCount; i++ {
		node, record := nn.DeepCopy(), n.DeepCopy()
		name := fmt.Sprintf("worker-%d", i)
		node.Name = name
		node.UID = ktypes.UID("native-" + name)
		node.ResourceVersion = ""
		record.Name = name
		record.UID = ktypes.UID("record-" + name)
		record.ResourceVersion = ""
		record.Spec.Node = ext.NativeObjectReference{Namespace: "agents", Name: name, UID: string(node.UID)}
		record.Spec.InstanceID = "engine-" + name
		record.Spec.PrivateAddress = fmt.Sprintf("10.0.3.%d", 100+i)
		record.Spec.AuthorizedGPUUUIDs = []string{fmt.Sprintf("GPU-%d", i)}
		record.Annotations[CredentialAnnotation] = "cred-" + name
		record.Status.AgentCredentialID = record.Annotations[CredentialAnnotation]
		record.Status.Capabilities = append(record.Status.Capabilities, ray.Capability)
		record.Status.Capacity = map[string]string{"cpu": "20", "memory": "236Gi"}
		objects = append(objects, node, record)
		ident := id
		ident.Name = name
		ident.UID = string(record.UID)
		ident.InstanceID = record.Spec.InstanceID
		ident.CredentialID = record.Status.AgentCredentialID
		f.ids = append(f.ids, ident)
	}
	seq := 0
	c := fake.NewClientBuilder().WithScheme(s.Reporter.Client.Scheme()).WithStatusSubresource(&native.Job{}, &native.Task{}, &ext.ContainerNode{}, &ext.ContainerJob{}, &ext.ContainerAllocation{}).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		seq++
		o.SetUID(ktypes.UID(fmt.Sprintf("%s-%d", o.GetName(), seq)))
		o.SetGeneration(1)
		return c.Create(ctx, o, opts...)
	}}).Build()
	s.Reporter.Client = c
	s.Reporter.Now = nil
	f.s = s
	f.r = controller.JobReservationReconciler{Client: c}
	f.req = ctrl.Request{NamespacedName: client.ObjectKey{Name: "job"}}
	for i, ident := range f.ids {
		path := t.TempDir()
		must(t, os.Chmod(path, 0700))
		journal, err := dockerruntime.OpenJournal(path)
		must(t, err)
		t.Cleanup(func() { must(t, journal.Close()) })
		engine := &groupEngine{id: ident.InstanceID}
		f.engines = append(f.engines, engine)
		f.agents = append(f.agents, DockerExecutionObserver{Client: transport(t, s, ident), Engine: engine, Journal: journal, EngineID: ident.InstanceID, GPUUUIDs: []string{fmt.Sprintf("GPU-%d", i)}})
	}
	return f
}
func (f *rayFixtureState) reconcile(t *testing.T, n int) {
	t.Helper()
	for range n {
		_, err := f.r.Reconcile(context.Background(), f.req)
		must(t, err)
	}
}
func (f *rayFixtureState) group(t *testing.T) *ext.JobGroupStatus {
	t.Helper()
	var b ext.ContainerJob
	must(t, f.r.Client.Get(context.Background(), client.ObjectKey{Namespace: "agents", Name: "job"}, &b))
	return b.Status.Group
}
func (f *rayFixtureState) allocations(t *testing.T) []ext.ContainerAllocation {
	t.Helper()
	var list ext.ContainerAllocationList
	must(t, f.r.Client.List(context.Background(), &list))
	return list.Items
}

func TestRayGroupMTLSPrepareBarrierAndTerminalCleanup(t *testing.T) {
	f := rayFixture(t, 2)
	ctx := context.Background()
	f.reconcile(t, 6)
	if f.group(t) == nil || f.group(t).Phase != "Preparing" {
		t.Fatalf("group=%+v", f.group(t))
	}
	must(t, f.agents[0].reconcile(ctx, true))
	f.reconcile(t, 1)
	if f.engines[0].creates != 1 || f.engines[0].starts != 0 || f.group(t).Phase != "Preparing" {
		t.Fatal("partial prepare started group")
	}
	// A duplicate/lost receipt retry recovers the same stopped instance.
	must(t, f.agents[0].reconcile(ctx, true))
	if f.engines[0].creates != 1 {
		t.Fatal("prepared retry duplicated container")
	}
	must(t, f.agents[1].reconcile(ctx, true))
	f.reconcile(t, 1)
	if f.group(t).Phase != "Starting" {
		t.Fatalf("group=%+v", f.group(t))
	}
	for _, o := range f.agents {
		must(t, o.reconcile(ctx, true))
	}
	f.reconcile(t, 1)
	for _, e := range f.engines {
		if e.starts != 1 || e.creates != 1 {
			t.Fatal("start not exact")
		}
	}
	// Determine head by placement, independent of candidate matching order.
	var head int
	for i, id := range f.ids {
		a, err := f.s.Handle(ctx, id, TaskRequest{Action: "assignment"})
		must(t, err)
		if a.Allocation.Spec.Group.MemberIndex == 0 {
			head = i
		}
	}
	f.engines[head].current.State.Status = "exited"
	f.engines[head].current.State.Running = false
	f.engines[head].current.State.ExitCode = 0
	must(t, f.agents[head].reconcile(ctx, true))
	f.reconcile(t, 1)
	if f.group(t).Phase != "Stopping" || f.group(t).Result != "Succeeded" {
		t.Fatalf("head outcome=%+v", f.group(t))
	}
	must(t, f.agents[head].reconcile(ctx, true))
	f.reconcile(t, 1)
	if len(f.allocations(t)) != 2 {
		t.Fatal("released capacity before every member cleaned")
	}
	for _, o := range f.agents {
		must(t, o.reconcile(ctx, true))
	}
	f.reconcile(t, 3)
	if len(f.allocations(t)) != 0 || f.group(t).Phase != "Succeeded" {
		t.Fatalf("cleanup group=%+v allocations=%d", f.group(t), len(f.allocations(t)))
	}
	f.reconcile(t, 3)
	for _, e := range f.engines {
		if e.creates != 1 || e.starts != 1 {
			t.Fatal("terminal group reran")
		}
	}
}

func TestRayGroupScarceCapacityNeverReservesPartialSet(t *testing.T) {
	f := rayFixture(t, 1)
	f.reconcile(t, 8)
	if len(f.allocations(t)) != 0 || f.group(t) != nil {
		t.Fatal("scarce group took partial reservation")
	}
}

func TestRayGroupPlacementUsesOneNetworkScope(t *testing.T) {
	f := rayFixture(t, 3)
	ctx := context.Background()
	for i, scope := range []string{"isolated-a", "private-b", "private-b"} {
		var node ext.ContainerNode
		must(t, f.r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: fmt.Sprintf("worker-%d", i)}, &node))
		node.Spec.NetworkScope = scope
		must(t, f.r.Client.Update(ctx, &node))
	}
	f.reconcile(t, 8)
	g := f.group(t)
	if g == nil || g.Plan.NetworkScope != "private-b" || len(g.Plan.Members) != 2 {
		t.Fatalf("group did not choose complete same-scope placement: %#v", g)
	}
	for _, member := range g.Plan.Members {
		var node ext.ContainerNode
		must(t, f.r.Client.Get(ctx, client.ObjectKey{Namespace: member.NodeRecord.Namespace, Name: member.NodeRecord.Name}, &node))
		if node.Spec.NetworkScope != g.Plan.NetworkScope {
			t.Fatalf("member crossed network scope: %q != %q", node.Spec.NetworkScope, g.Plan.NetworkScope)
		}
	}
}

func TestRayGroupStartRevokedByPeerCredentialAndStaleGeneration(t *testing.T) {
	for _, mode := range []string{"credential", "generation", "digest", "receipt"} {
		t.Run(mode, func(t *testing.T) {
			f := rayFixture(t, 2)
			ctx := context.Background()
			f.reconcile(t, 6)
			for _, o := range f.agents {
				must(t, o.reconcile(ctx, true))
			}
			f.reconcile(t, 1)
			a, err := f.s.Handle(ctx, f.ids[0], TaskRequest{Action: "assignment"})
			must(t, err)
			_, err = f.s.Handle(ctx, f.ids[0], requestFor(a, "authorize-start"))
			must(t, err)
			switch mode {
			case "credential":
				var peer ext.ContainerNode
				must(t, f.r.Client.Get(ctx, client.ObjectKey{Namespace: f.ids[1].Namespace, Name: f.ids[1].Name}, &peer))
				peer.Annotations[CredentialAnnotation] = "replacement"
				must(t, f.r.Client.Update(ctx, &peer))
			case "generation":
				var j native.Job
				must(t, f.r.Client.Get(ctx, f.req.NamespacedName, &j))
				j.Generation++
				must(t, f.r.Client.Update(ctx, &j))
			case "digest":
				var b ext.ContainerJob
				must(t, f.r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, &b))
				b.Status.Group.Digest = "changed"
				must(t, f.r.Client.Status().Update(ctx, &b))
			case "receipt":
				locks := f.allocations(t)
				locks[1].Status.Prepared.ManifestDigest = ""
				must(t, f.r.Client.Status().Update(ctx, &locks[1]))
			}
			if _, err = f.s.Handle(ctx, f.ids[0], requestFor(a, "authorize-start")); err == nil {
				t.Fatal("revoked group start accepted")
			}
			for _, e := range f.engines {
				if e.starts != 0 {
					t.Fatal("revocation started container")
				}
			}
		})
	}
}

func TestRayGroupHeartbeatLossRetainsLocks(t *testing.T) {
	f := rayFixture(t, 2)
	ctx := context.Background()
	f.reconcile(t, 6)
	for _, o := range f.agents {
		must(t, o.reconcile(ctx, true))
	}
	f.reconcile(t, 1)
	for _, o := range f.agents {
		must(t, o.reconcile(ctx, true))
	}
	f.reconcile(t, 1)
	f.r.Now = func() time.Time { return time.Now().Add(time.Minute) }
	f.reconcile(t, 2)
	if f.group(t).Phase != "Stopping" || f.group(t).Reason != "MemberHeartbeatLost" || len(f.allocations(t)) != 2 {
		t.Fatal("lost heartbeat released group")
	}
}

func TestRayGroupWorkerExitAndExplicitRestart(t *testing.T) {
	f := rayFixture(t, 2)
	ctx := context.Background()
	f.reconcile(t, 6)
	for _, o := range f.agents {
		must(t, o.reconcile(ctx, true))
	}
	f.reconcile(t, 1)
	for _, o := range f.agents {
		must(t, o.reconcile(ctx, true))
	}
	f.reconcile(t, 1)
	var worker int
	for i, id := range f.ids {
		a, err := f.s.Handle(ctx, id, TaskRequest{Action: "assignment"})
		must(t, err)
		if a.Allocation.Spec.Group.MemberIndex != 0 {
			worker = i
		}
	}
	f.engines[worker].current.State.Status = "exited"
	f.engines[worker].current.State.ExitCode = 0
	must(t, f.agents[worker].reconcile(ctx, true))
	f.reconcile(t, 1)
	if f.group(t).Result != "Failed" {
		t.Fatal("clean worker exit succeeded")
	}
	for _, o := range f.agents {
		must(t, o.reconcile(ctx, true))
	}
	f.reconcile(t, 3)
	oldID := f.group(t).ID
	var job native.Job
	must(t, f.r.Client.Get(ctx, f.req.NamespacedName, &job))
	job.Annotations["rlark.io/restarted-at"] = "new-execution"
	job.Generation++
	must(t, f.r.Client.Update(ctx, &job))
	f.reconcile(t, 12)
	if f.group(t) == nil || f.group(t).ID == oldID || f.group(t).Phase != "Preparing" {
		t.Fatalf("restart=%+v", f.group(t))
	}
}

func TestRayGroupControllerRestartsAndRecoversLostAllocationResponse(t *testing.T) {
	f := rayFixture(t, 2)
	ctx := context.Background()
	base := f.r.Client
	lost := false
	f.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, o, opts...); err != nil {
			return err
		}
		if _, ok := o.(*ext.ContainerAllocation); ok && !lost {
			lost = true
			return errors.New("allocation create response lost")
		}
		return nil
	}})
	failed := false
	for range 7 {
		if _, err := f.r.Reconcile(ctx, f.req); err != nil {
			failed = true
			break
		}
	}
	if !failed || !lost {
		t.Fatal("lost response not exercised")
	}
	var first ext.ContainerAllocationList
	must(t, base.List(ctx, &first))
	if len(first.Items) != 1 {
		t.Fatal("allocation not persisted before lost response")
	}
	firstUID := first.Items[0].UID
	f.r = controller.JobReservationReconciler{Client: base}
	f.reconcile(t, 4)
	found := false
	for _, a := range f.allocations(t) {
		found = found || a.UID == firstUID
	}
	if !found || len(f.allocations(t)) != 2 || f.group(t).Phase != "Preparing" {
		t.Fatal("restart replaced uncertain allocation")
	}
}

func TestRayGroupSuspendAfterPartialPreparationCleansWholeSet(t *testing.T) {
	f := rayFixture(t, 2)
	ctx := context.Background()
	f.reconcile(t, 6)
	must(t, f.agents[0].reconcile(ctx, true))
	var job native.Job
	must(t, f.r.Client.Get(ctx, f.req.NamespacedName, &job))
	job.Spec.Suspended = true
	job.Generation++
	must(t, f.r.Client.Update(ctx, &job))
	f.reconcile(t, 2)
	if f.group(t).Phase != "Stopping" {
		t.Fatal("stop did not fence entire group")
	}
	for _, o := range f.agents {
		must(t, o.reconcile(ctx, true))
	}
	f.reconcile(t, 10)
	if len(f.allocations(t)) != 0 {
		t.Fatal("suspended group retained cleaned allocation")
	}
	for _, e := range f.engines {
		if e.starts != 0 {
			t.Fatal("suspension started container")
		}
	}
}

func TestRayGroupTwoJobsCompeteWithoutPartialLocks(t *testing.T) {
	f := rayFixture(t, 2)
	ctx := context.Background()
	f.reconcile(t, 6)
	var job native.Job
	must(t, f.r.Client.Get(ctx, f.req.NamespacedName, &job))
	other := job.DeepCopy()
	other.Name = "second"
	other.ResourceVersion = ""
	other.UID = "second"
	other.Finalizers = nil
	other.Status = native.JobStatus{}
	must(t, f.r.Client.Create(ctx, other))
	var binding ext.ContainerJob
	must(t, f.r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, &binding))
	binding.Name = "second"
	binding.UID = "second-binding"
	binding.ResourceVersion = ""
	binding.Spec.Job = objectRef(other)
	binding.Status = ext.ContainerJobStatus{}
	must(t, f.r.Client.Create(ctx, &binding))
	for range 8 {
		_, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "second"}})
		must(t, err)
	}
	if len(f.allocations(t)) != 2 {
		t.Fatal("competing group took additional locks")
	}
	var second native.Job
	must(t, f.r.Client.Get(ctx, client.ObjectKey{Name: "second"}, &second))
	if second.Status.Phase != native.JobPhasePending {
		t.Fatal("scarce group advertised running")
	}
}
