package job

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	rawruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/raw"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type rawJobFixture struct {
	c      client.WithWatch
	scheme *runtime.Scheme
	job    *api.Job
	node   *api.Node
}

func newRawJobFixture(t *testing.T) *rawJobFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	job := &api.Job{ObjectMeta: metav1.ObjectMeta{Name: "training", UID: "job-uid", Generation: 1}}
	template := api.JobTaskTemplate{Name: "worker"}
	template.Runtime.Kind = "Raw"
	template.Placement.Environment = "prepared"
	template.Placement.NodeSelector = map[string]string{"pool": "gpu"}
	template.Workload.Kind = "raw.rlark.io/process"
	template.Workload.Version = "v1"
	template.Workload.Carrier = api.TaskWorkloadCarrier{Type: "raw.rlark.io/process", Version: "v1"}
	template.Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/python3","-c","print(42)"],"directory":"/work","env":["PATH=/usr/bin"]}`)
	job.Spec.Tasks = []api.JobTaskTemplate{template}
	capability := &api.RawNodeStatus{Incarnation: "container", TrainingIP: "10.1.0.1", NetworkScope: "ksyun-vpc", Environment: "prepared", AuthorizedGPUUUIDs: []string{"GPU-authorized"}}
	data, err := json.Marshal(capability)
	require.NoError(t, err)
	node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-a", Namespace: "agents", UID: "node-uid", Labels: map[string]string{"pool": "gpu"}, Annotations: map[string]string{rawnode.CapabilityAnnotation: string(data)}}, Spec: api.NodeSpec{AgentType: api.AgentTypeRaw}, Status: api.NodeStatus{Raw: capability, Phase: api.NodeOnline, LastHeartbeatTime: timePtrRaw(metav1.Now())}}
	cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cluster-uid"}, Spec: api.ClusterSpec{AgentNamespace: "agents"}}
	// Real API servers assign UIDs; the fake needs that behavior for new Tasks.
	count := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Job{}, &api.Node{}, &api.Task{}).WithObjects(job, node, cluster).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*api.Task); ok && obj.GetUID() == "" {
			count++
			obj.SetUID(types.UID("task-" + string(rune('0'+count))))
			obj.SetGeneration(1)
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	return &rawJobFixture{c, scheme, job, node}
}
func timePtrRaw(v metav1.Time) *metav1.Time { return &v }
func (f *rawJobFixture) reconcile(t *testing.T, c client.Client) {
	t.Helper()
	r := &Reconciler{Client: f.c, RawClient: c, Scheme: f.scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.job)})
	require.NoError(t, err)
}
func (f *rawJobFixture) task(t *testing.T) *api.Task {
	t.Helper()
	var list api.TaskList
	require.NoError(t, f.c.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
	return &list.Items[0]
}
func (f *rawJobFixture) bind(t *testing.T) *api.Task {
	t.Helper()
	for range 3 {
		f.reconcile(t, f.c)
	}
	task := f.task(t)
	require.NotNil(t, task.Spec.Placement.Binding)
	return task
}
func (f *rawJobFixture) reservation(t *testing.T) *api.RawNodeReservation {
	t.Helper()
	var node api.Node
	require.NoError(t, f.c.Get(context.Background(), client.ObjectKeyFromObject(f.node), &node))
	return node.Status.Reservation
}
func (f *rawJobFixture) agent(t *testing.T, s *jobSupervisor) *host.Host {
	t.Helper()
	h, err := rawruntime.NewHost(context.Background(), f.c, rawruntime.NodeAdmission{Client: f.c, Node: client.ObjectKeyFromObject(f.node), NodeUID: string(f.node.UID), Incarnation: "container", TenantID: tenant.DefaultID}, &rawruntime.Provider{ID: "raw-node", Supervisor: s}, []workload.Adapter{rawruntime.ProcessAdapter{}})
	require.NoError(t, err)
	return h
}
func runRawHost(t *testing.T, h *host.Host, task *api.Task) {
	t.Helper()
	_, err := h.Reconcile(context.Background(), client.ObjectKeyFromObject(task))
	require.NoError(t, err)
}

func TestRawJobSingleNodeExecutionRecoveryAndRelease(t *testing.T) {
	f := newRawJobFixture(t)
	task := f.bind(t)
	require.Empty(t, task.Spec.Environments)
	require.Equal(t, string(task.UID), f.reservation(t).TaskUID)
	spec, err := rawruntime.DecodeProcess(task.Spec.Workload.Parameters.Raw)
	require.NoError(t, err)
	require.Contains(t, spec.Env, "CUDA_VISIBLE_DEVICES=GPU-authorized")
	s := &jobSupervisor{}
	runRawHost(t, f.agent(t, s), task)
	runRawHost(t, f.agent(t, s), task) // Reconstruct Agent and controller each time.
	f.reconcile(t, f.c)
	require.Equal(t, 1, s.starts)
	require.Equal(t, api.JobPhaseRunning, readRawJob(t, f).Status.Phase)
	// A leader can exit while descendants survive: hold the reservation.
	s.record.Phase = "Succeeded"
	runRawHost(t, f.agent(t, s), task)
	f.reconcile(t, f.c)
	require.NotNil(t, f.reservation(t))
	require.Equal(t, api.TaskPhasePending, f.task(t).Status.Phase)
	s.record.Cleaned = true
	runRawHost(t, f.agent(t, s), task)
	f.reconcile(t, f.c)
	require.Nil(t, f.reservation(t))
	require.Equal(t, api.JobPhaseSucceeded, readRawJob(t, f).Status.Phase)
	for range 2 {
		f.reconcile(t, f.c)
	}
	require.Equal(t, task.UID, f.task(t).UID)
	require.Equal(t, 1, s.starts)
}
func readRawJob(t *testing.T, f *rawJobFixture) *api.Job {
	t.Helper()
	var job api.Job
	require.NoError(t, f.c.Get(context.Background(), client.ObjectKeyFromObject(f.job), &job))
	return &job
}

func TestRawJobDispatchPatchFailureRecoversSameReservation(t *testing.T) {
	f := newRawJobFixture(t)
	f.reconcile(t, f.c)
	f.reconcile(t, f.c)
	failing := interceptor.NewClient(f.c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		if task, ok := obj.(*api.Task); ok && task.Spec.Placement.Binding != nil {
			return errors.New("simulated uncertain dispatch response")
		}
		return c.Patch(ctx, obj, p, opts...)
	}})
	r := &Reconciler{Client: f.c, RawClient: failing, Scheme: f.scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.job)})
	require.Error(t, err)
	reserved := *f.reservation(t)
	require.NotEmpty(t, reserved.TaskUID)
	require.Nil(t, f.task(t).Spec.Placement.Binding)
	f.reconcile(t, f.c)
	require.Equal(t, reserved, *f.reservation(t))
	require.Equal(t, reserved.TaskUID, f.task(t).Spec.Placement.Binding.TaskUID)
}

