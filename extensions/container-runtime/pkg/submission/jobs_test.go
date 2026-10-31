package submission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func meta(name, ns, uid string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Generation: 1, Labels: map[string]string{tenant.IDLabel: "team-a"}}
}
func fixture(t *testing.T) (Service, ClusterTarget, Request) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, native.AddToScheme(scheme))
	require.NoError(t, ext.AddToScheme(scheme))
	nc := &native.Cluster{ObjectMeta: meta("cloud", "", "nc"), Spec: native.ClusterSpec{AgentNamespace: "agents"}}
	cc := &ext.ContainerCluster{ObjectMeta: meta("cloud", "agents", "cc"), Spec: ext.ContainerClusterSpec{Cluster: reference(nc), AgentNamespace: "agents", Runtime: "Raw"}}
	nn := &native.Node{ObjectMeta: meta("node", "agents", "nn"), Spec: native.NodeSpec{AgentType: native.AgentTypeRaw}}
	cn := &ext.ContainerNode{ObjectMeta: meta("node", "agents", "cn"), Spec: ext.ContainerNodeSpec{Node: reference(nn), Cluster: reference(nc), Runtime: "Raw", Environment: "prepared", AuthorizedGPUUUIDs: []string{"GPU-1"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc, cc, nn, cn).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if o.GetUID() == "" {
			o.SetUID(types.UID(uuid.NewString()))
		}
		o.SetGeneration(1)
		return c.Create(ctx, o, opts...)
	}}).Build()
	return Service{Client: c}, ClusterTarget{Namespace: "agents", Name: "cloud", UID: "cc"}, Request{OperationID: uuid.NewString(), ClusterUID: "cc", Environment: "prepared", Process: raw.ProcessSpec{Argv: []string{"/usr/bin/python3", "train.py"}, Directory: "/work", Env: []string{}}}
}

func TestSubmitRecoveryAndSuspensionDoesNotReplay(t *testing.T) {
	for _, lost := range []string{"job", "binding", "activation"} {
		t.Run(lost, func(t *testing.T) {
			s, target, req := fixture(t)
			base := s.Client
			injected := false
			s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
					err := c.Create(ctx, o, opts...)
					_, isJob := o.(*native.Job)
					_, isBinding := o.(*ext.ContainerJob)
					if err == nil && !injected && ((lost == "job" && isJob) || (lost == "binding" && isBinding)) {
						injected = true
						return errors.New("committed response lost")
					}
					return err
				},
				Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
					err := c.Patch(ctx, o, p, opts...)
					if err == nil && !injected && lost == "activation" {
						injected = true
						return errors.New("activation response lost")
					}
					return err
				},
			})
			first, err := s.Submit(context.Background(), target, req)
			if lost == "activation" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.True(t, first.Activated)
			}
			require.True(t, injected)
			s = Service{Client: base}
			done, err := s.Submit(context.Background(), target, req)
			require.NoError(t, err)
			require.True(t, done.Activated)
			require.False(t, done.Suspended)
			stopped, err := s.Suspend(context.Background(), target, done.Name, done.UID, done.ResourceVersion)
			require.NoError(t, err)
			require.True(t, stopped.Suspended)
			again, err := s.Submit(context.Background(), target, req)
			require.NoError(t, err)
			require.True(t, again.Suspended)
			require.Equal(t, done.UID, again.UID)
			var jobs native.JobList
			require.NoError(t, base.List(context.Background(), &jobs))
			require.Len(t, jobs.Items, 1)
			var bindings ext.ContainerJobList
			require.NoError(t, base.List(context.Background(), &bindings))
			require.Len(t, bindings.Items, 1)
		})
	}
}

func TestPartialBindingFailureLeavesSuspendedJob(t *testing.T) {
	s, target, req := fixture(t)
	base := s.Client
	s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if _, ok := o.(*ext.ContainerJob); ok {
			return errors.New("unavailable")
		}
		return c.Create(ctx, o, opts...)
	}})
	_, err := s.Submit(context.Background(), target, req)
	require.Error(t, err)
	var jobs native.JobList
	require.NoError(t, base.List(context.Background(), &jobs))
	require.Len(t, jobs.Items, 1)
	require.True(t, jobs.Items[0].Spec.Suspended)
	require.Empty(t, jobs.Items[0].Annotations[activatedAnnotation])
	done, err := (Service{Client: base}).Submit(context.Background(), target, req)
	require.NoError(t, err)
	require.True(t, done.Activated)
}

