package clusterupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rlinf/rlark/apps/rlark/astribot/nodeagent"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// NodeImageGrantAnnotation is reserved for the management plane. It stores a
// grant and its direction on the same object as the image it authorizes, so
// the image and the spent grant share a single resourceVersion.
const NodeImageGrantAnnotation = "rlark.io/node-image-upgrade-grant"

type NodeImageGrant struct {
	ClusterUID, DaemonSetUID, ChildUID types.UID
	Before, Target, ExecutorImage      string
	Deployment                         Images
	Expires                            time.Time
	Phase                              NodeImagePhase
}

type NodeImagePhase string

const (
	NodeImageApproved   NodeImagePhase = "Approved"
	NodeImageApplied    NodeImagePhase = "Applied"
	NodeImageRolledBack NodeImagePhase = "RolledBack"
)

type NodeImageAction string

const (
	ReadNodeImage      NodeImageAction = "read"
	ApplyNodeImage     NodeImageAction = "apply"
	RestoreNodeImage   NodeImageAction = "rollback"
	ObserveNodeRollout NodeImageAction = "observe"
)

var ErrNodeImageGrant = errors.New("no matching active node Agent image grant")

// NodeImageBroker is the management-side authority for node Agent image changes.
// Member must return a verifier client for exactly the requested membership.
// It is never returned to, or constructed from credentials provided by, the Job.
type NodeImageBroker struct {
	Clusters dynamic.ResourceInterface
	Member   func(context.Context, string, types.UID) (kubernetes.Interface, error)
}

type NodeImageResult struct {
	ClusterUID types.UID      `json:"clusterUID"`
	Before     string         `json:"before"`
	Target     string         `json:"target"`
	Phase      NodeImagePhase `json:"phase"`
	Deployment Images         `json:"deployment"`
	Ready      bool           `json:"ready"`
}

// Execute re-reads and authorizes every operation. The worker supplies identity
// and direction only. The approved images never come from a worker request.
func (b NodeImageBroker) Execute(ctx context.Context, cluster string, identity ExecutorIdentity, token string, action NodeImageAction) (NodeImageResult, error) {
	if action != ApplyNodeImage && action != RestoreNodeImage {
		return b.execute(ctx, cluster, identity, token, action)
	}
	var result NodeImageResult
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error
		result, err = b.execute(ctx, cluster, identity, token, action)
		return err
	})
	return result, err
}

func (b NodeImageBroker) execute(ctx context.Context, cluster string, identity ExecutorIdentity, token string, action NodeImageAction) (NodeImageResult, error) {
	if b.Clusters == nil || b.Member == nil || cluster == "" || identity.ChildUID == "" {
		return NodeImageResult{}, ErrNodeImageGrant
	}
	object, err := b.Clusters.Get(ctx, cluster, metav1.GetOptions{})
	if err != nil {
		return NodeImageResult{}, err
	}
	grant, err := currentNodeImageGrant(object, identity)
	if err != nil {
		return NodeImageResult{}, err
	}
	member, err := b.Member(ctx, cluster, grant.ClusterUID)
	if err != nil {
		return NodeImageResult{}, err
	}
	if err := VerifyExecutor(ctx, member, token, grant.ExecutorImage, identity); err != nil {
		return NodeImageResult{}, err
	}
	daemonSet, err := member.AppsV1().DaemonSets(Namespace).Get(ctx, nodeagent.Name, metav1.GetOptions{})
	if err != nil {
		return NodeImageResult{}, err
	}
	if !ownsNodeDaemonSet(daemonSet, grant) {
		return NodeImageResult{}, nodeagent.ErrImageDeclarationChanged
	}
	result := NodeImageResult{ClusterUID: grant.ClusterUID, Before: grant.Before, Target: grant.Target, Phase: grant.Phase, Deployment: grant.Deployment}
	switch action {
	case ReadNodeImage:
		return result, nil
	case ObserveNodeRollout:
		result.Ready = observeNodeImage(daemonSet, grant)
		return result, nil
	case ApplyNodeImage, RestoreNodeImage:
		return result, b.change(ctx, object, grant, action)
	default:
		return NodeImageResult{}, ErrNodeImageGrant
	}
}

