package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	rawruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func requestFor(a *Assignment, action string) TaskRequest {
	return TaskRequest{Action: action, TaskUID: string(a.Task.UID), Allocation: objectRef(&a.Allocation)}
}

func taskFixture(t *testing.T) (TaskService, Identity, *Assignment) {
	return taskFixtureWithParameters(t, []byte(`{"argv":["/usr/bin/true"],"env":[],"directory":"/work"}`))
}

func taskFixtureWithParameters(t *testing.T, parameters []byte) (TaskService, Identity, *Assignment) {
	t.Helper()
	ctx := context.Background()
	r, id, n := observerFixture(t)
	r.Now = nil
	var cc ext.ContainerCluster
	must(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cc))
	var cluster native.Cluster
	must(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "", Name: "cloud"}, &cluster))
	var nn native.Node
	must(t, r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn))
	task := &native.Task{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{utils.ParentUIDAnnotation: "job", utils.ChildTemplateAnnotation: "worker"}, Name: "job-worker", Namespace: "agents", UID: "task", Generation: 1, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&native.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", UID: "job"}}, native.GroupVersion.WithKind("Job"))}}, Spec: native.TaskSpec{Runtime: native.TaskRuntimeSpec{Kind: "Raw"}, Placement: native.TaskPlacementSpec{Environment: "fleet"}, Workload: native.TaskWorkloadSpec{Kind: "raw.rlark.io/process", Version: "v1", Carrier: native.TaskWorkloadCarrier{Type: "raw.rlark.io/process", Version: "v1"}, Parameters: extensions.JSON{Raw: []byte(`{"argv":["/usr/bin/true"],"env":[],"directory":"/work"}`)}}}}
	task.Spec.Workload.Parameters.Raw = parameters
	job := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", UID: "job", Generation: 1, Annotations: map[string]string{nativejob.ControllerOwnerAnnotation: ext.Group}}, Spec: native.JobSpec{Tasks: []native.JobTaskTemplate{{Name: "worker", TaskSpec: *task.Spec.DeepCopy()}}}}
	binding := &ext.ContainerJob{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "agents", UID: "binding"}, Spec: ext.ContainerJobSpec{Cluster: n.Spec.Cluster, Job: objectRef(job), TenantID: tenant.DefaultID, Runtime: "Raw"}}
	r.Client = fake.NewClientBuilder().WithScheme(r.Client.Scheme()).WithStatusSubresource(n, task, &ext.ContainerAllocation{}, job).WithObjects(n, &cluster, &nn, &cc, job, binding, task).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if _, ok := o.(*ext.ContainerAllocation); ok {
			o.SetUID("allocation")
			o.SetGeneration(1)
		}
		return c.Create(ctx, o, opts...)
	}}).Build()
	must(t, r.Report(ctx, id, Observation{Version: "v1", PrivateAddress: n.Spec.PrivateAddress, VisibleGPUUUIDs: n.Spec.AuthorizedGPUUUIDs}))
	_, err := (companion.Manager{Client: r.Client, Now: r.Now}).Acquire(ctx, companion.Request{Binding: client.ObjectKeyFromObject(binding), Node: client.ObjectKeyFromObject(n), Task: client.ObjectKeyFromObject(task)})
	must(t, err)
	s := TaskService{Reporter: r}
	a, err := s.Handle(ctx, id, TaskRequest{Action: "assignment"})
	must(t, err)
	if a == nil {
		t.Fatal("missing assignment")
	}
	return s, id, a
}

