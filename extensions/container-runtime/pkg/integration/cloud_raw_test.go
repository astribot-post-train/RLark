package integration

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
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
	logs := cloudLogAcceptance(t)
	checkpoints := os.Getenv("RLARK_TEST_RAW_CHECKPOINTS") == "true"
	if checkpoints && logs == nil {
		t.Fatal("RLARK_TEST_RAW_CHECKPOINTS=true requires the authenticated log acceptance inputs")
	}
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
		{"failure", gpu, native.JobPhaseFailed, false},
		{"stop", gpu, native.JobPhaseStopped, true},
	}
	checkpointFiles := map[string]string{}
	retained := map[string]string{}
	if checkpoints {
		for _, name := range []string{"success", "reuse", "failure", "stop"} {
			checkpointFiles[name] = directory + "/.rlark-checkpoint-" + uuid.NewString() + "-" + name + ".json"
		}
		// The verifier is a fifth, ordinary Job. It runs only after all four
		// lifecycle cases have cleaned their allocations, and is the sole process
		// allowed to remove these test-owned files.
		verify := checkpointVerifyCode(checkpointFiles, true)
		cases = append(cases, struct {
			name, code string
			want       native.JobPhase
			stop       bool
		}{"checkpoint-verify", verify, native.JobPhaseSucceeded, false})
	}
	for _, test := range cases {
		if !t.Run(test.name, func(t *testing.T) {
			if checkpoints && test.name != "checkpoint-verify" {
				path, ok := checkpointFiles[test.name]
				if !ok {
					t.Fatalf("missing checkpoint path for %s", test.name)
				}
				test.code = checkpointVerifyCode(retained, false) + "\n" + test.code + "\n" + checkpointWriteCode(path, test.name)
			}
			if test.name == "failure" {
				test.code += `; sys.exit(7)`
			}
			if test.stop {
				if logs != nil {
					test.code += `; print("X"*70000,flush=True)`
				}
				test.code += `; import time; time.sleep(600)`
			}
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
			var logTarget string
			if test.stop {
				poll("running before stop", func(ctx context.Context) (bool, error) {
					err := readJob(ctx)
					return job.Status.Phase == native.JobPhaseRunning && job.Status.ObservedGeneration == job.Generation, err
				})
				if logs != nil {
					var a ext.ContainerAllocation
					if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: companion.Name(node.Spec.Node.UID)}, &a); err != nil {
						t.Fatal(err)
					}
					if a.Spec.Job.UID != string(job.UID) || a.Spec.Task.UID == "" {
						t.Fatal("log acceptance requires the exact current Job/Task allocation")
					}
					logTarget = logs.checkRunning(t, namespace, &a, checkpoints)
				}
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
			if logTarget != "" {
				logs.checkReleased(t, logTarget)
			}
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
			if checkpoints && test.name != "checkpoint-verify" {
				retained[test.name] = checkpointFiles[test.name]
			}
		}) {
			return // Never dispatch another Job after uncertain cleanup.
		}
	}
}