func currentNodeImageGrant(cluster *unstructured.Unstructured, identity ExecutorIdentity) (NodeImageGrant, error) {
	var grant NodeImageGrant
	if cluster.GetDeletionTimestamp() != nil || cluster.GetUID() == "" || cluster.GetResourceVersion() == "" ||
		cluster.GetAnnotations()[DriverAnnotation] != DriverImageOnly ||
		json.Unmarshal([]byte(cluster.GetAnnotations()[NodeImageGrantAnnotation]), &grant) != nil ||
		grant.ClusterUID != cluster.GetUID() || grant.ChildUID != types.UID(identity.ChildUID) ||
		grant.DaemonSetUID == "" || grant.Before == "" || !time.Now().Before(grant.Expires) {
		return NodeImageGrant{}, ErrNodeImageGrant
	}
	for _, image := range []string{grant.Before, grant.Target, grant.Deployment.Agent, grant.Deployment.Tools} {
		if _, err := pinnedImage(image); err != nil {
			return NodeImageGrant{}, ErrNodeImageGrant
		}
	}
	var expected string
	switch grant.Phase {
	case NodeImageApproved, NodeImageRolledBack:
		expected = grant.Before
	case NodeImageApplied:
		expected = grant.Target
	default:
		return NodeImageGrant{}, ErrNodeImageGrant
	}

	enabled, _, boolErr := unstructured.NestedBool(cluster.Object, "spec", "nodeAgents", "enabled")
	image, _, strErr := unstructured.NestedString(cluster.Object, "spec", "nodeAgents", "image")
	if boolErr != nil || strErr != nil || !enabled || image != expected {
		return NodeImageGrant{}, nodeagent.ErrImageDeclarationChanged
	}
	return grant, nil
}

func (b NodeImageBroker) change(ctx context.Context, object *unstructured.Unstructured, grant NodeImageGrant, action NodeImageAction) error {
	if !time.Now().Before(grant.Expires) {
		return ErrNodeImageGrant
	}
	var image string
	if action == ApplyNodeImage {
		if grant.Phase == NodeImageRolledBack {
			return ErrNodeImageGrant
		}
		if grant.Phase == NodeImageApplied {
			return nil
		}
		grant.Phase, image = NodeImageApplied, grant.Target
	} else {
		if grant.Phase == NodeImageRolledBack {
			return nil
		}
		grant.Phase, image = NodeImageRolledBack, grant.Before
	}
	updated := object.DeepCopy()
	if err := unstructured.SetNestedField(updated.Object, image, "spec", "nodeAgents", "image"); err != nil {
		return err
	}
	payload, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("encode node Agent upgrade grant: %w", err)
	}
	annotations := updated.GetAnnotations()
	annotations[NodeImageGrantAnnotation] = string(payload)
	updated.SetAnnotations(annotations)
	// A late apply cannot race a rollback. Their grant and image updates use
	// the same Cluster resourceVersion, even if the rollback is a no-op.
	_, err = b.Clusters.Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

func ownsNodeDaemonSet(ds *appsv1.DaemonSet, grant NodeImageGrant) bool {
	return ds.UID == grant.DaemonSetUID && ds.DeletionTimestamp == nil && ds.Labels[nodeagent.ManagedByLabel] == nodeagent.ManagedBy
}

func observeNodeImage(ds *appsv1.DaemonSet, grant NodeImageGrant) bool {
	image := grant.Target
	if grant.Phase == NodeImageRolledBack {
		image = grant.Before
	} else if grant.Phase != NodeImageApplied {
		return false
	}
	status := ds.Status
	return slices.ContainsFunc(ds.Spec.Template.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == nodeagent.Name && c.Image == image
	}) && status.ObservedGeneration >= ds.Generation && status.DesiredNumberScheduled > 0 &&
		status.UpdatedNumberScheduled == status.DesiredNumberScheduled &&
		status.NumberAvailable == status.DesiredNumberScheduled && status.NumberMisscheduled == 0
}