func TestTaskTransportFencesEveryIdentityAndStartState(t *testing.T) {
	for _, mode := range []string{"valid", "credential", "tenant", "node UID", "allocation UID", "task UID", "task generation", "job generation", "job suspend", "task suspend", "cordon", "node unschedulable", "heartbeat", "release", "selector", "deleted cluster"} {
		t.Run(mode, func(t *testing.T) {
			s, id, a := taskFixture(t)
			ctx := context.Background()
			req := requestFor(a, "authorize")
			// Establish a valid baseline before changing exactly one fence. This
			// prevents unrelated fixture failures from making negative cases pass.
			_, baselineErr := s.Handle(ctx, id, req)
			must(t, baselineErr)
			switch mode {
			case "credential":
				id.CredentialID = "old"
			case "tenant":
				id.TenantID = "foreign"
			case "node UID":
				id.UID = "old"
			case "allocation UID":
				req.Allocation.UID = "old"
			case "task UID":
				req.TaskUID = "old"
			case "task generation":
				a.Task.Generation++
				must(t, s.Reporter.Client.Update(ctx, &a.Task))
			case "task suspend":
				a.Task.Spec.Suspended = true
				must(t, s.Reporter.Client.Update(ctx, &a.Task))
			case "job generation", "job suspend":
				var j native.Job
				must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Name: "job"}, &j))
				if mode == "job generation" {
					j.Generation++
				} else {
					j.Spec.Suspended = true
				}
				must(t, s.Reporter.Client.Update(ctx, &j))
			case "cordon", "deleted cluster":
				var c native.Cluster
				must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "", Name: "cloud"}, &c))
				if mode == "cordon" {
					c.Spec.Cordoned = true
					must(t, s.Reporter.Client.Update(ctx, &c))
				} else {
					must(t, s.Reporter.Client.Delete(ctx, &c))
				}
			case "node unschedulable":
				var nn native.Node
				must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn))
				nn.Spec.Unschedulable = true
				must(t, s.Reporter.Client.Update(ctx, &nn))
			case "heartbeat":
				s.Reporter.Now = func() time.Time { return time.Now().Add(time.Minute) }
			case "release":
				var c ext.ContainerCluster
				must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &c))
				c.Spec.TargetAgentRelease = "missing"
				must(t, s.Reporter.Client.Update(ctx, &c))
			case "selector":
				a.Task.Spec.Placement.NodeSelector = map[string]string{"rack": "missing"}
				must(t, s.Reporter.Client.Update(ctx, &a.Task))
			}
			_, err := s.Handle(ctx, id, req)
			if mode == "valid" {
				if err != nil {
					t.Fatalf("valid error: %T %v", err, err)
				}
			} else if err == nil {
				t.Fatal("unauthorized start was allowed")
			}
		})
	}
}

func transport(t *testing.T, s TaskService, id Identity) *Client {
	t.Helper()
	pool, certificate := agentCertificate(t, id.Metadata())
	server := httptest.NewUnstartedServer(Handler{Reporter: s.Reporter, EnableTasks: true})
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	t.Cleanup(server.Close)
	server.Client().Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{certificate}
	return &Client{Endpoint: server.URL, HTTPClient: server.Client()}
}

type memorySupervisor struct {
	record         *rawruntime.Record
	ensures, stops int
	stopErr        error
}

func (s *memorySupervisor) Observe(_ context.Context, _ rt.TaskIdentity) (rawruntime.Record, error) {
	if s.record == nil {
		return rawruntime.Record{}, fault.New(fault.NotFound, "absent")
	}
	return *s.record, nil
}
func (s *memorySupervisor) Ensure(_ context.Context, t rt.TaskIdentity, spec rawruntime.ProcessSpec) (rawruntime.Record, error) {
	if s.record == nil {
		s.ensures++
		s.record = &rawruntime.Record{Task: t, Spec: spec, Phase: "Running"}
	}
	if s.record.Deleted {
		return rawruntime.Record{}, fault.New(fault.Conflict, "cancelled")
	}
	return *s.record, nil
}
func (s *memorySupervisor) Stop(_ context.Context, t rt.TaskIdentity) (rawruntime.Record, error) {
	s.stops++
	if s.stopErr != nil {
		return rawruntime.Record{}, s.stopErr
	}
	if s.record == nil {
		s.record = &rawruntime.Record{Task: t}
	}
	s.record.Deleted = true
	s.record.Cleaned = true
	s.record.Phase = "Stopped"
	return *s.record, nil
}

