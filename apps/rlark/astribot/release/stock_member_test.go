package release

import (
	"testing"

	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	stock.SetUID("replacement")
	if err := s.management.Tracker().Update(clusterGVR, stock, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.initializeStockMember(work, "lab"); err == nil {
		t.Fatal("replacement adopted a managed policy")
	}
}