func TestRawJobDeletionWaitsForSupervisorThenReleases(t *testing.T) {
	f := newRawJobFixture(t)
	task := f.bind(t)
	s := &jobSupervisor{}
	runRawHost(t, f.agent(t, s), task)
	require.NoError(t, f.c.Delete(context.Background(), readRawJob(t, f)))
	f.reconcile(t, f.c)
	for range 2 {
		f.reconcile(t, f.c)
	}
	require.NotNil(t, f.reservation(t))
	require.Contains(t, f.task(t).Finalizers, RawAllocationFinalizer)
	s.unavailable = true
	_, err := f.agent(t, s).Reconcile(context.Background(), client.ObjectKeyFromObject(task))
	require.Error(t, err)
	f.reconcile(t, f.c)
	require.NotNil(t, f.reservation(t))
	s.unavailable = false
	runRawHost(t, f.agent(t, s), task)
	for range 3 {
		f.reconcile(t, f.c)
	}
	require.Nil(t, f.reservation(t))
	var job api.Job
	require.True(t, client.IgnoreNotFound(f.c.Get(context.Background(), client.ObjectKeyFromObject(f.job), &job)) == nil)
}

func TestRawJobUnclaimedCancellationAndSuspension(t *testing.T) {
	for _, mode := range []string{"delete-before-bind", "delete-after-assignment", "suspend-before-bind", "suspend-running"} {
		t.Run(mode, func(t *testing.T) {
			f := newRawJobFixture(t)
			f.reconcile(t, f.c)
			f.reconcile(t, f.c)
			var s *jobSupervisor
			if mode == "delete-after-assignment" {
				failing := interceptor.NewClient(f.c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*api.Task); ok {
						return errors.New("dispatch unavailable")
					}
					return c.Patch(ctx, obj, p, opts...)
				}})
				r := &Reconciler{Client: f.c, RawClient: failing, Scheme: f.scheme}
				_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.job)})
				require.Error(t, err)
				require.NotNil(t, f.reservation(t))
			}
			if mode == "suspend-running" {
				f.reconcile(t, f.c)
				s = &jobSupervisor{}
				runRawHost(t, f.agent(t, s), f.task(t))
			}
			job := readRawJob(t, f)
			if strings.HasPrefix(mode, "delete") {
				require.NoError(t, f.c.Delete(context.Background(), job))
			} else {
				job.Spec.Suspended = true
				job.Generation++
				require.NoError(t, f.c.Update(context.Background(), job))
			}
			f.reconcile(t, f.c)
			if s != nil {
				require.NotNil(t, f.reservation(t))
				runRawHost(t, f.agent(t, s), f.task(t))
			}
			for range 4 {
				f.reconcile(t, f.c)
			}
			require.Nil(t, f.reservation(t))
			if s != nil {
				require.Equal(t, api.JobPhaseStopped, readRawJob(t, f).Status.Phase)
			}
		})
	}
}