func TestScopedHostExecutionRestartGPUAndCleanup(t *testing.T) {
	s, id, a := taskFixture(t)
	c := transport(t, s, id)
	supervisor := &memorySupervisor{}
	ctx := context.Background()
	for range 3 {
		must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx))
	}
	if supervisor.ensures != 1 || supervisor.record == nil || !reflect.DeepEqual(supervisor.record.Spec.Env, []string{"CUDA_VISIBLE_DEVICES=GPU-a"}) {
		t.Fatalf("unexpected execution: %+v", supervisor)
	}
	var task native.Task
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Task), &task))
	if task.Status.Phase != native.TaskPhaseRunning || task.Status.ObservedGeneration != task.Generation {
		t.Fatalf("missing current status: %+v", task.Status)
	}
	task.Spec.Suspended = true
	task.Generation++
	must(t, s.Reporter.Client.Update(ctx, &task))
	must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx))
	var allocation ext.ContainerAllocation
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Allocation), &allocation))
	if !allocation.Status.CleanupConfirmed || supervisor.stops != 1 {
		t.Fatalf("cleanup not proven: %+v", allocation.Status)
	}
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&task), &task))
	if task.Annotations[host.TaskClaimantAnnotation] != "" {
		t.Fatal("claim remained after cleanup")
	}
	must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx))
}

func TestStopBeforeClaimRequiresSupervisorProofAndRecovers(t *testing.T) {
	s, id, a := taskFixture(t)
	ctx := context.Background()
	task := a.Task.DeepCopy()
	task.Spec.Suspended = true
	task.Generation++
	must(t, s.Reporter.Client.Update(ctx, task))
	c := transport(t, s, id)
	supervisor := &memorySupervisor{stopErr: errors.New("supervisor unavailable")}
	execution := ExecutionObserver{Client: c, Supervisor: supervisor}
	if err := execution.Reconcile(ctx); err == nil {
		t.Fatal("supervisor failure was hidden")
	}
	var got ext.ContainerAllocation
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Allocation), &got))
	if got.Status.CleanupConfirmed {
		t.Fatal("absent inventory was treated as proof")
	}
	supervisor.stopErr = nil
	must(t, execution.Reconcile(ctx))
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Allocation), &got))
	if !got.Status.CleanupConfirmed || supervisor.record == nil || !supervisor.record.Deleted {
		t.Fatal("durable cancellation was not recorded")
	}
}

func TestTaskClaimAndStatusRejectForeignFieldsAndStaleVersions(t *testing.T) {
	s, id, a := taskFixture(t)
	ctx := context.Background()
	c := transport(t, s, id)
	scoped := ScopedTasks{Client: c, Allocation: objectRef(&a.Allocation), Task: objectRef(&a.Task)}
	task := a.Task.DeepCopy()
	req := requestFor(a, "claim")
	req.ResourceVersion = task.ResourceVersion
	req.Patch = []byte(`[{"op":"add","path":"/spec/suspended","value":true}]`)
	if _, err := s.Handle(ctx, id, req); !errors.Is(err, ErrCapability) {
		t.Fatalf("spec patch allowed: %v", err)
	}
	if err := scoped.Get(ctx, client.ObjectKey{Name: "other", Namespace: "agents"}, &native.Task{}); !errors.Is(err, ErrCapability) {
		t.Fatal("foreign read allowed")
	}
	if err := scoped.Get(ctx, client.ObjectKeyFromObject(task), &ext.ContainerNode{}); !errors.Is(err, ErrCapability) {
		t.Fatal("foreign resource allowed")
	}
	supervisor := &memorySupervisor{}
	must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx))
	req.Status = &host.StatusIntent{TaskUID: string(task.UID), Generation: task.Generation, Phase: "Running"}
	req.Action = "status"
	req.Patch = nil
	if _, err := s.Handle(ctx, id, req); !apierrors.IsConflict(err) {
		t.Fatalf("stale status was allowed: %v", err)
	}
	must(t, scoped.Get(ctx, client.ObjectKeyFromObject(task), task))
	req.ResourceVersion = task.ResourceVersion
	req.Status.Conditions = []host.ConditionMutation{{Condition: metav1.Condition{Type: "Validated", Status: metav1.ConditionTrue, Reason: "Forged", ObservedGeneration: task.Generation}}}
	if _, err := s.Handle(ctx, id, req); !errors.Is(err, ErrCapability) {
		t.Fatalf("foreign condition allowed: %v", err)
	}
	before := task.DeepCopy()
	must(t, scoped.Get(ctx, client.ObjectKeyFromObject(task), task))
	if !reflect.DeepEqual(task, before) {
		t.Fatal("rejected writes changed Task")
	}
}

