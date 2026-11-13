package addon

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/base"
)

func TestAddonReadiness(t *testing.T) {
	tests := []struct {
		phase      rlarkv1alpha1.AddonPhase
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{rlarkv1alpha1.AddonPhaseReady, metav1.ConditionTrue, addonReasonReady},
		{rlarkv1alpha1.AddonPhaseFailed, metav1.ConditionFalse, addonReasonFailed},
		{rlarkv1alpha1.AddonPhaseInstalling, metav1.ConditionFalse, addonReasonInstalling},
		{rlarkv1alpha1.AddonPhaseUpgrading, metav1.ConditionFalse, addonReasonUpgrading},
		{rlarkv1alpha1.AddonPhasePending, metav1.ConditionUnknown, addonReasonUnknown},
		{rlarkv1alpha1.AddonPhase("Bogus"), metav1.ConditionUnknown, addonReasonUnknown},
	}

	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			status, reason := addonReadiness(tt.phase)
			if status != tt.wantStatus || reason != tt.wantReason {
				t.Fatalf("addonReadiness(%q) = %s/%s, want %s/%s", tt.phase, status, reason, tt.wantStatus, tt.wantReason)
			}
		})
	}
}

func TestCleanupAddonOnlyDeletesCurrentInstanceAndWaits(t *testing.T) {
	ctx := context.Background()
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "current"}}
	makeConfig := func(name, uid string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "rlark-system", UID: types.UID(name + "-uid"), Labels: map[string]string{"rlark.io/addon-name": "a", "rlark.io/addon-uid": uid}}}
	}
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(makeConfig("owned", "current"), makeConfig("other", "old"), makeConfig("legacy", "")).Build()
	r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
	if err := r.cleanupAddon(ctx, addon); !errors.Is(err, errAddonCleanupPending) {
		t.Fatalf("first cleanup = %v, want pending", err)
	}
	if err := r.cleanupAddon(ctx, addon); err != nil {
		t.Fatalf("second cleanup = %v", err)
	}
	for _, name := range []string{"other", "legacy"} {
		var got corev1.ConfigMap
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: "rlark-system"}, &got); err != nil {
			t.Fatalf("unowned resource %s removed: %v", name, err)
		}
	}
	addon.UID = ""
	if err := r.cleanupAddon(ctx, addon); err == nil {
		t.Fatal("accepted missing addon UID")
	}
}

type failOnceUpdateClient struct {
	client.Client
	fail bool
}

type failOnceDeleteClient struct {
	client.Client
	fail bool
}

type relabelBeforeDeleteClient struct {
	client.Client
	once bool
}

func (c *relabelBeforeDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if !c.once {
		c.once = true
		var current corev1.ConfigMap
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &current); err != nil {
			return err
		}
		current.Labels["rlark.io/addon-uid"] = "replacement"
		if err := c.Update(ctx, &current); err != nil {
			return err
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestCleanupDoesNotDeleteRelabelledResource(t *testing.T) {
	ctx := context.Background()
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "uid"}}
	resource := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "rlark-system", UID: "object-uid", Labels: map[string]string{"rlark.io/addon-name": "a", "rlark.io/addon-uid": "uid"}}}
	cl := &relabelBeforeDeleteClient{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(resource).Build()}
	r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
	if err := r.cleanupAddon(ctx, addon); err == nil {
		t.Fatal("stale delete succeeded after ownership changed")
	}
	var got corev1.ConfigMap
	if err := cl.Get(ctx, client.ObjectKeyFromObject(resource), &got); err != nil {
		t.Fatalf("relabelled resource was deleted: %v", err)
	}
	if got.Labels["rlark.io/addon-uid"] != "replacement" {
		t.Fatal("replacement ownership lost")
	}
	if err := r.cleanupAddon(ctx, addon); err != nil {
		t.Fatalf("retry after relabel: %v", err)
	}
}

func (c *failOnceDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.fail {
		c.fail = false
		return errors.New("temporary delete failure")
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestCleanupAddonRetriesDeleteFailure(t *testing.T) {
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "uid"}}
	resource := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "rlark-system", UID: "object-uid", Labels: map[string]string{"rlark.io/addon-name": "a", "rlark.io/addon-uid": "uid"}}}
	cl := &failOnceDeleteClient{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(resource).Build(), fail: true}
	r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
	if err := r.cleanupAddon(context.Background(), addon); err == nil || errors.Is(err, errAddonCleanupPending) {
		t.Fatalf("delete failure was not returned: %v", err)
	}
	if err := r.cleanupAddon(context.Background(), addon); !errors.Is(err, errAddonCleanupPending) {
		t.Fatalf("retry = %v, want pending", err)
	}
	if err := r.cleanupAddon(context.Background(), addon); err != nil {
		t.Fatalf("cleanup after deletion = %v", err)
	}
}