func TestRawJobValidationAndUnavailableCapacity(t *testing.T) {
	for _, mode := range []string{"ray", "multiple", "binding", "environment", "gpu-env", "unknown-field", "cordoned", "stale-heartbeat", "foreign-tenant"} {
		t.Run(mode, func(t *testing.T) {
			f := newRawJobFixture(t)
			job := readRawJob(t, f)
			switch mode {
			case "ray":
				job.Spec.Tasks[0].Workload.Kind = "ray"
			case "multiple":
				job.Spec.Tasks = append(job.Spec.Tasks, job.Spec.Tasks[0])
			case "binding":
				job.Spec.Tasks[0].Placement.Binding = &api.RawTaskBinding{}
			case "environment":
				job.Spec.Tasks[0].Placement.Environment = ""
			case "gpu-env":
				job.Spec.Tasks[0].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/python3"],"directory":"/work","env":["CUDA_VISIBLE_DEVICES=all"]}`)
			case "unknown-field":
				job.Spec.Tasks[0].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/python3"],"directory":"/work","unknown":true}`)
			case "cordoned":
				node := f.node.DeepCopy()
				node.Spec.Unschedulable = true
				require.NoError(t, f.c.Update(context.Background(), node))
			case "stale-heartbeat":
				node := f.node.DeepCopy()
				node.Status.LastHeartbeatTime = nil
				require.NoError(t, f.c.Status().Update(context.Background(), node))
			case "foreign-tenant":
				job.Labels = map[string]string{tenant.IDLabel: "other"}
			}
			require.NoError(t, f.c.Update(context.Background(), job))
			for range 4 {
				f.reconcile(t, f.c)
			}
			require.Nil(t, f.reservation(t))
			var tasks api.TaskList
			require.NoError(t, f.c.List(context.Background(), &tasks))
			for _, task := range tasks.Items {
				require.Nil(t, task.Spec.Placement.Binding)
			}
			result := readRawJob(t, f)
			if mode == "cordoned" || mode == "stale-heartbeat" || mode == "foreign-tenant" {
				require.Equal(t, api.JobPhasePending, result.Status.Phase)
			} else {
				require.Equal(t, api.JobPhaseFailed, result.Status.Phase)
			}
		})
	}
}

