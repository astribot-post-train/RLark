package release

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestInitializeStockPolicyFromRelease(t *testing.T) {
	ctx := t.Context()
	s := newFakeSite(t)
	_, environment, release := writeFiles(t)
	desired := environment.Desired(release, testClusters)
	desired.StockExecutor = newRLark
	lab := desired.Clusters["lab"]
	lab.Agent = oldRLark
	desired.Clusters["lab"] = lab
	run := newRun(t, s, environment, desired, nil)
	run.Tree.stockUpgradeBootstrap = true
	clusters := s.management.Resource(clusterGVR)
	stock, err := clusters.Get(ctx, "lab", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stock.SetUID("stock-member")
	stock.SetAnnotations(map[string]string{clusterupgrade.DriverAnnotation: clusterupgrade.DriverStock})
	delete(stock.Object["spec"].(map[string]any), "nodeAgents")
	if err := s.management.Tracker().Update(clusterGVR, stock, ""); err != nil {
		t.Fatal(err)
	}
	work, releaseMembers, err := run.holdMembers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMembers()
	cm := s.members["lab"].CoreV1().ConfigMaps(agentNamespace)
	if _, err := cm.Create(ctx, &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterupgrade.StockStateName, Namespace: agentNamespace}, Data: map[string]string{"image": "preserve-baseline"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Name: "unlocked", Namespace: agentNamespace, Labels: map[string]string{"rlark.io/cluster-upgrade-job": "old"}}}
	if _, err := s.members["lab"].BatchV1().Jobs(agentNamespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := run.initializeStockMember(work, "lab"); err == nil {
		t.Fatal("cutover accepted an active old Job")
	}
	if _, err := cm.Get(ctx, clusterupgrade.PolicyConfigMap, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("wrote policy before the cutover: %v", err)
	}
	if err := s.members["lab"].BatchV1().Jobs(agentNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if changes, err := run.changes(ctx, stockUpgradeStage, State{}); err != nil || len(changes) == 0 {
		t.Fatalf("missing dry-run changes: %v %v", changes, err)
	}
	for range 2 {
		if err := run.act(work, stockUpgradeStage, State{}); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := cm.Get(ctx, clusterupgrade.PolicyConfigMap, metav1.GetOptions{})
	if err != nil || policy.Annotations[stockPolicyOwner] != "stock-member" || policy.Data["executorImage"] != desired.StockExecutor {
		t.Fatalf("unexpected stock policy %v %v", policy, err)
	}
	state, err := cm.Get(ctx, clusterupgrade.StockStateName, metav1.GetOptions{})
	if err != nil || state.Data["image"] != "preserve-baseline" {
		t.Fatalf("baseline changed %v %v", state, err)
	}
	if err := run.initializeStockMember(work, "office"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.members["office"].CoreV1().ConfigMaps(agentNamespace).Get(ctx, clusterupgrade.PolicyConfigMap, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("initialized unapproved member: %v", err)
	}
	verifyStockRBACUpgrade(t, run, work)
	stock.SetUID("replacement")
	if err := s.management.Tracker().Update(clusterGVR, stock, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.initializeStockMember(work, "lab"); err == nil {
		t.Fatal("replacement adopted a managed policy")
	}
}

func verifyStockRBACUpgrade(t *testing.T, run *Run, ctx context.Context) {
	t.Helper()
	defs, err := ParseDefinitions("stock_member_rbac.yaml", stockMemberRBAC)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range defs {
		client := definitionClient(run.Site.MemberDefinitions["lab"], definition)
		current, err := client.Get(ctx, definition.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		current.SetUID(types.UID("original-" + definition.GetKind()))
		current.SetResourceVersion("5")
		current.SetLabels(map[string]string{"existing": "preserved"})
		field := "rules"
		if definition.GetKind() == "RoleBinding" {
			field = "subjects"
		}
		current.Object[field] = []any{}
		if _, err := client.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	changes, err := run.stockChanges(ctx, "lab")
	if err != nil || len(changes) != 2 {
		t.Fatalf("expected two RBAC updates: %v, %v", changes, err)
	}
	if err := run.initializeStockMember(ctx, "lab"); err != nil {
		t.Fatal(err)
	}
	for _, definition := range defs {
		client := definitionClient(run.Site.MemberDefinitions["lab"], definition)
		current, err := client.Get(ctx, definition.GetName(), metav1.GetOptions{})
		if err != nil || current.GetUID() != types.UID("original-"+definition.GetKind()) || current.GetLabels()["existing"] != "preserved" ||
			len(differences(jsonValue(current.Object), jsonValue(stated(definition)), false)) != 0 {
			t.Fatalf("RBAC was not updated in place: %v, %v", current, err)
		}
	}
	role := defs[0]
	client := definitionClient(run.Site.MemberDefinitions["lab"], role)
	current, err := client.Get(ctx, role.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.SetAnnotations(nil)
	current.Object["rules"] = []any{}
	if _, err := client.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := run.stockChanges(ctx, "lab"); err == nil {
		t.Fatal("plan proposed overwriting manual RBAC")
	}
	if err := run.initializeStockMember(ctx, "lab"); err == nil {
		t.Fatal("apply overwrote manual RBAC")
	}
}

func TestStockStageDefersOnlyMemberWithActiveUpgrade(t *testing.T) {
	ctx := t.Context()
	s := newFakeSite(t)
	_, environment, release := writeFiles(t)
	desired := environment.Desired(release, testClusters)
	desired.StockExecutor = newRLark
	for _, name := range []string{"lab", "office"} {
		decl := desired.Clusters[name]
		decl.Agent = oldRLark
		desired.Clusters[name] = decl
		cluster, err := s.management.Resource(clusterGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cluster.SetUID("stock-" + types.UID(name))
		cluster.SetAnnotations(map[string]string{clusterupgrade.DriverAnnotation: clusterupgrade.DriverStock})
		delete(cluster.Object["spec"].(map[string]any), "nodeAgents")
		if err := s.management.Tracker().Update(clusterGVR, cluster, ""); err != nil {
			t.Fatal(err)
		}
	}
	var log bytes.Buffer
	run := newRun(t, s, environment, desired, &log)
	run.log = logTo{out: &log}
	run.Tree.stockUpgradeBootstrap = true
	work, releaseMembers, err := run.holdMembers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMembers()
	jobs := s.members["lab"].BatchV1().Jobs(agentNamespace)
	pending := &batch.Job{ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: agentNamespace, Labels: map[string]string{"rlark.io/cluster-upgrade-job": "old"}}}
	if _, err := jobs.Create(ctx, pending, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	stage := Stage{Name: stockUpgradeStage}
	changes, err := run.changes(work, stage.Name, State{})
	if err != nil || !slices.ContainsFunc(changes, func(change Change) bool {
		return strings.Contains(change.What, "lab") && strings.Contains(change.DeferredReason, "pending")
	}) {
		t.Fatalf("preview did not report deferred member: %v %v", changes, err)
	}
	run.stockDeferred = map[string]string{}
	if err := run.stage(work, stage, Snapshot{}); err != nil {
		t.Fatalf("busy Job blocked the stage: %v", err)
	}
	if !strings.Contains(log.String(), "upgrade Job pending is still active") {
		t.Fatal(log.String())
	}
	policy := func(name string) error {
		_, err := s.members[name].CoreV1().ConfigMaps(agentNamespace).Get(ctx, clusterupgrade.PolicyConfigMap, metav1.GetOptions{})
		return err
	}
	if err := policy("lab"); !apierrors.IsNotFound(err) {
		t.Fatalf("busy member should be untouched: %v", err)
	}
	if err := policy("office"); err != nil {
		t.Fatalf("other member did not converge: %v", err)
	}
	if err := jobs.Delete(ctx, pending.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if reading, err := run.settled(work, stage, Snapshot{}); err != nil || reading != "" {
		t.Fatalf("deferred member restarted the readback: %q %v", reading, err)
	}
	run.stockDeferred = map[string]string{}
	if err := run.stage(work, stage, Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if err := policy("lab"); err != nil {
		t.Fatalf("next run did not retry the deferred member: %v", err)
	}
}
