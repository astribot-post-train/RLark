package release

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

//go:embed stock_member_rbac.yaml
var stockMemberRBAC []byte

const stockUpgradeStage = "stock-upgrades"

const stockMemberRBACPath = "apps/rlark/astribot/release/stock_member_rbac.yaml"

const stockPolicyOwner = "rlark.io/stock-policy-cluster-uid"

type activeStockUpgrade struct{ job string }

func (e *activeStockUpgrade) Error() string {
	return fmt.Sprintf("upgrade Job %s is still active", e.job)
}

func (r *Run) deferStockMember(name, reason string) Change {
	if r.stockDeferred != nil {
		r.stockDeferred[name] = reason
	}
	return Change{Stage: stockUpgradeStage, What: name + " stock upgrade initialization", DeferredReason: reason}
}

func (r *Run) stockMember(ctx context.Context, name string) (*rlark.Cluster, error) {
	if r.Desired.StockExecutor == "" || r.Tree == nil || !r.Tree.stockUpgradeBootstrap || r.Desired.Clusters[name].Agent == "" {
		return nil, nil
	}
	object, err := r.Site.Management.Resource(clusterGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if object.GetAnnotations()[clusterupgrade.DriverAnnotation] != clusterupgrade.DriverStock {
		return nil, nil
	}
	var cluster rlark.Cluster
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &cluster); err != nil {
		return nil, err
	}
	if cluster.UID == "" || cluster.DeletionTimestamp != nil {
		return nil, fmt.Errorf("stock member %s has no current Cluster identity", name)
	}
	if err := clusterupgrade.ValidateStockTarget(cluster); err != nil {
		return nil, err
	}
	if err := clusterupgrade.ValidateExecutorImage(r.Desired.StockExecutor); err != nil {
		return nil, err
	}
	return &cluster, nil
}