func (c *failOnceUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.fail {
		c.fail = false
		return errors.New("temporary update failure")
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestApplyObjectRetriesAfterUpdateFailure(t *testing.T) {
	ctx := context.Background()
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "uid", ResourceVersion: "1"}}
	makeConfig := func(value string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "test", "namespace": "rlark-system", "labels": map[string]interface{}{"rlark.io/addon-name": "a", "rlark.io/addon-uid": "uid"}, "annotations": map[string]interface{}{AddonGenerationAnnotation: "1"}},
			"data":     map[string]interface{}{"key": value},
		}}
	}
	cl := &failOnceUpdateClient{Client: fake.NewClientBuilder().WithObjects(makeConfig("old")).Build(), fail: true}
	r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
	if err := r.applyObject(ctx, makeConfig("new"), addon); err == nil {
		t.Fatal("update failure was ignored")
	}
	if err := r.applyObject(ctx, makeConfig("new"), addon); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	got := makeConfig("old")
	if err := cl.Get(ctx, types.NamespacedName{Name: "test", Namespace: "rlark-system"}, got); err != nil {
		t.Fatal(err)
	}
	if value, _, _ := unstructured.NestedString(got.Object, "data", "key"); value != "new" {
		t.Fatalf("retry left value %q", value)
	}
}

func newAddonTestReconciler(t *testing.T, addon *rlarkv1alpha1.Addon) *pullReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rlarkv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(addon).
		WithStatusSubresource(&rlarkv1alpha1.Addon{}).
		Build()
	return &pullReconciler{c: &Controller{Controller: base.Controller{ManagementClient: cl}}}
}

func TestUpdateStatusAdvancesObservedGenerationAndWritesReady(t *testing.T) {
	addon := &rlarkv1alpha1.Addon{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Generation: 2},
		Spec:       rlarkv1alpha1.AddonSpec{Version: "v1"},
		Status: rlarkv1alpha1.AddonStatus{
			Phase:   rlarkv1alpha1.AddonPhaseReady,
			Message: "ready",
			Version: "v1",
			// Phase, message and version already converged, but the generation moved
			// on: ObservedGeneration must still advance or consumers would treat the
			// status as stale forever.
			ObservedGeneration: 1,
		},
	}
	r := newAddonTestReconciler(t, addon)

	r.updateStatus(context.Background(), addon, rlarkv1alpha1.AddonPhaseReady, "ready")

	var got rlarkv1alpha1.Addon
	if err := r.c.ManagementClient.Get(context.Background(), types.NamespacedName{Name: "a"}, &got); err != nil {
		t.Fatalf("failed to get addon: %v", err)
	}
	if got.Status.ObservedGeneration != 2 {
		t.Fatalf("observedGeneration = %d, want 2", got.Status.ObservedGeneration)
	}
	cond := findAddonCondition(got.Status.Conditions, rlarkv1alpha1.ConditionTypeReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != addonReasonReady {
		t.Fatalf("unexpected Ready condition: %#v", got.Status.Conditions)
	}
}

func TestUpdateStatusRecordsFailure(t *testing.T) {
	addon := &rlarkv1alpha1.Addon{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Generation: 1},
		Spec:       rlarkv1alpha1.AddonSpec{Version: "v1"},
	}
	r := newAddonTestReconciler(t, addon)

	r.updateStatus(context.Background(), addon, rlarkv1alpha1.AddonPhaseFailed, "render error")

	var got rlarkv1alpha1.Addon
	if err := r.c.ManagementClient.Get(context.Background(), types.NamespacedName{Name: "a"}, &got); err != nil {
		t.Fatalf("failed to get addon: %v", err)
	}
	if got.Status.Phase != rlarkv1alpha1.AddonPhaseFailed || got.Status.Message != "render error" {
		t.Fatalf("unexpected status: %#v", got.Status)
	}
	// A failed install must not publish the desired version as the observed one.
	if got.Status.Version != "" {
		t.Fatalf("failed addon reported version %q", got.Status.Version)
	}
	cond := findAddonCondition(got.Status.Conditions, rlarkv1alpha1.ConditionTypeReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != addonReasonFailed {
		t.Fatalf("unexpected Ready condition: %#v", got.Status.Conditions)
	}
	if cond.Message != "render error" {
		t.Fatalf("Ready condition message = %q, want %q", cond.Message, "render error")
	}
}

func findAddonCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func TestApplyObjectRepairsOwnedDriftAndRejectsForeignObjects(t *testing.T) {
	ctx := context.Background()
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "csi", UID: types.UID("addon-uid"), Generation: 42}}
	makeObject := func(uid string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]interface{}{"name": "driver", "namespace": "rlark-system", "labels": map[string]interface{}{"rlark.io/addon-name": "csi", "rlark.io/addon-uid": uid}, "annotations": map[string]interface{}{AddonGenerationAnnotation: "42"}},
			"spec":     map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"nodeSelector": map[string]interface{}{"kubernetes.io/hostname": "worker"}}}},
		}}
	}
	for _, tt := range []struct {
		name, uid string
		wantErr   bool
	}{{"owned", "addon-uid", false}, {"different instance", "other", true}, {"unowned", "", true}} {
		t.Run(tt.name, func(t *testing.T) {
			live := makeObject(tt.uid)
			unstructured.RemoveNestedField(live.Object, "spec", "template", "spec", "nodeSelector")
			cl := fake.NewClientBuilder().WithObjects(live).Build()
			r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
			err := r.applyObject(ctx, makeObject("addon-uid"), addon)
			if (err != nil) != tt.wantErr {
				t.Fatalf("apply: %v", err)
			}
			got := makeObject(tt.uid)
			if err := cl.Get(ctx, types.NamespacedName{Name: "driver", Namespace: "rlark-system"}, got); err != nil {
				t.Fatal(err)
			}
			_, found, err := unstructured.NestedMap(got.Object, "spec", "template", "spec", "nodeSelector")
			if err != nil || found == tt.wantErr {
				t.Fatalf("selector found=%v err=%v", found, err)
			}
			if !tt.wantErr {
				version := got.GetResourceVersion()
				if err := r.applyObject(ctx, makeObject("addon-uid"), addon); err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(ctx, types.NamespacedName{Name: "driver", Namespace: "rlark-system"}, got); err != nil {
					t.Fatal(err)
				}
				if got.GetResourceVersion() != version {
					t.Fatal("unchanged object updated again")
				}
			}
		})
	}
}

func TestDesiredFieldsMatchAllowsDefaultedFields(t *testing.T) {
	desired := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"replicas": int64(1)}}}
	existing := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"replicas": int64(1), "defaulted": "value"}}}
	if !desiredFieldsMatch(desired, existing) {
		t.Fatal("server-defaulted field considered drift")
	}
}

func TestDesiredFieldsMatchDetectsZeroValuesAndListDrift(t *testing.T) {
	for _, pair := range [][2]interface{}{{false, true}, {int64(0), int64(1)}, {"", "nonempty"}, {[]interface{}{"one"}, []interface{}{"one", "two"}}} {
		if desiredValueMatches(pair[0], pair[1]) {
			t.Errorf("accepted drift: wanted %#v, got %#v", pair[0], pair[1])
		}
	}
}

func TestSecretStringDataComparedAsStoredData(t *testing.T) {
	desired := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Secret", "stringData": map[string]interface{}{"key": "value"}}}
	existing := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Secret", "data": map[string]interface{}{"key": "dmFsdWU="}}}
	if !desiredFieldsMatch(desired, existing) {
		t.Fatal("equivalent Secret treated as drift")
	}
	existing.Object["data"] = map[string]interface{}{"key": "b3RoZXI="}
	if desiredFieldsMatch(desired, existing) {
		t.Fatal("Secret drift ignored")
	}
	if _, ok := desired.Object["stringData"]; !ok {
		t.Fatal("comparison mutated desired object")
	}
}

