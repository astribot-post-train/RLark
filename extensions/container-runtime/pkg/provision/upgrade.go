package provision

import (
	"context"
	"errors"
	"reflect"
	"strings"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UpgradeRequest creates one explicit operation in the Cluster's exclusive
// upgrade slot. Release identity and source version are caller-retained intent,
// so retries cannot silently change a partially committed operation.
type UpgradeRequest struct {
	OperationID string                    `json:"operationID"`
	Node        NodeIdentity              `json:"node"`
	Release     ext.NativeObjectReference `json:"release"`
	SHA256      string                    `json:"sha256"`
	FromVersion string                    `json:"fromVersion"`
}

// CreateUpgrade records intent only. The opt-in controller reserves the shared
// node lock later; this command neither downloads nor installs Agent binaries.
func (s Service) CreateUpgrade(ctx context.Context, req UpgradeRequest) (*ext.AgentUpgrade, error) {
	if s.Client == nil {
		return nil, errors.New("management client required")
	}
	if err := validateOperation(req.OperationID, req.Node.TenantID, req.Node.Name); err != nil {
		return nil, err
	}
	if req.Release.Name == "" || req.Release.UID == "" || req.Release.Namespace != req.Node.Namespace || strings.TrimSpace(req.FromVersion) == "" || len(req.FromVersion) > 128 || strings.ContainsAny(req.FromVersion, "\x00\r\n") {
		return nil, errors.New("exact namespaced release and bounded source Agent version required")
	}
	node, err := s.ReadNode(ctx, req.Node)
	if err != nil {
		return nil, err
	}
	if node.Spec.Runtime != "Raw" {
		return nil, errors.New("upgrade reservations currently support Raw nodes")
	}
	_, cc, err := s.cluster(ctx, node.Spec.Cluster.Name, node.Spec.Cluster.UID, req.Node.TenantID)
	if err != nil {
		return nil, err
	}
	var release ext.AgentRelease
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: req.Release.Namespace, Name: req.Release.Name}, &release); err != nil {
		return nil, err
	}
	if ref(&release) != req.Release || cc.Spec.TargetAgentRelease != release.Name || release.Spec.SHA256 != req.SHA256 || tenant.IDOf(&release) != req.Node.TenantID || !release.DeletionTimestamp.IsZero() || !release.Status.Available || release.Status.ObservedGeneration != release.Generation || artifact.ValidateSpec(release.Spec) != nil {
		return nil, ErrConflict
	}
	intent, err := fingerprint(req)
	if err != nil {
		return nil, err
	}
	wanted := &ext.AgentUpgrade{ObjectMeta: metadata(companion.UpgradeName(node.Spec.Cluster.UID), node.Namespace, req.Node.TenantID, req.OperationID, intent), Spec: ext.AgentUpgradeSpec{Cluster: node.Spec.Cluster, ClusterRecord: ref(cc), NodeRecord: ref(node), Release: ref(&release), ReleaseSpec: release.Spec, TenantID: req.Node.TenantID, InstanceID: node.Spec.InstanceID, CredentialID: req.Node.CredentialID, FromVersion: req.FromVersion}}
	wanted.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cc, ext.GroupVersion.WithKind("ContainerCluster"))}
	existing := &ext.AgentUpgrade{}
	if err := s.ensureCreate(ctx, wanted, existing, func() bool {
		return reflect.DeepEqual(existing.Spec, wanted.Spec) && reflect.DeepEqual(existing.OwnerReferences, wanted.OwnerReferences)
	}); err != nil {
		return nil, err
	}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(wanted), wanted); err != nil {
		return nil, err
	}
	if wanted.UID == "" {
		return nil, errors.New("management API did not assign AgentUpgrade UID")
	}
	// Fresh authorization after creation also catches a lost/slow Create whose
	// credentials or target were replaced while the result was uncertain.
	if _, err := s.ReadNode(ctx, req.Node); err != nil {
		return nil, err
	}
	_, currentCC, err := s.cluster(ctx, node.Spec.Cluster.Name, node.Spec.Cluster.UID, req.Node.TenantID)
	if err != nil {
		return nil, err
	}
	if ref(currentCC) != ref(cc) || currentCC.Spec.TargetAgentRelease != release.Name {
		return nil, ErrConflict
	}
	var currentRelease ext.AgentRelease
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(&release), &currentRelease); err != nil {
		return nil, err
	}
	if ref(&currentRelease) != ref(&release) || currentRelease.Spec != release.Spec || tenant.IDOf(&currentRelease) != req.Node.TenantID || !currentRelease.DeletionTimestamp.IsZero() || !currentRelease.Status.Available || currentRelease.Status.ObservedGeneration != currentRelease.Generation {
		return nil, ErrConflict
	}
	return wanted, nil
}