type jobSupervisor struct {
	record      *rawruntime.Record
	starts      int
	unavailable bool
}

func (s *jobSupervisor) Observe(context.Context, rt.TaskIdentity) (rawruntime.Record, error) {
	if s.unavailable {
		return rawruntime.Record{}, errors.New("supervisor unavailable")
	}
	if s.record == nil {
		return rawruntime.Record{}, fault.New(fault.NotFound, "absent")
	}
	return *s.record, nil
}
func (s *jobSupervisor) Ensure(_ context.Context, t rt.TaskIdentity, p rawruntime.ProcessSpec) (rawruntime.Record, error) {
	if s.record == nil {
		s.starts++
		s.record = &rawruntime.Record{Task: t, Spec: p, Phase: "Running"}
	}
	return *s.record, nil
}
func (s *jobSupervisor) Stop(context.Context, rt.TaskIdentity) (rawruntime.Record, error) {
	if s.unavailable {
		return rawruntime.Record{}, errors.New("supervisor unavailable")
	}
	if s.record == nil {
		return rawruntime.Record{}, fault.New(fault.NotFound, "absent")
	}
	s.record.Cleaned = true
	s.record.Deleted = true
	return *s.record, nil
}

func TestRawJobReplacementWaitsForCleanupAndUsesNewIdentity(t *testing.T) {
	for _, mode := range []string{"spec-change", "restart", "resume"} {
		t.Run(mode, func(t *testing.T) {
			f := newRawJobFixture(t)
			old := f.bind(t)
			s := &jobSupervisor{}
			runRawHost(t, f.agent(t, s), old)
			job := readRawJob(t, f)
			if mode == "resume" {
				job.Spec.Suspended = true
				job.Generation++
				require.NoError(t, f.c.Update(context.Background(), job))
				f.reconcile(t, f.c)
				runRawHost(t, f.agent(t, s), f.task(t))
				f.reconcile(t, f.c)
				require.Nil(t, f.reservation(t))
				job = readRawJob(t, f)
				job.Spec.Suspended = false
			} else if mode == "restart" {
				job.Annotations = map[string]string{RestartedAtAnnotation: "operation-2"}
			} else {
				job.Spec.Tasks[0].Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/python3","-c","print(43)"],"directory":"/work"}`)
			}
			job.Generation++
			require.NoError(t, f.c.Update(context.Background(), job))
			f.reconcile(t, f.c)
			f.reconcile(t, f.c)
			require.Equal(t, old.UID, f.task(t).UID)
			require.False(t, f.task(t).DeletionTimestamp.IsZero())
			// A replacement cannot bypass the Agent's durable cleanup finalizer.
			runRawHost(t, f.agent(t, s), f.task(t))
			for range 4 {
				f.reconcile(t, f.c)
			}
			replacement := f.task(t)
			require.NotEqual(t, old.UID, replacement.UID)
			require.NotNil(t, replacement.Spec.Placement.Binding)
			require.Equal(t, string(replacement.UID), f.reservation(t).TaskUID)
			// A fresh supervisor view stands in for a different execution record.
			next := &jobSupervisor{}
			runRawHost(t, f.agent(t, next), replacement)
			require.Equal(t, 1, s.starts)
			require.Equal(t, 1, next.starts)
		})
	}
}

func TestRawJobNeverAdoptsConflictingChild(t *testing.T) {
	f := newRawJobFixture(t)
	task := f.bind(t)
	old := *f.reservation(t)
	task.OwnerReferences[0].UID = "foreign-job"
	require.NoError(t, f.c.Update(context.Background(), task))
	r := &Reconciler{Client: f.c, RawClient: f.c, Scheme: f.scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.job)})
	require.ErrorContains(t, err, "conflicting Job ownership")
	require.Equal(t, old, *f.reservation(t))
	require.True(t, f.task(t).DeletionTimestamp.IsZero())
}
