package release

import (
	"errors"
	"os"
	"testing"

	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestPrepareMemberLeaseMigratesExistingReleaseRole(t *testing.T) {
	ctx := t.Context()
	namespace := clusterupgrade.Namespace
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "rlark-release", Namespace: namespace},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"update"}}},
	}
	member := fake.NewClientset(role)
	resource := schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}
	roleUpdated := false
	member.PrependReactor("update", "roles", func(_ ktesting.Action) (bool, runtime.Object, error) {
		roleUpdated = true
		return false, nil, nil
	})
	for _, verb := range []string{"create", "get"} {
		member.PrependReactor(verb, "leases", func(_ ktesting.Action) (bool, runtime.Object, error) {
			if !roleUpdated {
				return true, nil, apierrors.NewForbidden(resource, clusterupgrade.LeaseName, errors.New("old release role"))
			}
			return false, nil, nil
		})
	}
	if err := prepareMemberLease(ctx, member); err != nil {
		t.Fatal(err)
	}
	if err := prepareMemberLease(ctx, member); err != nil {
		t.Fatal(err)
	}
	updated, err := member.RbacV1().Roles(namespace).Get(ctx, role.Name, metav1.GetOptions{})
	if err != nil || len(updated.Rules) != 4 || updated.Rules[0].Resources[0] != "deployments" {
		t.Fatalf("unexpected member role: %+v, %v", updated, err)
	}
	leases := member.CoordinationV1().Leases(namespace)
	guard, work, err := clusterupgrade.Acquire(ctx, leases, "rlarkctl-release/test", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = guard.Release(ctx) }()
	if err := guard.Check(work); err != nil {
		t.Fatal(err)
	}
}

// Reproduce the first release on an existing member using the old delegated
// release identity, rather than the envtest administrator for the migration.
func TestBootstrapMemberLeaseWithLegacyKubernetesRBAC(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for the isolated Kubernetes API test")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx := t.Context()
	admin, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	namespace, account := clusterupgrade.Namespace, "rlark-release"
	if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rbac := admin.RbacV1()
	subject := rbacv1.Subject{Kind: "ServiceAccount", Namespace: namespace, Name: account}
	rule := rbacv1.PolicyRule{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"roles"}, Verbs: []string{"get", "update", "escalate"}}
	if _, err := rbac.ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: account}, Rules: []rbacv1.PolicyRule{rule}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rbac.ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: account}, Subjects: []rbacv1.Subject{subject},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: account},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rbac.Roles(namespace).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: account}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rbac.RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: account}, Subjects: []rbacv1.Subject{subject},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: account},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	memberConfig := rest.CopyConfig(config)
	memberConfig.Impersonate.UserName = "system:serviceaccount:" + namespace + ":" + account
	member, err := kubernetes.NewForConfig(memberConfig)
	if err != nil {
		t.Fatal(err)
	}
	leases := member.CoordinationV1().Leases(namespace)
	if _, err := leases.Get(ctx, clusterupgrade.LeaseName, metav1.GetOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("legacy writer should start without Lease permissions: %v", err)
	}
	if err := prepareMemberLease(ctx, member); err != nil {
		t.Fatal(err)
	}
	guard, _, err := clusterupgrade.Acquire(ctx, leases, "rlarkctl-release/test", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyReleaseTreeRetainsMemberFenceRules(t *testing.T) {
	definitions, err := ParseDefinitions(releaseMemberRBAC, []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: rlark-release
  namespace: rlark-system
rules:
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["update"]
`))
	if err != nil {
		t.Fatal(err)
	}
	var parsed rbacv1.Role
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(definitions[0].Object, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Rules) != 4 || parsed.Rules[0].Resources[0] != "deployments" || parsed.Rules[1].ResourceNames[0] != clusterupgrade.LeaseName || parsed.Rules[3].Resources[0] != "jobs" {
		t.Fatalf("unexpected rules: %+v", parsed.Rules)
	}
}