func TestSubmissionRejectsChangedInputAndReplacement(t *testing.T) {
	for _, mode := range []string{"argv", "directory", "environment", "cluster uid", "tenant", "owner", "workload", "missing job", "replaced job", "duplicate binding", "foreign binding", "native cluster", "duplicate cluster", "unknown environment", "docker"} {
		t.Run(mode, func(t *testing.T) {
			s, target, req := fixture(t)
			ctx := context.Background()
			if mode == "unknown environment" {
				req.Environment = "unknown"
				_, err := s.Submit(ctx, target, req)
				require.ErrorIs(t, err, ErrInvalid)
				return
			}
			if mode == "docker" {
				var cc ext.ContainerCluster
				require.NoError(t, s.Client.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &cc))
				cc.Spec.Runtime = "Docker"
				require.NoError(t, s.Client.Update(ctx, &cc))
				_, err := s.Submit(ctx, target, req)
				require.ErrorIs(t, err, ErrConflict)
				return
			}
			done, err := s.Submit(ctx, target, req)
			require.NoError(t, err)
			var j native.Job
			require.NoError(t, s.Client.Get(ctx, client.ObjectKey{Name: done.Name}, &j))
			switch mode {
			case "argv":
				req.Process.Argv[1] = "other.py"
			case "directory":
				req.Process.Directory = "/other"
			case "environment":
				req.Environment = "other"
			case "cluster uid":
				target.UID = "replacement"
				req.ClusterUID = target.UID
			case "tenant":
				j.Labels[tenant.IDLabel] = "other"
				require.NoError(t, s.Client.Update(ctx, &j))
			case "owner":
				j.Annotations["rlark.io/controller-owner"] = "foreign"
				require.NoError(t, s.Client.Update(ctx, &j))
			case "workload":
				j.Spec.Tasks[0].Role = "changed"
				require.NoError(t, s.Client.Update(ctx, &j))
			case "missing job", "replaced job":
				require.NoError(t, s.Client.Delete(ctx, &j))
				if mode == "replaced job" {
					j.UID = "replacement"
					j.ResourceVersion = ""
					require.NoError(t, s.Client.Create(ctx, &j))
				}
			case "duplicate binding", "foreign binding":
				var b ext.ContainerJob
				require.NoError(t, s.Client.Get(ctx, client.ObjectKey{Name: done.Name, Namespace: target.Namespace}, &b))
				if mode == "duplicate binding" {
					b.Name = "other"
					b.UID = "other"
					b.ResourceVersion = ""
					require.NoError(t, s.Client.Create(ctx, &b))
				} else {
					b.Labels[tenant.IDLabel] = "other"
					require.NoError(t, s.Client.Update(ctx, &b))
				}
			case "native cluster":
				var n native.Cluster
				require.NoError(t, s.Client.Get(ctx, client.ObjectKey{Name: target.Name}, &n))
				n.UID = "replaced"
				require.NoError(t, s.Client.Update(ctx, &n))
			case "duplicate cluster":
				n := &native.Cluster{ObjectMeta: meta("other", "", "other"), Spec: native.ClusterSpec{AgentNamespace: target.Namespace}}
				require.NoError(t, s.Client.Create(ctx, n))
			}
			_, err = s.Submit(ctx, target, req)
			require.ErrorIs(t, err, ErrConflict)
		})
	}
}

func TestCompletedSubmitAndListDoNotLeakUnboundPayload(t *testing.T) {
	s, target, req := fixture(t)
	ctx := context.Background()
	done, err := s.Submit(ctx, target, req)
	require.NoError(t, err)
	var j native.Job
	require.NoError(t, s.Client.Get(ctx, client.ObjectKey{Name: done.Name}, &j))
	j.Status.Phase = native.JobPhaseSucceeded
	j.Status.ObservedGeneration = j.Generation
	require.NoError(t, s.Client.Update(ctx, &j))
	replay, err := s.Submit(ctx, target, req)
	require.NoError(t, err)
	require.Equal(t, native.JobPhaseSucceeded, replay.Phase)
	list, err := s.List(ctx, target)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.True(t, list[0].BindingValid)
	data, err := json.Marshal(list)
	require.NoError(t, err)
	require.NotContains(t, string(data), "train.py")
	require.NoError(t, s.Client.Delete(ctx, &j))
	j.UID = "foreign"
	j.ResourceVersion = ""
	j.Spec.Tasks[0].Role = "private-payload"
	require.NoError(t, s.Client.Create(ctx, &j))
	list, err = s.List(ctx, target)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.False(t, list[0].BindingValid)
	require.Empty(t, list[0].UID)
	_, err = s.Suspend(ctx, target, done.Name, done.UID, done.ResourceVersion)
	require.ErrorIs(t, err, ErrConflict)
}