func TestReadyAddonSchedulesDriftReconciliation(t *testing.T) {
	ctx := context.Background()
	a := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "csi", Namespace: "test", UID: "uid", Finalizers: []string{AddonFinalizer}}, Spec: rlarkv1alpha1.AddonSpec{AddonName: "csi-driver-rclone"}}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rlarkv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a).WithStatusSubresource(&rlarkv1alpha1.Addon{}).Build()
	replicas := int32(1)
	local := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.Deployment{}, &appsv1.DaemonSet{}).WithObjects(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "rlark-system", Labels: map[string]string{"rlark.io/addon-name": "csi"}}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}}).Build()
	r := &pullReconciler{c: &Controller{Controller: base.Controller{ManagementClient: mgmt, LocalKubeClient: local, AgentType: string(rlarkv1alpha1.AgentTypeKubernetes)}}}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "csi", Namespace: "test"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := local.Get(ctx, types.NamespacedName{Name: "csi-driver-rclone-controller", Namespace: "rlark-system"}, &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Replicas, dep.Status.UpdatedReplicas, dep.Status.ReadyReplicas = *dep.Spec.Replicas, *dep.Spec.Replicas, *dep.Spec.Replicas
	if err := local.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	if err := local.Get(ctx, types.NamespacedName{Name: "csi-driver-rclone-node", Namespace: "rlark-system"}, &ds); err != nil {
		t.Fatal(err)
	}
	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled, ds.Status.UpdatedNumberScheduled, ds.Status.NumberReady = 1, 1, 1
	if err := local.Status().Update(ctx, &ds); err != nil {
		t.Fatal(err)
	}
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != AddonRequeueInterval {
		t.Fatalf("ready addon requeue = %v", result.RequeueAfter)
	}
	var got rlarkv1alpha1.Addon
	if err := mgmt.Get(ctx, types.NamespacedName{Name: "csi", Namespace: "test"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != rlarkv1alpha1.AddonPhaseReady {
		t.Fatalf("expected Ready branch, got %s: %s", got.Status.Phase, got.Status.Message)
	}
	// Simulate the next scheduled pass after an external actor removed a
	// desired field without changing the management Addon's resourceVersion.
	var config corev1.ConfigMap
	if err := local.Get(ctx, types.NamespacedName{Name: "csi-driver-rclone-config", Namespace: "rlark-system"}, &config); err != nil {
		t.Fatal(err)
	}
	config.Data = map[string]string{"unexpected": "drift"}
	if err := local.Update(ctx, &config); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := local.Get(ctx, types.NamespacedName{Name: "csi-driver-rclone-config", Namespace: "rlark-system"}, &config); err != nil {
		t.Fatal(err)
	}
	if _, exists := config.Data["unexpected"]; exists {
		t.Fatal("scheduled reconciliation left drifted config")
	}
}

func TestApplyObjectInjectsDistributedImagePullSecrets(t *testing.T) {
	ctx := context.Background()
	credential := func(name, registry string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "rlark-system", Labels: map[string]string{"rlark.io/image-registry-credential": "true"}, Annotations: map[string]string{"rlark.io/registry": registry}},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")},
		}
	}
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(credential("ir-site", "registry.rlark.internal:5443"), credential("ir-other", "other.example:5000")).Build()
	r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "u", ResourceVersion: "1"}}
	objs, err := parseManifest([]byte(`apiVersion: apps/v1
kind: DaemonSet
metadata: {name: d, namespace: rlark-system}
spec:
  selector: {matchLabels: {app: d}}
  template:
    metadata: {labels: {app: d}}
    spec:
      containers: [{name: c, image: "registry.rlark.internal:5443/fleet/robot@sha256:0000000000000000000000000000000000000000000000000000000000000000"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	injectLabels(objs[0], addon.Name, string(addon.UID))
	if err := r.applyObject(ctx, objs[0], addon); err != nil {
		t.Fatal(err)
	}
	var got appsv1.DaemonSet
	if err := cl.Get(ctx, types.NamespacedName{Name: "d", Namespace: "rlark-system"}, &got); err != nil {
		t.Fatal(err)
	}
	if secrets := got.Spec.Template.Spec.ImagePullSecrets; len(secrets) != 1 || secrets[0].Name != "ir-site" {
		t.Fatalf("imagePullSecrets = %v, want [ir-site]", secrets)
	}
}

func TestCheckReadyDaemonSetForAClassOfNodesMayHaveNone(t *testing.T) {
	ctx := context.Background()
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "u"}}
	daemonSet := func(name string, affinity *corev1.Affinity, observed int64) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "rlark-system", Generation: 1, Labels: map[string]string{"rlark.io/addon-name": "a"}},
			Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Affinity: affinity}}},
			Status:     appsv1.DaemonSetStatus{ObservedGeneration: observed},
		}
	}
	robots := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
	for _, tt := range []struct {
		name  string
		ds    *appsv1.DaemonSet
		ready bool
	}{
		{"every node, none scheduled", daemonSet("all", nil, 1), false},
		{"robot nodes, none in cluster", daemonSet("robot", robots, 1), true},
		{"robot nodes, not yet observed", daemonSet("robot", robots, 0), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(tt.ds).WithStatusSubresource(tt.ds).Build()
			r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
			ready, msg, err := r.checkReady(ctx, addon)
			if err != nil || ready != tt.ready {
				t.Fatalf("checkReady = %v %q %v, want %v", ready, msg, err, tt.ready)
			}
		})
	}
}

func TestCheckReadyDeploymentStuckRolloutIsNotReady(t *testing.T) {
	ctx := context.Background()
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "u"}}
	one := int32(1)
	// The old ReplicaSet's Pod is ready, the new one cannot pull its image.
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "rlark-system", Generation: 2, Labels: map[string]string{"rlark.io/addon-name": "a"}},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(d).WithStatusSubresource(d).Build()
	r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
	if ready, msg, err := r.checkReady(ctx, addon); err != nil || ready {
		t.Fatalf("checkReady = %v %q %v, want not ready", ready, msg, err)
	}
}

// A status write changes the management Addon's resourceVersion but not its generation. The local object,
// already rendered from that generation and unchanged, must not be written again: an annotation update
// moves a Deployment's generation, and the Addon would read as not ready on every reconciliation.
func TestApplyObjectLeavesAnUnchangedObjectAloneAcrossStatusWrites(t *testing.T) {
	ctx := context.Background()
	addon := &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "site-registry", UID: types.UID("addon-uid"), Generation: 3, ResourceVersion: "1001"}}
	makeObject := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]interface{}{"name": "site-registry", "namespace": "rlark-system", "labels": map[string]interface{}{"rlark.io/addon-name": "site-registry", "rlark.io/addon-uid": "addon-uid"}, "annotations": map[string]interface{}{AddonGenerationAnnotation: "3"}},
			"spec":     map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"hostNetwork": true}}},
		}}
	}
	cl := fake.NewClientBuilder().WithObjects(makeObject()).Build()
	r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
	before := makeObject()
	if err := cl.Get(ctx, types.NamespacedName{Name: "site-registry", Namespace: "rlark-system"}, before); err != nil {
		t.Fatal(err)
	}
	addon.ResourceVersion = "1002"
	if err := r.applyObject(ctx, makeObject(), addon); err != nil {
		t.Fatal(err)
	}
	after := makeObject()
	if err := cl.Get(ctx, types.NamespacedName{Name: "site-registry", Namespace: "rlark-system"}, after); err != nil {
		t.Fatal(err)
	}
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf("object written again after a status-only change: resourceVersion %s -> %s", before.GetResourceVersion(), after.GetResourceVersion())
	}
}

// Moving a site's cache to another machine is moving the label its Deployment selects on: the Pod on the node
// that lost the label is replaced, the one on a matching node and Pods of other workloads are left alone.
func TestReplacePodsOnUnselectedNodesFollowsTheLabel(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "site-registry", Namespace: "rlark-system", Labels: map[string]string{"rlark.io/addon-name": "site-registry"}},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "site-registry"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeSelector: map[string]string{"rlark.io/site-registry": "true"}}}},
	}
	node := func(name string, labels map[string]string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	pod := func(name, app, nodeName string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "rlark-system", UID: types.UID(name), Labels: map[string]string{"app": app}}, Spec: corev1.PodSpec{NodeName: nodeName}}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment,
		node("old", nil), node("new", map[string]string{"rlark.io/site-registry": "true"}),
		pod("cache-on-old", "site-registry", "old"), pod("cache-on-new", "site-registry", "new"), pod("other-on-old", "other", "old")).Build()
	r := &pullReconciler{c: &Controller{Controller: base.Controller{LocalKubeClient: cl}}}
	if err := r.replacePodsOnUnselectedNodes(ctx, &rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "site-registry"}}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"cache-on-old": false, "cache-on-new": true, "other-on-old": true} {
		err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: "rlark-system"}, &corev1.Pod{})
		if (err == nil) != want {
			t.Errorf("%s: present=%v, want %v", name, err == nil, want)
		}
	}
}
