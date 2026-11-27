package clusterupgradejob

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/base"
)

type blockedStateStore struct {
	t *testing.T
}

func (b blockedStateStore) Get(context.Context) (*ClusterState, error) {
	b.t.Fatal("image-only attempt read the stock state")
	return nil, nil
}

func (b blockedStateStore) Save(context.Context, *ClusterState) error {
	b.t.Fatal("image-only attempt wrote the stock state")
	return nil
}

func TestImageOnlyAttemptDoesNotUseStockState(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, appsv1.AddToScheme, rlark.AddToScheme} {
		require.NoError(t, register(scheme))
	}
	image := "registry.example/rlark-upgrade@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	child := &rlark.ClusterUpgradeJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "lab", Name: "isolated-upgrade", UID: "attempt-uid"},
		Spec: rlark.ClusterUpgradeJobSpec{
			Image: image, AutoRollback: true,
			ConfigPatch: "apiVersion: rlark.io/v1alpha1\nkind: DeployConfig\nplane: data\nkubernetes:\n  agent-image: " + image + "\n  image: " + image + "\n",
		},
	}
	management := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(child).WithObjects(child).Build()
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&batchv1.Job{}).Build()
	driver := NewK8sDriver(kube, "lab")
	driver.policy = &clusterupgrade.JobPolicy{Deployment: "agent", ExecutorImage: image}
	driver.stateStore = blockedStateStore{t}
	run := &pullRun{
		pullReconciler: &pullReconciler{c: NewClusterUpgradeJobController(base.Controller{ManagementClient: management})},
		driver:         driver, log: logr.Discard(),
	}
	_, err := run.startUpgrade(t.Context(), child)
	require.NoError(t, err)
	require.Equal(t, rlark.ClusterUpgradeJobUpgrading, child.Status.Phase)
	var job batchv1.Job
	require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: StateStoreNamespace, Name: child.Status.JobName}, &job))
	// A stale or unrelated Job must not be accepted through a reused name.
	unowned := child.DeepCopy()
	unowned.UID = "different-attempt"
	foreign, err := driver.CheckJobResult(t.Context(), unowned)
	require.NoError(t, err)
	require.Equal(t, JobStatusFailed, foreign.Status)
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}
	require.NoError(t, kube.Status().Update(t.Context(), &job))
	_, err = run.monitorUpgrade(t.Context(), child)
	require.NoError(t, err)
	require.Equal(t, rlark.ClusterUpgradeJobSucceeded, child.Status.Phase)
}
