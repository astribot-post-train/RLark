package clusterupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodeagent"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrBatchAuthority = errors.New("member can write upgrade approval resources or its permissions could not be verified")
var ErrManagedBaseline = errors.New("node Agent baseline has not converged")

type BatchGrantIssuer struct {
	Management client.Reader
	// ApprovedExecutor comes from the broker's operator-controlled inventory.
	ApprovedExecutor         func(cluster string, uid types.UID) (string, bool)
	CheckControllerOwnership func(ctx context.Context, namespace, childName, parentName string) error
}

// approve derives an immutable grant from the approved parent, its owned child
// and the live member baseline. The caller has already acquired the member lease.
func (i BatchGrantIssuer) approve(ctx context.Context, object *unstructured.Unstructured, member kubernetes.Interface, identity ExecutorIdentity, token string) (NodeImageGrant, error) {
	if identity.ChildName == "" || i.Management == nil || i.ApprovedExecutor == nil || i.CheckControllerOwnership == nil {
		return NodeImageGrant{}, ErrNodeImageGrant
	}
	var cluster rlark.Cluster
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &cluster); err != nil {
		return NodeImageGrant{}, err
	}
	pinned, ok := i.ApprovedExecutor(cluster.Name, cluster.UID)
	if !ok {
		return NodeImageGrant{}, ErrNodeImageGrant
	}
	// Authenticate before resolving parent or disclosing member baseline.
	if err := VerifyExecutor(ctx, member, token, pinned, identity); err != nil {
		return NodeImageGrant{}, err
	}
	var child rlark.ClusterUpgradeJob
	if err := i.Management.Get(ctx, client.ObjectKey{Namespace: cluster.Name, Name: identity.ChildName}, &child); err != nil {
		return NodeImageGrant{}, err
	}
	owner := metav1.GetControllerOf(&child)
	if child.UID != types.UID(identity.ChildUID) || child.DeletionTimestamp != nil || child.Spec.Image != pinned ||
		child.Annotations[ChildClusterUIDAnnotation] != string(cluster.UID) || child.Labels["rlark.io/cluster"] != cluster.Name ||
		owner == nil || owner.UID == "" || owner.Kind != "UpgradeJob" || owner.APIVersion != rlark.GroupVersion.String() ||
		child.Spec.UpgradeJobName != owner.Name || child.Name != owner.Name+"-"+cluster.Name {
		return NodeImageGrant{}, ErrNodeImageGrant
	}
	if err := i.CheckControllerOwnership(ctx, cluster.Name, child.Name, owner.Name); err != nil {
		return NodeImageGrant{}, err
	}
	var parent rlark.UpgradeJob
	if err := i.Management.Get(ctx, client.ObjectKey{Name: owner.Name}, &parent); err != nil {
		return NodeImageGrant{}, err
	}
	if parent.UID != owner.UID || parent.DeletionTimestamp != nil || parent.Status.CompletionTime != nil ||
		!batchSelects(parent.Spec, cluster) {
		return NodeImageGrant{}, ErrNodeImageGrant
	}
	plan, err := ParseBatchPlan(parent.Spec)
	if err != nil {
		return NodeImageGrant{}, ErrNodeImageGrant
	}
	target, err := plan.ForCluster(cluster)
	if err != nil || !target.Managed || target.ExecutorImage != child.Spec.Image || target.DeployConfig != child.Spec.ConfigPatch ||
		parent.Spec.Strategy.AutoRollback != child.Spec.AutoRollback || parent.Spec.Strategy.TimeoutSeconds != child.Spec.TimeoutSeconds {
		return NodeImageGrant{}, ErrNodeImageGrant
	}
	deployment, nodeImage, err := parseImagePlan(target.DeployConfig, true)
	if err != nil {
		return NodeImageGrant{}, ErrNodeImageGrant
	}
	if _, err := pinnedImage(cluster.Spec.NodeAgents.Image); err != nil {
		return NodeImageGrant{}, fmt.Errorf("node Agent baseline image must be pinned by digest")
	}
	ds, err := member.AppsV1().DaemonSets(Namespace).Get(ctx, nodeagent.Name, metav1.GetOptions{})
	if err != nil {
		return NodeImageGrant{}, err
	}
	baseline := NodeImageGrant{DaemonSetUID: ds.UID, Phase: NodeImageApplied, Target: cluster.Spec.NodeAgents.Image}
	if ds.UID == "" || !ownsNodeDaemonSet(ds, baseline) || !observeNodeImage(ds, baseline) {
		return NodeImageGrant{}, ErrManagedBaseline
	}
	lifetime, err := managedGrantTTL(child.Spec.TimeoutSeconds)
	if err != nil {
		return NodeImageGrant{}, err
	}
	return NodeImageGrant{
		ClusterUID: cluster.UID, DaemonSetUID: ds.UID, ChildUID: child.UID,
		Before: cluster.Spec.NodeAgents.Image, Target: nodeImage, ExecutorImage: pinned, Deployment: deployment,
		Phase: NodeImageApproved, Expires: time.Now().Add(lifetime),
	}, nil
}

func batchSelects(spec rlark.UpgradeJobSpec, cluster rlark.Cluster) bool {
	if len(spec.ClusterNames) > 0 {
		return slices.Contains(spec.ClusterNames, cluster.Name)
	}
	if spec.ClusterSelector == nil {
		return true
	}
	selector, err := metav1.LabelSelectorAsSelector(spec.ClusterSelector)
	return err == nil && selector.Matches(labels.Set(cluster.Labels))
}

func mayIssueGrant(object *unstructured.Unstructured, identity ExecutorIdentity) bool {
	existing := object.GetAnnotations()[NodeImageGrantAnnotation]
	if existing == "" {
		return true
	}
	var previous NodeImageGrant
	return json.Unmarshal([]byte(existing), &previous) == nil && previous.ChildUID != "" && previous.ChildUID != types.UID(identity.ChildUID)
}
