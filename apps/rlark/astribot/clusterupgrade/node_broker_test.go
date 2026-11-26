package clusterupgrade

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodeagent"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	runtimefake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const brokerDigest = "@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

func TestNodeImageBrokerScopesWritesAndRejectsForwardReplay(t *testing.T) {
	ctx := t.Context()
	id := ExecutorIdentity{ChildUID: "attempt", PodName: "executor", PodUID: "executor-pod"}
	grant := NodeImageGrant{
		ClusterUID: "lab-uid", DaemonSetUID: "ds-uid", ChildUID: types.UID(id.ChildUID),
		Before: "registry.example/node-old" + brokerDigest, Target: "registry.example/node-new" + brokerDigest,
		Deployment:    Images{Agent: "registry.example/agent" + brokerDigest, Tools: "registry.example/tools" + brokerDigest},
		ExecutorImage: "registry.example/upgrade" + brokerDigest, Expires: time.Now().Add(time.Hour), Phase: NodeImageApproved,
	}
	data, err := json.Marshal(grant)
	require.NoError(t, err)
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rlinf.io/v1alpha1", "kind": "Cluster",
		"metadata": map[string]any{"name": "lab", "uid": string(grant.ClusterUID), "resourceVersion": "1"},
		"spec":     map[string]any{"nodeAgents": map[string]any{"enabled": true, "image": grant.Before, "nodeSelector": map[string]any{"robot": "true"}}},
	}}
	cluster.SetAnnotations(map[string]string{DriverAnnotation: DriverImageOnly, NodeImageGrantAnnotation: string(data), "unrelated": "preserve"})
	gvr := schema.GroupVersionResource{Group: "rlinf.io", Version: "v1alpha1", Resource: "clusters"}
	other := cluster.DeepCopy()
	other.SetName("other")
	other.SetUID("other-uid")
	clusters := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), cluster, other).Resource(gvr)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: JobName(id.ChildUID), UID: "local-job", Labels: map[string]string{ChildUIDLabel: id.ChildUID}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: id.PodName, UID: types.UID(id.PodUID),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
		Spec:   corev1.PodSpec{ServiceAccountName: JobServiceAccount, Containers: []corev1.Container{{Name: "upgrade", Image: grant.ExecutorImage}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	now := metav1.NewMicroTime(time.Now())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: Namespace, Annotations: map[string]string{executorAnnotation: id.PodName + "/" + id.PodUID}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To("upgrade-job/" + id.ChildUID), RenewTime: &now, LeaseDurationSeconds: ptr.To(int32(300))},
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: nodeagent.Name, UID: grant.DaemonSetUID, Generation: 2,
			Labels: map[string]string{nodeagent.ManagedByLabel: nodeagent.ManagedBy}},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: nodeagent.Name, Image: grant.Before}},
		}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 2, UpdatedNumberScheduled: 1, NumberAvailable: 2},
	}
	member := fake.NewClientset(job, pod, lease, ds)
	member.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authv1.TokenReview)
		require.Equal(t, []string{BrokerAudience}, review.Spec.Audiences)
		review.Status = authv1.TokenReviewStatus{
			Authenticated: review.Spec.Token == "bound-token" || review.Spec.Token == "unbound-token",
			Audiences:     []string{BrokerAudience},
			User:          authv1.UserInfo{Username: "system:serviceaccount:" + Namespace + ":" + JobServiceAccount},
		}
		if review.Spec.Token == "bound-token" {
			review.Status.User.Extra = map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {id.PodName}, "authentication.kubernetes.io/pod-uid": {id.PodUID},
			}
		}
		return true, review, nil
	})
	broker := NodeImageBroker{Clusters: clusters, Member: func(_ context.Context, name string, uid types.UID) (kubernetes.Interface, error) {
		require.Equal(t, cluster.GetName(), name)
		require.Equal(t, cluster.GetUID(), uid)
		return member, nil
	}}
	server := httptest.NewTLSServer(broker)
	t.Cleanup(server.Close)
	newClient := func(cluster, token string) *BrokerNodeImages {
		adapter, err := NewBrokerNodeImages(server.URL, cluster, grant.Target, grant.Deployment, id, func() (string, error) { return token, nil }, server.Client())
		require.NoError(t, err)
		return adapter
	}

	_, err = newClient("other", "bound-token").Read(ctx)
	require.ErrorIs(t, err, ErrNodeImageGrant)
	_, err = newClient("lab", "unbound-token").Read(ctx)
	require.ErrorIs(t, err, ErrUnverifiedExecutor)

	client := newClient("lab", "bound-token")
	snap, err := client.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, NodeAgentTarget{ClusterUID: string(grant.ClusterUID), Before: grant.Before}, snap)
	require.ErrorIs(t, client.CompareAndSet(ctx, snap, grant.Before, "unapproved", false), ErrNodeImageGrant)
	require.NoError(t, client.CompareAndSet(ctx, snap, grant.Before, grant.Target, false))
	require.NoError(t, client.CompareAndSet(ctx, snap, grant.Before, grant.Target, false))

	observed, err := client.call(ctx, ObserveNodeRollout)
	require.NoError(t, err)
	require.False(t, observed.Ready, "an incomplete rollout cannot be certified")
	ds.Spec.Template.Spec.Containers[0].Image = grant.Target
	_, err = member.AppsV1().DaemonSets(Namespace).Update(ctx, ds, metav1.UpdateOptions{})
	require.NoError(t, err)
	ds.Status.UpdatedNumberScheduled = ds.Status.DesiredNumberScheduled
	_, err = member.AppsV1().DaemonSets(Namespace).UpdateStatus(ctx, ds, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, client.WaitReady(ctx, snap, grant.Target))

	require.NoError(t, client.CompareAndSet(ctx, snap, grant.Target, grant.Before, true))
	require.ErrorIs(t, client.CompareAndSet(ctx, snap, grant.Before, grant.Target, false), ErrNodeImageGrant)
	current, err := clusters.Get(ctx, "lab", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, cluster.Object["spec"], current.Object["spec"], "rollback preserves Cluster fields")
	require.Equal(t, "preserve", current.GetAnnotations()["unrelated"])
	replayGrant, err := currentNodeImageGrant(current, id)
	require.NoError(t, err)
	require.Equal(t, NodeImageRolledBack, replayGrant.Phase)

	lease.Spec.HolderIdentity = ptr.To("another-attempt")
	_, err = member.CoordinationV1().Leases(Namespace).Update(ctx, lease, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = client.call(ctx, ReadNodeImage)
	require.ErrorIs(t, err, ErrTargetChanged)

	// Exercise automatic issuance with the same live verifier fixture and
	// a management child whose spec must agree with the owned parent.
	id.ChildName = "batch-lab"
	lease.Spec.HolderIdentity = ptr.To("upgrade-job/" + id.ChildUID)
	_, err = member.CoordinationV1().Leases(Namespace).Update(ctx, lease, metav1.UpdateOptions{})
	require.NoError(t, err)
	current, err = clusters.Get(ctx, "lab", metav1.GetOptions{})
	require.NoError(t, err)
	annotations := current.GetAnnotations()
	delete(annotations, NodeImageGrantAnnotation)
	annotations[ManagedBatchAnnotation] = "enabled"
	current.SetAnnotations(annotations)
	require.NoError(t, unstructured.SetNestedField(current.Object, "lab", "spec", "agentNamespace"))
	_, err = clusters.Update(ctx, current, metav1.UpdateOptions{})
	require.NoError(t, err)
	ds.Spec.Template.Spec.Containers[0].Image = grant.Before
	_, err = member.AppsV1().DaemonSets(Namespace).Update(ctx, ds, metav1.UpdateOptions{})
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, rlark.AddToScheme(scheme))
	deployConfig := "apiVersion: rlark.io/v1alpha1\nkind: DeployConfig\nplane: data\nkubernetes:\n  agent-image: " +
		grant.Deployment.Agent + "\n  image: " + grant.Deployment.Tools + "\n  node-agent-image: " + grant.Target + "\n"
	parent := &rlark.UpgradeJob{
		ObjectMeta: metav1.ObjectMeta{Name: "batch", UID: "real-parent"},
		Spec: rlark.UpgradeJobSpec{Image: "stock-upgrader", ClusterNames: []string{"lab"},
			ConfigPatch: "apiVersion: astribot.rlark.io/v1alpha1\nkind: BatchDeployConfig\nmanaged:\n  executorImage: " + grant.ExecutorImage +
				"\n  deployConfig: |\n    " + strings.ReplaceAll(strings.TrimSuffix(deployConfig, "\n"), "\n", "\n    ") + "\n",
			Strategy: rlark.UpgradeStrategy{TimeoutSeconds: 120, AutoRollback: true}},
	}
	child := &rlark.ClusterUpgradeJob{
		ObjectMeta: metav1.ObjectMeta{Name: id.ChildName, Namespace: "lab", UID: types.UID(id.ChildUID),
			Annotations:     map[string]string{ChildClusterUIDAnnotation: string(grant.ClusterUID)},
			Labels:          map[string]string{"rlark.io/cluster": "lab"},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, rlark.GroupVersion.WithKind("UpgradeJob"))}},
		Spec: rlark.ClusterUpgradeJobSpec{UpgradeJobName: parent.Name, Image: grant.ExecutorImage, ConfigPatch: deployConfig + "# edited child\n", TimeoutSeconds: 120, AutoRollback: true},
	}
	management := runtimefake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, child).Build()
	issuer := BatchGrantIssuer{
		Management:       management,
		ApprovedExecutor: func(string, types.UID) (string, bool) { return grant.ExecutorImage, true },
		CheckControllerOwnership: func(_ context.Context, namespace, childName, parentName string) error {
			require.Equal(t, "lab", namespace)
			require.Equal(t, child.Name, childName)
			require.Equal(t, parent.Name, parentName)
			return nil
		},
	}
	broker.Issuer = &issuer
	_, err = broker.Execute(ctx, "lab", id, "bound-token", ReadNodeImage)
	require.ErrorIs(t, err, ErrNodeImageGrant, "member cannot change the approved child spec")
	require.NoError(t, management.Get(ctx, runtimeclient.ObjectKeyFromObject(child), child))
	child.Spec.ConfigPatch = deployConfig
	require.NoError(t, management.Update(ctx, child))

	result, err := broker.Execute(ctx, "lab", id, "bound-token", ReadNodeImage)
	require.NoError(t, err)
	require.Equal(t, NodeImageApproved, result.Phase)
	require.Equal(t, grant.Before, result.Before)
	persisted, err := clusters.Get(ctx, "lab", metav1.GetOptions{})
	require.NoError(t, err)
	record, err := currentNodeImageGrant(persisted, id)
	require.NoError(t, err)
	require.Equal(t, grant.DaemonSetUID, record.DaemonSetUID)
	require.Equal(t, grant.Target, record.Target)
}

