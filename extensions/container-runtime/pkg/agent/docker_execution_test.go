//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/system"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func dockerTaskFixture(t *testing.T) (TaskService, Identity, *Assignment) {
	t.Helper()
	s, id, a := taskFixture(t)
	ctx := context.Background()
	// Start from a complete, already UID-bound fixture and change every runtime
	// authority together. Allocation fields are runtime neutral by contract.
	var node ext.ContainerNode
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &node))
	node.Spec.Runtime = "Docker"
	must(t, s.Reporter.Client.Update(ctx, &node))
	var nn native.Node
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn))
	nn.Spec.AgentType = native.AgentTypeDocker
	must(t, s.Reporter.Client.Update(ctx, &nn))
	var cc ext.ContainerCluster
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cc))
	cc.Spec.Runtime = "Docker"
	must(t, s.Reporter.Client.Update(ctx, &cc))
	var b ext.ContainerJob
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, &b))
	b.Spec.Runtime = "Docker"
	must(t, s.Reporter.Client.Update(ctx, &b))
	a.Task.Spec.Runtime.Kind = "Docker"
	a.Task.Spec.Workload.Kind = dockerruntime.CarrierType
	a.Task.Spec.Workload.Version = "v1"
	a.Task.Spec.Workload.Carrier.Type = dockerruntime.CarrierType
	a.Task.Spec.Workload.Parameters.Raw = []byte(`{"image":"image","entrypoint":["/bin/sh"],"command":["-c","exit 0"],"hostNetwork":true}`)
	must(t, s.Reporter.Client.Update(ctx, &a.Task))
	var j native.Job
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Name: "job"}, &j))
	j.Spec.Tasks[0].TaskSpec = *a.Task.Spec.DeepCopy()
	must(t, s.Reporter.Client.Update(ctx, &j))
	must(t, s.Reporter.Report(ctx, id, Observation{Runtime: "Docker", InstanceID: id.InstanceID, Version: "v1", PrivateAddress: node.Spec.PrivateAddress, VisibleGPUUUIDs: []string{"GPU-a"}, Capacity: map[string]string{"cpu": "20", "memory": "236Gi"}, Capabilities: []string{"docker-observation-v1", DockerExecutionCapability}}))
	var err error
	a, err = s.Handle(ctx, id, TaskRequest{Action: "assignment"})
	must(t, err)
	_, err = s.Handle(ctx, id, requestFor(a, "authorize"))
	must(t, err)
	return s, id, a
}