func (r *Run) stockChanges(ctx context.Context, name string) ([]Change, error) {
	if reason := r.stockDeferred[name]; reason != "" {
		return []Change{r.deferStockMember(name, reason)}, nil
	}
	cluster, err := r.stockMember(ctx, name)
	if err != nil || cluster == nil {
		return nil, err
	}
	member := r.Site.Members[name]
	configs := member.CoreV1().ConfigMaps(agentNamespace)
	policy, err := configs.Get(ctx, clusterupgrade.PolicyConfigMap, metav1.GetOptions{})
	if apierrors.IsForbidden(err) {
		return []Change{{Stage: stockUpgradeStage, What: name + " stock member upgrade bootstrap", To: r.Desired.StockExecutor}}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	var changes []Change
	if policy == nil || apierrors.IsNotFound(err) {
		changes = append(changes, Change{Stage: stockUpgradeStage, What: name + " stock executor policy", To: r.Desired.StockExecutor})
	} else {
		if err := checkStockPolicy(policy, cluster); err != nil {
			return nil, err
		}
		if policy.Annotations[stockPolicyOwner] != "" {
			changed(stockUpgradeStage, name+" stock executor policy", policy.Data["executorImage"], r.Desired.StockExecutor, &changes)
		}
	}
	_, err = configs.Get(ctx, clusterupgrade.StockStateName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		changes = append(changes, Change{Stage: stockUpgradeStage, What: name + " stock rollback state", To: "initialized"})
	} else if err != nil {
		return nil, err
	}
	defs, err := ParseDefinitions("stock_member_rbac.yaml", stockMemberRBAC)
	if err != nil {
		return nil, err
	}
	roles, err := stockRBACChanges(ctx, r.Site.MemberDefinitions[name], defs, cluster.UID)
	if err != nil {
		return nil, err
	}
	for _, role := range roles {
		changes = append(changes, Change{Stage: stockUpgradeStage, What: name + " " + definitionName(role.object), To: "as in " + short(r.Tree.Commit)})
	}
	if len(changes) > 0 {
		var busy *activeStockUpgrade
		if err := checkNoActiveStockJob(ctx, member); errors.As(err, &busy) {
			return []Change{r.deferStockMember(name, busy.Error())}, nil
		} else if err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func checkStockPolicy(policy *core.ConfigMap, cluster *rlark.Cluster) error {
	owner := policy.Annotations[stockPolicyOwner]
	if policy.Data["mode"] != clusterupgrade.DriverStock || (owner != "" && owner != string(cluster.UID)) || policy.DeletionTimestamp != nil {
		return fmt.Errorf("member %s has an incompatible or differently owned upgrade policy", cluster.Name)
	}
	return clusterupgrade.ValidateExecutorImage(policy.Data["executorImage"])
}

func (r *Run) initializeStockMember(ctx context.Context, name string) error {
	cluster, err := r.stockMember(ctx, name)
	if err != nil || cluster == nil {
		return err
	}
	check := func(ctx context.Context) error {
		live, err := r.stockMember(ctx, name)
		if err != nil {
			return err
		}
		if live == nil || live.UID != cluster.UID {
			return fmt.Errorf("stock member %s changed during initialization", name)
		}
		return r.checkMemberLease(ctx, name)
	}
	member := r.Site.Members[name]

	configs := member.CoreV1().ConfigMaps(agentNamespace)
	policy, err := configs.Get(ctx, clusterupgrade.PolicyConfigMap, metav1.GetOptions{})
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return err
	}
	if !missing {
		if err := checkStockPolicy(policy, cluster); err != nil {
			return err
		}
	}
	defs, err := ParseDefinitions("stock_member_rbac.yaml", stockMemberRBAC)
	if err != nil {
		return err
	}
	plane := r.Site.MemberDefinitions[name]
	roles, err := stockRBACChanges(ctx, plane, defs, cluster.UID)
	if err != nil {
		return err
	}
	_, err = configs.Get(ctx, clusterupgrade.StockStateName, metav1.GetOptions{})
	stateMissing := apierrors.IsNotFound(err)
	if err != nil && !stateMissing {
		return err
	}
	policyChanged := missing || (policy.Annotations[stockPolicyOwner] != "" && policy.Data["executorImage"] != r.Desired.StockExecutor)
	if len(roles) == 0 && !stateMissing && !policyChanged {
		return nil
	}
	deployment, _, err := clusterAgent(ctx, member)
	if err != nil {
		return err
	}
	if reading, err := deploymentSettled(ctx, member, agentNamespace, deployment.Name, r.Desired.Clusters[name].Agent); err != nil || reading != "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("cluster Agent must settle before installing stock upgrade policy: %s", reading)
	}
	if err := checkNoActiveStockJob(ctx, member); err != nil {
		return err
	}
	for _, change := range roles {
		definition := change.object
		if err := check(ctx); err != nil {
			return err
		}
		client := definitionClient(plane, definition)
		if change.create {
			_, err = client.Create(ctx, definition, metav1.CreateOptions{})
		} else {
			_, err = client.Update(ctx, definition, metav1.UpdateOptions{})
		}
		if err != nil {
			return err
		}
	}
	if stateMissing {
		if err := check(ctx); err != nil {
			return err
		}
		if _, err := configs.Create(ctx, &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterupgrade.StockStateName, Namespace: agentNamespace}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	if missing {
		if err := check(ctx); err != nil {
			return err
		}
		_, err = configs.Create(ctx, &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterupgrade.PolicyConfigMap, Namespace: agentNamespace, Annotations: map[string]string{stockPolicyOwner: string(cluster.UID)}}, Data: map[string]string{"mode": clusterupgrade.DriverStock, "executorImage": r.Desired.StockExecutor}}, metav1.CreateOptions{})
		return err
	}
	if policy.Annotations[stockPolicyOwner] == "" || policy.Data["executorImage"] == r.Desired.StockExecutor {
		return nil
	}
	if err := check(ctx); err != nil {
		return err
	}
	policy.Data["executorImage"] = r.Desired.StockExecutor
	_, err = configs.Update(ctx, policy, metav1.UpdateOptions{})
	return err
}

func checkNoActiveStockJob(ctx context.Context, member kubernetes.Interface) error {
	jobs, err := member.BatchV1().Jobs(agentNamespace).List(ctx, metav1.ListOptions{LabelSelector: "rlark.io/cluster-upgrade-job"})
	if err != nil {
		return err
	}
	for _, job := range jobs.Items {
		if !slices.ContainsFunc(job.Status.Conditions, func(c batch.JobCondition) bool {
			return c.Status == core.ConditionTrue && (c.Type == batch.JobComplete || c.Type == batch.JobFailed)
		}) {
			return &activeStockUpgrade{job: job.Name}
		}
	}
	return nil
}
