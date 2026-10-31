package agent

import (
	"context"
	"errors"
	"reflect"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// ErrNoRelease means the owning Cluster has no desired Agent release.
	ErrNoRelease = errors.New("no target Agent release configured")
	// ErrReleaseUnavailable means the current target is absent or not published.
	ErrReleaseUnavailable = errors.New("target Agent release unavailable")
	// ErrReleaseIdentity means the caller's release selector is stale or foreign.
	ErrReleaseIdentity = errors.New("agent release identity conflict")
)

// ReleaseRequest selects only the current target for the authenticated node.
type ReleaseRequest struct {
	Action       string `json:"action"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	ReleaseUID   string `json:"releaseUID,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
}

// ReleaseManifest is safe for an Agent to consume and contains no server path.
type ReleaseManifest struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Version      string `json:"version"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Protocol     string `json:"protocol"`
	SHA256       string `json:"sha256"`
	Bytes        int64  `json:"bytes"`
}

// Release returns a verified current target and optionally its exact bytes.
// It re-reads identity after artifact I/O so target replacement cannot publish
// bytes for a different Cluster or credential.
func (r Reporter) Release(ctx context.Context, id Identity, req ReleaseRequest, store artifact.Store) (*ReleaseManifest, []byte, error) {
	if store == nil || req.Action != "manifest" && req.Action != "download" || req.OS != "linux" || req.Architecture != "amd64" && req.Architecture != "arm64" {
		return nil, nil, ErrReleaseIdentity
	}
	if req.Action == "manifest" && (req.ReleaseUID != "" || req.SHA256 != "") || req.Action == "download" && (req.ReleaseUID == "" || req.SHA256 == "") {
		return nil, nil, ErrReleaseIdentity
	}
	node, release, err := r.targetRelease(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if release.Spec.OS != req.OS || release.Spec.Architecture != req.Architecture {
		return nil, nil, ErrReleaseIdentity
	}
	if req.Action == "download" && (req.ReleaseUID != string(release.UID) || req.SHA256 != release.Spec.SHA256) {
		return nil, nil, ErrReleaseIdentity
	}
	data, err := store.Load(ctx, release.Spec)
	if err != nil {
		return nil, nil, err
	}
	if err := artifact.Verify(release.Spec, data); err != nil {
		return nil, nil, err
	}
	manifest := &ReleaseManifest{Namespace: release.Namespace, Name: release.Name, UID: string(release.UID), Version: release.Spec.Version, OS: release.Spec.OS, Architecture: release.Spec.Architecture, Protocol: release.Spec.Protocol, SHA256: release.Spec.SHA256, Bytes: int64(len(data))}
	currentNode, current, err := r.targetRelease(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if current.UID != release.UID || current.Namespace != release.Namespace || current.Name != release.Name || !reflect.DeepEqual(current.Spec, release.Spec) || !reflect.DeepEqual(currentNode.Spec, node.Spec) {
		return nil, nil, ErrReleaseIdentity
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if req.Action == "manifest" {
		return manifest, nil, nil
	}
	return manifest, data, nil
}

func (r Reporter) targetRelease(ctx context.Context, id Identity) (*ext.ContainerNode, *ext.AgentRelease, error) {
	if r.Client == nil || id.Namespace == "" || id.Name == "" || id.UID == "" || id.CredentialID == "" || id.TenantID == "" {
		return nil, nil, ErrIdentity
	}
	var node ext.ContainerNode
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, &node); err != nil {
		return nil, nil, err
	}
	if string(node.UID) != id.UID || tenant.IDOf(&node) != id.TenantID || node.Annotations[CredentialAnnotation] != id.CredentialID || node.Spec.InstanceID != id.InstanceID || node.Spec.Cluster.UID != id.ClusterUID || !node.DeletionTimestamp.IsZero() {
		return nil, nil, ErrIdentity
	}
	if err := r.validateBindings(ctx, &node, id); err != nil {
		return nil, nil, err
	}
	var clusters ext.ContainerClusterList
	if err := r.Client.List(ctx, &clusters, client.InNamespace(node.Namespace)); err != nil {
		return nil, nil, err
	}
	var cc *ext.ContainerCluster
	for i := range clusters.Items {
		candidate := &clusters.Items[i]
		if candidate.Spec.Cluster == node.Spec.Cluster {
			if cc != nil {
				return nil, nil, ErrReleaseIdentity
			}
			cc = candidate
		}
	}
	if cc == nil {
		return nil, nil, ErrReleaseIdentity
	}
	// This is a fresh list after validateBindings. Validate the actual target
	// selector too; an intervening replacement must not inherit prior approval.
	validated := meta.FindStatusCondition(cc.Status.Conditions, "Validated")
	if cc.UID == "" || tenant.IDOf(cc) != id.TenantID || !cc.DeletionTimestamp.IsZero() || cc.Spec.Runtime != node.Spec.Runtime || cc.Spec.AgentNamespace != node.Namespace || validated == nil || validated.Status != metav1.ConditionTrue || validated.ObservedGeneration != cc.Generation || cc.Status.ObservedGeneration != cc.Generation {
		return nil, nil, ErrReleaseIdentity
	}
	if cc.Spec.TargetAgentRelease == "" {
		return nil, nil, ErrNoRelease
	}
	var release ext.AgentRelease
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cc.Namespace, Name: cc.Spec.TargetAgentRelease}, &release); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, ErrReleaseUnavailable
		}
		return nil, nil, err
	}
	if release.UID == "" || !release.DeletionTimestamp.IsZero() || !release.Status.Available || release.Status.ObservedGeneration != release.Generation || release.Spec.Protocol != "v1" {
		return nil, nil, ErrReleaseUnavailable
	}
	if tenant.IDOf(&release) != id.TenantID {
		return nil, nil, ErrReleaseIdentity
	}
	return &node, &release, nil
}