// TestDockerScopedExecutionUnixSDKAndMTLS uses real mTLS Task transport and
// official Docker SDK Unix-socket transport against a deterministic fake daemon.
// It verifies lifecycle integration; it does not claim a real GPU container ran.
func TestDockerScopedExecutionUnixSDKAndMTLS(t *testing.T) {
	for _, outcome := range []string{"suspend", "succeeded", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			s, id, a := dockerTaskFixture(t)
			ctx := context.Background()
			var mu sync.Mutex
			var current *types.ContainerJSON
			creates, starts, removes := 0, 0, 0
			daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				path := strings.TrimPrefix(r.URL.Path, "/v1.44")
				switch {
				case path == "/_ping":
					w.Header().Set("API-Version", "1.44")
				case path == "/version":
					_ = json.NewEncoder(w).Encode(types.Version{Version: "26.1", APIVersion: "1.44"})
				case path == "/info":
					_ = json.NewEncoder(w).Encode(system.Info{ID: id.InstanceID})
				case path == "/images/image/json":
					_, _ = fmt.Fprint(w, `{"Id":"image"}`)
				case path == "/containers/json":
					if current == nil {
						_, _ = fmt.Fprint(w, `[]`)
					} else {
						_ = json.NewEncoder(w).Encode([]types.Container{{ID: current.ID}})
					}
				case path == "/containers/create":
					var input struct {
						container.Config
						HostConfig *container.HostConfig
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						http.Error(w, "bad", 400)
						return
					}
					creates++
					current = &types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{ID: "container-id", Name: "/" + r.URL.Query().Get("name"), State: &types.ContainerState{Status: "created"}, HostConfig: input.HostConfig}, Config: &input.Config}
					w.WriteHeader(201)
					_, _ = fmt.Fprint(w, `{"Id":"container-id"}`)
				case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
					if current == nil {
						w.WriteHeader(404)
						_, _ = fmt.Fprint(w, `{"message":"absent"}`)
					} else {
						_ = json.NewEncoder(w).Encode(current)
					}
				case path == "/containers/container-id/start":
					starts++
					current.State.Status = "running"
					current.State.Running = true
					w.WriteHeader(204)
				case path == "/containers/container-id/stop":
					current.State.Status = "exited"
					current.State.Running = false
					w.WriteHeader(204)
				case path == "/containers/container-id" && r.Method == http.MethodDelete:
					removes++
					current = nil
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected Engine request %s %s", r.Method, path)
					http.Error(w, "unexpected", 500)
				}
			}))
			socket := filepath.Join(t.TempDir(), "engine.sock")
			listener, err := net.Listen("unix", socket)
			must(t, err)
			daemon.Listener = listener
			daemon.Start()
			defer daemon.Close()
			engine, err := dockerruntime.NewSDKClient("unix://" + socket)
			must(t, err)
			defer func() {
				if err := engine.Close(); err != nil {
					t.Error(err)
				}
			}()
			state := t.TempDir()
			must(t, os.Chmod(state, 0700))
			journal, err := dockerruntime.OpenJournal(state)
			must(t, err)
			defer func() {
				if err := journal.Close(); err != nil {
					t.Error(err)
				}
			}()
			o := DockerExecutionObserver{Client: transport(t, s, id), Engine: engine, Journal: journal, EngineID: id.InstanceID, GPUUUIDs: []string{"GPU-a", "GPU-extra"}}
			for range 3 {
				must(t, o.reconcile(ctx, true))
			}
			mu.Lock()
			if creates != 1 || starts != 1 || len(current.HostConfig.DeviceRequests) != 1 || current.HostConfig.DeviceRequests[0].DeviceIDs[0] != "GPU-a" || !current.HostConfig.NetworkMode.IsHost() {
				t.Fatalf("wrong Docker creation: %+v", current)
			}
			mu.Unlock()
			var task native.Task
			must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Task), &task))
			if task.Status.Phase != native.TaskPhaseRunning || !strings.Contains(task.Annotations[host.TaskClaimantAnnotation], "/Docker/") {
				t.Fatalf("wrong status/claim: %+v", task)
			}
			// Restart the Agent owner without losing the local cancellation state.
			must(t, journal.Close())
			journal, err = dockerruntime.OpenJournal(state)
			must(t, err)
			defer func() {
				if err := journal.Close(); err != nil {
					t.Error(err)
				}
			}()
			o.Journal = journal
			if outcome == "suspend" {
				task.Spec.Suspended = true
				task.Generation++
				must(t, s.Reporter.Client.Update(ctx, &task))
			} else {
				mu.Lock()
				current.State.Status = "exited"
				current.State.Running = false
				if outcome == "failed" {
					current.State.ExitCode = 7
				}
				mu.Unlock()
				must(t, o.reconcile(ctx, true))
			}
			must(t, o.reconcile(ctx, false))
			var allocation ext.ContainerAllocation
			must(t, s.Reporter.Client.Get(ctx, client.ObjectKeyFromObject(&a.Allocation), &allocation))
			if !allocation.Status.CleanupConfirmed {
				t.Fatal("allocation released without cleanup proof")
			}
			mu.Lock()
			if current != nil || removes != 1 || creates != 1 {
				t.Fatal("runtime cleanup not completed")
			}
			mu.Unlock()
			if outcome != "suspend" {
				reconciler := controller.JobReservationReconciler{Client: s.Reporter.Client}
				for range 4 {
					_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "job"}})
					must(t, err)
				}
				var job native.Job
				must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Name: "job"}, &job))
				want := native.JobPhaseSucceeded
				if outcome == "failed" {
					want = native.JobPhaseFailed
				}
				if job.Status.Phase != want {
					t.Fatalf("job status: %+v", job.Status)
				}
			}
		})
	}
}

func TestDockerTaskAuthorizationRejectsMissingExecutionCapabilityAndRay(t *testing.T) {
	s, id, a := dockerTaskFixture(t)
	ctx := context.Background()
	var node ext.ContainerNode
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &node))
	node.Status.Capabilities = []string{"docker-observation-v1"}
	must(t, s.Reporter.Client.Status().Update(ctx, &node))
	if _, err := s.Handle(ctx, id, requestFor(a, "authorize")); err == nil {
		t.Fatal("observation-only node dispatched")
	}
	node.Status.Capabilities = append(node.Status.Capabilities, DockerExecutionCapability)
	must(t, s.Reporter.Client.Status().Update(ctx, &node))
	_, err := s.Handle(ctx, id, requestFor(a, "authorize"))
	must(t, err)
	var job native.Job
	must(t, s.Reporter.Client.Get(ctx, client.ObjectKey{Name: "job"}, &job))
	job.Spec.Tasks[0].Workload.Kind = "rlinf.io/ray"
	must(t, s.Reporter.Client.Update(ctx, &job))
	if _, err := s.Handle(ctx, id, requestFor(a, "authorize")); err == nil {
		t.Fatal("unsupported Ray group authorized")
	}
}