func TestCleanupStillWorksWhileBindingsDeleteOrHeartbeatExpires(t *testing.T) {
	s, id, a := taskFixture(t)
	ctx := context.Background()
	a.Task.Spec.Suspended = true
	a.Task.Generation++
	must(t, s.Reporter.Client.Update(ctx, &a.Task))
	var cluster native.Cluster
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "", Name: "cloud"}, &cluster))
	cluster.Finalizers = []string{"test/hold"}
	must(t, s.Reporter.Client.Update(ctx, &cluster))
	must(t, s.Reporter.Client.Delete(ctx, &cluster))
	var node ext.ContainerNode
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, &node))
	node.Finalizers = []string{"test/hold"}
	must(t, s.Reporter.Client.Update(ctx, &node))
	must(t, s.Reporter.Client.Delete(ctx, &node))
	s.Reporter.Now = func() time.Time { return time.Now().Add(time.Hour) }
	_, err := s.Handle(ctx, id, requestFor(a, "cleanup"))
	must(t, err)
	id.CredentialID = "old"
	if _, err := s.Handle(ctx, id, requestFor(a, "cleanup")); !errors.Is(err, ErrIdentity) {
		t.Fatalf("revoked cleanup identity allowed: %v", err)
	}
}

func TestDecoderRejectsGPUOverride(t *testing.T) {
	_, _, a := taskFixture(t)
	a.Task.Spec.Workload.Parameters.Raw = []byte(`{"argv":["/usr/bin/true"],"env":["CUDA_VISIBLE_DEVICES=all"],"directory":"/work"}`)
	if _, err := (allocatedEligibility{assignment: a}).decode(&a.Task); !errors.Is(err, ErrCapability) {
		t.Fatalf("override was allowed: %v", err)
	}
}

func TestClaimRecoversAfterCommittedResponseLost(t *testing.T) {
	s, id, a := taskFixture(t)
	lost := false
	s.Reporter.Client = interceptor.NewClient(s.Reporter.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
		if err := c.Patch(ctx, o, p, opts...); err != nil {
			return err
		}
		if _, ok := o.(*native.Task); ok && !lost {
			lost = true
			return errors.New("claim response lost")
		}
		return nil
	}})
	c := transport(t, s, id)
	supervisor := &memorySupervisor{}
	if err := (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(context.Background()); err == nil || supervisor.ensures != 0 {
		t.Fatal("uncertain claim allowed a start")
	}
	// A new Agent/Host instance recovers the persisted claim rather than taking
	// another allocation or starting the same execution twice.
	for range 2 {
		must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(context.Background()))
	}
	if !lost || supervisor.ensures != 1 || supervisor.record.Task.ObjectUID != string(a.Task.UID) {
		t.Fatal("claim recovery lost exact execution identity")
	}
}