func TestVerifyManagedCompletionRequiresAppliedAttemptAndSameReadyDaemonSet(t *testing.T) {
	images := Images{Agent: "registry.example/agent" + brokerDigest, Tools: "registry.example/tools" + brokerDigest}
	grant := NodeImageGrant{ClusterUID: "cluster", DaemonSetUID: "daemonset", ChildUID: "child",
		Before: "registry.example/old" + brokerDigest, Target: "registry.example/node" + brokerDigest,
		ExecutorImage: "registry.example/upgrade" + brokerDigest, Expires: time.Now().Add(-time.Hour), Phase: NodeImageApplied,
		Deployment: images}
	encoded, err := json.Marshal(grant)
	require.NoError(t, err)
	cluster := &rlark.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lab", UID: grant.ClusterUID, ResourceVersion: "19",
			Annotations: map[string]string{DriverAnnotation: DriverImageOnly, NodeImageGrantAnnotation: string(encoded)}},
		Spec: rlark.ClusterSpec{NodeAgents: &rlark.ClusterNodeAgents{Enabled: true, Image: grant.Target}},
	}
	daemon := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{UID: grant.DaemonSetUID, Generation: 3, Labels: map[string]string{nodeagent.ManagedByLabel: nodeagent.ManagedBy}},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: nodeagent.Name, Image: grant.Target},
		}}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 3, DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberAvailable: 1},
	}
	job := &rlark.ClusterUpgradeJob{ObjectMeta: metav1.ObjectMeta{UID: grant.ChildUID},
		Spec: rlark.ClusterUpgradeJobSpec{Image: grant.ExecutorImage, ConfigPatch: "apiVersion: rlark.io/v1alpha1\nkind: DeployConfig\nplane: data\nkubernetes:\n  agent-image: " +
			images.Agent + "\n  image: " + images.Tools + "\n  node-agent-image: " + grant.Target + "\n"}}
	checkpoint := Checkpoint{Target: images, NodeImage: grant.Target,
		NodeAgent: NodeAgentTarget{ClusterUID: string(grant.ClusterUID), Before: grant.Before}, Completed: true}
	require.True(t, VerifyManagedNodeCompletion(cluster, daemon, job, checkpoint), "expired grant should still provide read-only evidence")
	replaced := daemon.DeepCopy()
	replaced.UID = "replacement"
	require.False(t, VerifyManagedNodeCompletion(cluster, replaced, job, checkpoint))
	stale := daemon.DeepCopy()
	stale.Status.ObservedGeneration--
	require.False(t, VerifyManagedNodeCompletion(cluster, stale, job, checkpoint))
	modified := cluster.DeepCopy()
	modified.Spec.NodeAgents.Image = grant.Before
	require.False(t, VerifyManagedNodeCompletion(modified, daemon, job, checkpoint))
	newAttempt := job.DeepCopy()
	newAttempt.UID = "different-attempt"
	require.False(t, VerifyManagedNodeCompletion(cluster, daemon, newAttempt, checkpoint))
}
