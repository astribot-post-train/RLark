package integration

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestCloudRawLifecycle is deliberately opt-in: it creates real GPU Jobs in an
// already enrolled, dedicated single-node preview Cluster. Ordinary tests never
// access management credentials or start workloads.
func TestCloudRawLifecycle(t *testing.T) {
	kubeconfig := os.Getenv("RLARK_TEST_RAW_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set RLARK_TEST_RAW_KUBECONFIG and the documented dedicated cloud test inputs")
	}
	required := func(key string) string {
		t.Helper()
		value := os.Getenv(key)
		if value == "" {
			t.Fatalf("%s is required for real GPU acceptance", key)
		}
		return value
	}
	namespace := required("RLARK_TEST_RAW_NAMESPACE")
	clusterName := required("RLARK_TEST_RAW_CLUSTER")
	nodeName := required("RLARK_TEST_RAW_NODE")
	tenantID := required("RLARK_TEST_RAW_TENANT")
	executable := required("RLARK_TEST_RAW_EXECUTABLE")
	directory := required("RLARK_TEST_RAW_DIRECTORY")
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 10 * time.Second
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{native.AddToScheme, ext.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var cluster ext.ContainerCluster
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterName}, &cluster); err != nil {
		t.Fatal(err)
	}
	var nodes ext.ContainerNodeList
	if err := c.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 1 || nodes.Items[0].Name != nodeName {
		t.Fatal("acceptance requires a dedicated Cluster with exactly the requested node")
	}
	node := nodes.Items[0]
	if cluster.Spec.Runtime != "Raw" || node.Spec.Runtime != "Raw" || cluster.Spec.Cluster != node.Spec.Cluster || tenant.IDOf(&cluster) != tenantID || tenant.IDOf(&node) != tenantID || node.Spec.Environment == "" || len(node.Spec.AuthorizedGPUUUIDs) != 1 {
		t.Fatal("exact Raw cluster/node/tenant binding and one authorized GPU required")
	}
	if !node.Status.Schedulable || node.Status.Busy || node.Status.LastHeartbeatTime == nil || time.Since(node.Status.LastHeartbeatTime.Time) > 45*time.Second || !meta.IsStatusConditionTrue(node.Status.Conditions, "Ready") {
		t.Fatal("a fresh idle enrolled node is required")
	}
	var locks ext.ContainerAllocationList
	if err := c.List(ctx, &locks, client.InNamespace(namespace)); err != nil || len(locks.Items) != 0 {
		t.Fatalf("dedicated namespace must have no allocations: %v", err)
	}
	// Every run checks real CUDA computation; distinct Job/Task/allocation UIDs
	// prove reuse rather than replaying a retained result from the first run.
	gpu := `import os,sys,torch; assert torch.cuda.device_count()==1; assert os.environ["CUDA_VISIBLE_DEVICES"].startswith("GPU-"); assert torch.tensor([42.0],device="cuda").item()==42.0; print("RLARK_GPU_SMOKE_OK",sys.prefix,flush=True)`
	cases := []struct {
		name, code string
		want       native.JobPhase
		stop       bool
	}{
		{"success", gpu, native.JobPhaseSucceeded, false},
		{"reuse", gpu, native.JobPhaseSucceeded, false},
		{"failure", gpu + `; sys.exit(7)`, native.JobPhaseFailed, false},
		{"stop", gpu + `; import time; time.sleep(600)`, native.JobPhaseStopped, true},
	}
	for _, test := range cases {
		if !t.Run(test.name, func(t *testing.T) {
			name := "raw-accept-" + test.name + "-" + uuid.NewString()[:8]
			params, err := json.Marshal(map[string]any{"argv": []string{executable, "-u", "-c", test.code}, "directory": directory, "env": []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}})
			if err != nil {
				t.Fatal(err)
			}
			job := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{tenant.IDLabel: tenantID}, Annotations: map[string]string{nativejob.ControllerOwnerAnnotation: ext.Group}}, Spec: native.JobSpec{Tasks: []native.JobTaskTemplate{{Name: "worker", TaskSpec: native.TaskSpec{Role: "worker", Placement: native.TaskPlacementSpec{Environment: node.Spec.Environment}, Runtime: native.TaskRuntimeSpec{Kind: "Raw"}, Workload: native.TaskWorkloadSpec{Kind: "raw.rlark.io/process", Version: "v1", Carrier: native.TaskWorkloadCarrier{Type: "raw.rlark.io/process", Version: "v1"}, Parameters: apiextensions.JSON{Raw: params}}}}}}}
			if err := c.Create(ctx, job); err != nil {
				t.Fatal(err)
			}
			t.Logf("Job %s UID=%s (retained on failure for explicit recovery)", job.Name, job.UID)
			binding := &ext.ContainerJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{tenant.IDLabel: tenantID}}, Spec: ext.ContainerJobSpec{Cluster: cluster.Spec.Cluster, Job: ext.NativeObjectReference{Name: job.Name, UID: string(job.UID)}, TenantID: tenantID, Runtime: "Raw"}}
			if err := c.Create(ctx, binding); err != nil {
				t.Fatal(err)
			}
			poll := func(description string, check func(context.Context) (bool, error)) {
				t.Helper()
				if err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, check); err != nil {
					t.Fatalf("%s: %v; inspect retained Job %s", description, err, job.Name)
				}
			}
			readJob := func(ctx context.Context) error {
				uid := job.UID
				if err := c.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
					return err
				}
				if job.UID != uid {
					t.Fatal("Job identity replaced during acceptance")
				}
				return nil
			}
			if test.stop {
				poll("running before stop", func(ctx context.Context) (bool, error) {
					err := readJob(ctx)
					return job.Status.Phase == native.JobPhaseRunning && job.Status.ObservedGeneration == job.Generation, err
				})
				before := job.DeepCopy()
				job.Spec.Suspended = true
				if err := c.Patch(ctx, job, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					t.Fatal(err)
				}
			}
			poll("terminal result and cleanup", func(ctx context.Context) (bool, error) {
				if err := readJob(ctx); err != nil {
					return false, err
				}
				if job.Status.Phase != test.want || job.Status.ObservedGeneration != job.Generation {
					return false, nil
				}
				var allocation ext.ContainerAllocation
				err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: companion.Name(node.Spec.Node.UID)}, &allocation)
				return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
			})
			if err := c.Delete(ctx, job, client.Preconditions{UID: &job.UID, ResourceVersion: &job.ResourceVersion}); err != nil {
				t.Fatal(err)
			}
			poll("owned Job and Task deletion", func(ctx context.Context) (bool, error) {
				if err := c.Get(ctx, client.ObjectKeyFromObject(job), &native.Job{}); !apierrors.IsNotFound(err) {
					return false, err
				}
				var task native.Task
				err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: job.Name + "-worker"}, &task)
				return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
			})
			// The binding controller may patch status while the Job is being
			// cleaned. Refresh its current resourceVersion before the exact-UID
			// delete so a stale response cannot turn a successful run into a
			// false cleanup failure.
			poll("exact binding deletion", func(ctx context.Context) (bool, error) {
				var current ext.ContainerJob
				if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &current); err != nil {
					return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
				}
				if current.UID != binding.UID {
					t.Fatal("binding identity replaced during acceptance")
				}
				err := c.Delete(ctx, &current, client.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion})
				if apierrors.IsConflict(err) {
					return false, nil
				}
				return false, client.IgnoreNotFound(err) // Observe absence next time.
			})
		}) {
			return // Never dispatch another Job after uncertain cleanup.
		}
	}
}