func TestCleanupRecoversFromPartialEffectsAndLostResponses(t *testing.T) {
	for _, mode := range []string{"task patch response lost", "allocation patch failed", "allocation response lost"} {
		t.Run(mode, func(t *testing.T) {
			s, id, a := taskFixture(t)
			ctx := context.Background()
			supervisor := &memorySupervisor{}
			must(t, (ExecutionObserver{Client: transport(t, s, id), Supervisor: supervisor}).Reconcile(ctx))
			var task native.Task
			must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Task), &task))
			task.Spec.Suspended = true
			task.Generation++
			must(t, s.Reporter.Client.Update(ctx, &task))
			injected := false
			s.Reporter.Client = interceptor.NewClient(s.Reporter.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
					if err := c.Patch(ctx, o, p, opts...); err != nil {
						return err
					}
					if mode == "task patch response lost" && !injected {
						injected = true
						return errors.New("response lost")
					}
					return nil
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
					if _, ok := o.(*ext.ContainerAllocation); ok && !injected && mode != "task patch response lost" {
						injected = true
						if mode == "allocation response lost" {
							if err := c.SubResource(sub).Patch(ctx, o, p, opts...); err != nil {
								return err
							}
						}
						return errors.New("allocation unavailable")
					}
					return c.SubResource(sub).Patch(ctx, o, p, opts...)
				},
			})
			c := transport(t, s, id)
			if err := (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx); err == nil || !injected {
				t.Fatal("failure was hidden")
			}
			for range 2 {
				must(t, (ExecutionObserver{Client: c, Supervisor: supervisor}).Reconcile(ctx))
			}
			var allocation ext.ContainerAllocation
			must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Allocation), &allocation))
			must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&task), &task))
			if !allocation.Status.CleanupConfirmed || task.Annotations[host.TaskClaimantAnnotation] != "" || slices.Contains(task.Finalizers, host.TaskFinalizer) || supervisor.ensures != 1 {
				t.Fatal("partial cleanup did not recover")
			}
		})
	}
}

func TestObservationFailureAllowsOnlyCleanup(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		s, id, a := taskFixture(t)
		if suspended {
			a.Task.Spec.Suspended = true
			must(t, s.Reporter.Client.Update(context.Background(), &a.Task))
		}
		supervisor := &memorySupervisor{}
		must(t, (ExecutionObserver{Client: transport(t, s, id), Supervisor: supervisor}).reconcile(context.Background(), false))
		if supervisor.ensures != 0 || (suspended && supervisor.stops != 1) || (!suspended && supervisor.stops != 0) {
			t.Fatal("cleanup-only path performed wrong external effect")
		}
	}
}

func TestTaskHTTPRejectsForeignIdentityAndMalformedRequests(t *testing.T) {
	for _, mode := range []string{"disabled", "foreign tenant", "foreign node", "unknown fields", "multiple objects", "oversized", "wrong method", "no certificate"} {
		t.Run(mode, func(t *testing.T) {
			s, id, _ := taskFixture(t)
			switch mode {
			case "foreign tenant":
				id.TenantID = "foreign"
			case "foreign node":
				id.UID = "foreign"
			}
			c := transport(t, s, id)
			body := `{"action":"assignment"}`
			status, method := http.StatusBadRequest, http.MethodPost
			switch mode {
			case "foreign tenant", "foreign node":
				status = http.StatusForbidden
			case "unknown fields":
				body = `{"action":"assignment","namespace":"other"}`
			case "multiple objects":
				body += `{}`
			case "oversized":
				body = `{"action":"` + strings.Repeat("a", 1<<20) + `"}`
			case "wrong method":
				method, status = http.MethodGet, http.StatusMethodNotAllowed
			case "disabled", "no certificate":
				h := Handler{Reporter: s.Reporter, EnableTasks: mode != "disabled"}
				r := httptest.NewRequest(method, "/v1/task", strings.NewReader(body))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				expected := http.StatusForbidden
				if mode == "disabled" {
					expected = http.StatusNotFound
				}
				if w.Code != expected {
					t.Fatalf("got %d want %d", w.Code, expected)
				}
				return
			}
			r, err := http.NewRequest(method, c.Endpoint+"/v1/task", strings.NewReader(body))
			must(t, err)
			res, err := c.HTTPClient.Do(r)
			must(t, err)
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != status {
				t.Fatalf("got %d want %d", res.StatusCode, status)
			}
		})
	}
}