func TestConcurrentSubmitCreatesOnlyOneExecutionIntent(t *testing.T) {
	s, target, req := fixture(t)
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 20; attempt++ {
				_, err := s.Submit(context.Background(), target, req)
				if err == nil {
					return
				}
				if !apierrors.IsConflict(err) && !apierrors.IsInvalid(err) && !errors.Is(err, ErrConflict) {
					failures <- err
					return
				}
			}
			failures <- fmt.Errorf("retry did not converge")
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	var jobs native.JobList
	require.NoError(t, s.Client.List(context.Background(), &jobs))
	require.Len(t, jobs.Items, 1)
	var bindings ext.ContainerJobList
	require.NoError(t, s.Client.List(context.Background(), &bindings))
	require.Len(t, bindings.Items, 1)
	require.Equal(t, string(jobs.Items[0].UID), bindings.Items[0].Spec.Job.UID)
}

func TestInvalidSubmissionHasNoEffects(t *testing.T) {
	for _, mutate := range []func(*Request){func(r *Request) { r.OperationID = "bad" }, func(r *Request) { r.Process.Argv[0] = "python" }, func(r *Request) { r.Process.Directory = "../work" }, func(r *Request) { r.Process.Env = []string{"CUDA_VISIBLE_DEVICES=0"} }, func(r *Request) { r.Process.Env = []string{"NVIDIA_VISIBLE_DEVICES=all"} }, func(r *Request) { r.Process.Env = []string{"SECRET=x", "SECRET=y"} }} {
		s, target, req := fixture(t)
		mutate(&req)
		_, err := s.Submit(context.Background(), target, req)
		require.ErrorIs(t, err, ErrInvalid)
		var jobs native.JobList
		require.NoError(t, s.Client.List(context.Background(), &jobs))
		require.Empty(t, jobs.Items)
	}
}

func TestConcurrentSuspendFencesPendingActivation(t *testing.T) {
	s, target, req := fixture(t)
	base := s.Client
	// Pause the submitter just before activation, with a valid binding present.
	reached, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
		once.Do(func() { close(reached); <-resume })
		return c.Patch(ctx, o, p, opts...)
	}})
	done := make(chan error, 1)
	go func() { _, err := s.Submit(context.Background(), target, req); done <- err }()
	<-reached
	list, err := (Service{Client: base}).List(context.Background(), target)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.False(t, list[0].Activated)
	stop, err := (Service{Client: base}).Suspend(context.Background(), target, list[0].Name, list[0].UID, list[0].ResourceVersion)
	// Always release the submitter, including on a failed assertion.
	close(resume)
	require.NoError(t, err)
	require.True(t, stop.Suspended)
	require.ErrorIs(t, <-done, ErrConflict)
	again, err := (Service{Client: base}).Submit(context.Background(), target, req)
	require.NoError(t, err)
	require.True(t, again.Suspended)
}

func TestSuspendLostResponseRequiresReadAndPreservesStop(t *testing.T) {
	s, target, req := fixture(t)
	ctx := context.Background()
	job, err := s.Submit(ctx, target, req)
	require.NoError(t, err)
	base := s.Client
	s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
		if err := c.Patch(ctx, o, p, opts...); err != nil {
			return err
		}
		return errors.New("committed stop response lost")
	}})
	_, err = s.Suspend(ctx, target, job.Name, job.UID, job.ResourceVersion)
	require.Error(t, err)
	s.Client = base
	_, err = s.Suspend(ctx, target, job.Name, job.UID, job.ResourceVersion)
	require.ErrorIs(t, err, ErrConflict)
	list, err := s.List(ctx, target)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.True(t, list[0].Suspended)
	replay, err := s.Submit(ctx, target, req)
	require.NoError(t, err)
	require.True(t, replay.Suspended)
}