// TestCloudRawRollout is deliberately opt-in: it upgrades one already enrolled,
// dedicated idle Raw node and leaves the target release configured for the
// operator's follow-up workload acceptance. It never runs against a shared
// cluster and does not force-delete uncertain upgrade state.
func TestCloudRawRollout(t *testing.T) {
	kubeconfig := os.Getenv("RLARK_TEST_RAW_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set RLARK_TEST_RAW_KUBECONFIG and the documented dedicated cloud rollout inputs")
	}
	required := func(key string) string {
		t.Helper()
		value := os.Getenv(key)
		if value == "" {
			t.Fatalf("%s is required for real Agent rollout acceptance", key)
		}
		return value
	}
	namespace := required("RLARK_TEST_RAW_NAMESPACE")
	clusterName := required("RLARK_TEST_RAW_CLUSTER")
	nodeName := required("RLARK_TEST_RAW_NODE")
	tenantID := required("RLARK_TEST_RAW_TENANT")
	releaseName := required("RLARK_TEST_RAW_RELEASE")
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
	if cluster.Spec.TargetAgentRelease != "" {
		t.Fatalf("rollout acceptance requires no existing target, got %q", cluster.Spec.TargetAgentRelease)
	}
	var release ext.AgentRelease
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: releaseName}, &release); err != nil {
		t.Fatal(err)
	}
	if release.UID == "" || !release.DeletionTimestamp.IsZero() || release.Status.ObservedGeneration != release.Generation || !release.Status.Available || tenant.IDOf(&release) != tenantID || artifact.ValidateSpec(release.Spec) != nil {
		t.Fatal("target AgentRelease is not current, available and tenant-bound")
	}
	var nodes ext.ContainerNodeList
	if err := c.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 1 || nodes.Items[0].Name != nodeName {
		t.Fatal("rollout acceptance requires exactly the requested dedicated node")
	}
	node := nodes.Items[0]
	oldVersion := node.Status.AgentVersion
	if cluster.UID == "" || cluster.Spec.Runtime != "Raw" || node.Spec.Runtime != "Raw" || node.Spec.Cluster != cluster.Spec.Cluster || tenant.IDOf(&cluster) != tenantID || tenant.IDOf(&node) != tenantID || len(node.Spec.AuthorizedGPUUUIDs) != 1 || !cluster.DeletionTimestamp.IsZero() || !node.DeletionTimestamp.IsZero() {
		t.Fatal("exact live Raw cluster/node/tenant bindings and one authorized GPU are required")
	}
	currentCondition := func(conditions []metav1.Condition, generation int64, name string) bool {
		condition := meta.FindStatusCondition(conditions, name)
		return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
	}
	if oldVersion == "" || oldVersion == release.Spec.Version || !node.Status.Schedulable || node.Status.Busy || node.Status.LastHeartbeatTime == nil || time.Since(node.Status.LastHeartbeatTime.Time) > 45*time.Second || node.Status.ObservedGeneration != node.Generation || !currentCondition(node.Status.Conditions, node.Generation, "Ready") || !currentCondition(node.Status.Conditions, node.Generation, "Validated") || !currentCondition(node.Status.Conditions, node.Generation, "AgentReleaseReady") || !slices.Contains(node.Status.Capabilities, ext.AgentUpgradeCapability) || !slices.Contains(node.Status.Capabilities, "agent-platform/"+release.Spec.OS+"/"+release.Spec.Architecture) {
		t.Fatal("a fresh idle upgrade-capable node is required")
	}
	var allocations ext.ContainerAllocationList
	if err := c.List(ctx, &allocations, client.InNamespace(namespace)); err != nil || len(allocations.Items) != 0 {
		t.Fatalf("dedicated rollout namespace must have no allocations: %v", err)
	}
	var upgrades ext.AgentUpgradeList
	if err := c.List(ctx, &upgrades, client.InNamespace(namespace)); err != nil || len(upgrades.Items) != 0 {
		t.Fatalf("dedicated rollout namespace must have no prior upgrade operations: %v", err)
	}
	before := cluster.DeepCopy()
	cluster.Spec.TargetAgentRelease = release.Name
	if err := c.Patch(ctx, &cluster, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatal(err)
	}
	poll := func(description string, check func(context.Context) (bool, error)) {
		t.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 8*time.Minute, true, check); err != nil {
			t.Fatalf("%s: %v; inspect retained rollout operation", description, err)
		}
	}
	poll("target Agent version and fresh idle observation", func(ctx context.Context) (bool, error) {
		var current ext.ContainerNode
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeName}, &current); err != nil {
			return false, err
		}
		if current.UID != node.UID || current.Spec.InstanceID != node.Spec.InstanceID || current.Spec.Node != node.Spec.Node || current.Annotations[ext.AgentCredentialAnnotation] != node.Annotations[ext.AgentCredentialAnnotation] {
			t.Fatal("ContainerNode identity replaced during rollout")
		}
		return current.Status.AgentVersion == release.Spec.Version && current.Status.Schedulable && !current.Status.Busy && current.Status.LastHeartbeatTime != nil && time.Since(current.Status.LastHeartbeatTime.Time) <= 45*time.Second && currentCondition(current.Status.Conditions, current.Generation, "AgentReleaseReady"), nil
	})
	poll("cluster rollout readiness and exact receipt", func(ctx context.Context) (bool, error) {
		var current ext.ContainerCluster
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterName}, &current); err != nil {
			return false, err
		}
		if current.UID != cluster.UID || current.Generation != cluster.Generation || current.Spec.TargetAgentRelease != release.Name {
			t.Fatal("Cluster identity or desired target changed during rollout")
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, "AgentRolloutReady")
		ready := condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == current.Generation
		receipt := current.Status.LastAgentUpgrade
		return ready && receipt != nil && receipt.Operation.UID != "" && receipt.Release.Name == release.Name && receipt.Release.Namespace == namespace && receipt.Release.UID == string(release.UID) && receipt.SHA256 == release.Spec.SHA256 && receipt.NodeRecord.Name == node.Name && receipt.NodeRecord.Namespace == namespace && receipt.NodeRecord.UID == string(node.UID), nil
	})
	poll("upgrade operation and allocation deletion", func(ctx context.Context) (bool, error) {
		var current ext.AgentUpgradeList
		if err := c.List(ctx, &current, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		var held ext.ContainerAllocationList
		if err := c.List(ctx, &held, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		return len(current.Items) == 0 && len(held.Items) == 0, nil
	})
	t.Logf("rollout succeeded: %s -> %s; target remains configured", oldVersion, release.Spec.Version)
}
