// Package provision implements explicit administrator-owned container onboarding.
// It never gives management credentials or CA signing keys to a node.
package provision

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/rawnode"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OperationAnnotation proves creation by a particular administrator request.
const OperationAnnotation = ext.Group + "/provision-operation"

// ParentUIDAnnotation pins a provisioned namespace to the Cluster incarnation.
const ParentUIDAnnotation = ext.Group + "/provision-cluster-uid"

// RequestAnnotation pins partial creation to the original canonical request.
const RequestAnnotation = ext.Group + "/provision-request-sha256"

// ErrConflict means an object cannot be proven to belong to this request.
var ErrConflict = errors.New("container provisioning ownership conflict")

// ClusterRequest creates a separate native Cluster and authoritative namespace.
type ClusterRequest struct {
	OperationID    string      `json:"operationID"`
	Name           string      `json:"name"`
	TenantID       string      `json:"tenantID"`
	AgentNamespace string      `json:"agentNamespace"`
	Runtime        ext.Runtime `json:"runtime"`
}

// Validate checks cluster creation input without reading or mutating resources.
func (req ClusterRequest) Validate() error {
	if err := validateOperation(req.OperationID, req.TenantID, req.Name); err != nil {
		return err
	}
	if len(validation.IsDNS1123Label(req.AgentNamespace)) != 0 || req.AgentNamespace == "default" || strings.HasPrefix(req.AgentNamespace, "kube-") || req.AgentNamespace == "rlark-system" {
		return errors.New("a dedicated container agent namespace is required")
	}
	if req.Runtime != "Raw" && req.Runtime != "Docker" {
		return errors.New("runtime must be Raw or Docker")
	}
	return nil
}

// NodeRequest registers an explicitly authorized Raw container or Docker host.
type NodeRequest struct {
	OperationID         string   `json:"operationID"`
	Cluster             string   `json:"cluster"`
	ClusterUID          string   `json:"clusterUID"`
	ContainerClusterUID string   `json:"containerClusterUID,omitempty"`
	TenantID            string   `json:"tenantID"`
	Name                string   `json:"name"`
	InstanceID          string   `json:"instanceID"`
	PrivateAddress      string   `json:"privateAddress"`
	Environment         string   `json:"environment"`
	NetworkScope        string   `json:"networkScope"`
	AuthorizedGPUUUIDs  []string `json:"authorizedGPUUUIDs"`
}

// Validate checks node registration input without reading or mutating resources.
// Cluster and tenant identity are resolved by callers from an authoritative
// binding; they remain in the request so the provisioning fingerprint includes
// the exact parent and tenant used for the operation.
func (req NodeRequest) Validate() error {
	if err := validateOperation(req.OperationID, req.TenantID, req.Name); err != nil {
		return err
	}
	if req.Cluster == "" || req.ClusterUID == "" {
		return errors.New("exact Cluster name and UID required")
	}
	capability := &native.RawNodeStatus{Incarnation: req.InstanceID, TrainingIP: req.PrivateAddress, Environment: req.Environment, NetworkScope: req.NetworkScope, AuthorizedGPUUUIDs: req.AuthorizedGPUUUIDs}
	return rawnode.ValidateCapability(capability)
}

// Service uses authoritative reads and never adopts unmarked existing objects.
type Service struct{ Client client.Client }

func validateOperation(operation, tenantID, name string) error {
	id, err := uuid.Parse(operation)
	if err != nil || id == uuid.Nil || id.String() != operation {
		return errors.New("canonical nonzero operation UUID required")
	}
	if tenantID == "" || len(validation.IsValidLabelValue(tenantID)) != 0 {
		return errors.New("explicit valid tenant ID required")
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return errors.New("valid resource name required")
	}
	return nil
}

func fingerprint(request any) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode provisioning intent: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func metadata(name, namespace, tenantID, operation, intent string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{tenant.IDLabel: tenantID}, Annotations: map[string]string{OperationAnnotation: operation, RequestAnnotation: intent}}
}

func owned(current, wanted client.Object) bool {
	return current.GetUID() != "" && current.GetDeletionTimestamp().IsZero() && current.GetAnnotations()[OperationAnnotation] == wanted.GetAnnotations()[OperationAnnotation] && current.GetAnnotations()[RequestAnnotation] == wanted.GetAnnotations()[RequestAnnotation] && current.GetLabels()[tenant.IDLabel] == wanted.GetLabels()[tenant.IDLabel]
}

// ensureCreate recovers a committed Create whose response was lost. A later
// call resumes from the same operation ID; it never patches foreign resources.
func (s Service) ensureCreate(ctx context.Context, wanted, current client.Object, matches func() bool) error {
	err := s.Client.Get(ctx, client.ObjectKeyFromObject(wanted), current)
	if apierrors.IsNotFound(err) {
		err = s.Client.Create(ctx, wanted)
		if err == nil {
			return nil
		}
		if readErr := s.Client.Get(ctx, client.ObjectKeyFromObject(wanted), current); readErr != nil {
			return fmt.Errorf("create provisioning resource: %w", err)
		}
	} else if err != nil {
		return err
	}
	if !owned(current, wanted) || !matches() {
		return fmt.Errorf("%w: %s/%s", ErrConflict, wanted.GetNamespace(), wanted.GetName())
	}
	return nil
}

// CreateCluster creates a dedicated namespace and exact-UID companion. Repeated
// calls with the same operation recover partial creation without changing Spec.
func (s Service) CreateCluster(ctx context.Context, req ClusterRequest) (*ext.ContainerCluster, error) {
	if s.Client == nil {
		return nil, errors.New("management client required")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	intent, err := fingerprint(req)
	if err != nil {
		return nil, err
	}
	var clusters native.ClusterList
	if err := s.Client.List(ctx, &clusters); err != nil {
		return nil, err
	}
	for _, c := range clusters.Items {
		if c.Spec.AgentNamespace == req.AgentNamespace && c.Name != req.Name {
			return nil, fmt.Errorf("%w: namespace already belongs to a Cluster", ErrConflict)
		}
	}
	cluster := &native.Cluster{ObjectMeta: metadata(req.Name, "", req.TenantID, req.OperationID, intent), Spec: native.ClusterSpec{AgentNamespace: req.AgentNamespace}}
	var retainedNamespace corev1.Namespace
	if err := s.Client.Get(ctx, client.ObjectKey{Name: req.AgentNamespace}, &retainedNamespace); err == nil {
		var live native.Cluster
		if err := s.Client.Get(ctx, client.ObjectKey{Name: req.Name}, &live); err != nil {
			return nil, fmt.Errorf("%w: existing namespace lost its native Cluster instance", ErrConflict)
		}
		if !owned(&retainedNamespace, cluster) || retainedNamespace.Annotations[ParentUIDAnnotation] != string(live.UID) {
			return nil, ErrConflict
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	var bindings ext.ContainerClusterList
	if err := s.Client.List(ctx, &bindings); err != nil {
		return nil, err
	}
	for _, binding := range bindings.Items {
		if binding.Spec.Cluster.Name != req.Name {
			if binding.Namespace == req.AgentNamespace {
				return nil, fmt.Errorf("%w: namespace contains a foreign companion Cluster", ErrConflict)
			}
			continue
		}
		if binding.Name != req.Name || binding.Namespace != req.AgentNamespace || !owned(&binding, cluster) || binding.Spec.Runtime != req.Runtime || binding.Spec.AgentNamespace != req.AgentNamespace {
			return nil, fmt.Errorf("%w: conflicting companion Cluster", ErrConflict)
		}
		var live native.Cluster
		if err := s.Client.Get(ctx, client.ObjectKey{Name: req.Name}, &live); err != nil {
			return nil, fmt.Errorf("%w: existing companion Cluster lost its native instance", ErrConflict)
		}
		if ownership.MatchesNative(binding.Spec.Cluster, &live) != nil {
			return nil, ErrConflict
		}
	}
	current := &native.Cluster{}
	if err := s.ensureCreate(ctx, cluster, current, func() bool { return current.Spec.AgentNamespace == req.AgentNamespace }); err != nil {
		return nil, err
	}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		return nil, err
	}
	if cluster.UID == "" {
		return nil, errors.New("management API did not assign Cluster UID")
	}
	namespace := &corev1.Namespace{ObjectMeta: metadata(req.AgentNamespace, "", req.TenantID, req.OperationID, intent)}
	namespace.Annotations[ParentUIDAnnotation] = string(cluster.UID)
	existingNamespace := &corev1.Namespace{}
	if err := s.ensureCreate(ctx, namespace, existingNamespace, func() bool { return existingNamespace.Annotations[ParentUIDAnnotation] == string(cluster.UID) }); err != nil {
		return nil, err
	}
	cc := &ext.ContainerCluster{ObjectMeta: metadata(req.Name, req.AgentNamespace, req.TenantID, req.OperationID, intent), Spec: ext.ContainerClusterSpec{Cluster: ref(cluster), Runtime: req.Runtime, AgentNamespace: req.AgentNamespace}}
	existing := &ext.ContainerCluster{}
	if err := s.ensureCreate(ctx, cc, existing, func() bool {
		return existing.Spec.Cluster == cc.Spec.Cluster && existing.Spec.Runtime == cc.Spec.Runtime && existing.Spec.AgentNamespace == cc.Spec.AgentNamespace
	}); err != nil {
		return nil, err
	}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(cc), cc); err != nil {
		return nil, err
	}
	return cc, nil
}

func ref(o metav1.Object) ext.NativeObjectReference {
	return ext.NativeObjectReference{Name: o.GetName(), Namespace: o.GetNamespace(), UID: string(o.GetUID())}
}

func (s Service) cluster(ctx context.Context, name, uid, tenantID string) (*native.Cluster, *ext.ContainerCluster, error) {
	if s.Client == nil || uid == "" || tenantID == "" {
		return nil, nil, errors.New("management client, Cluster UID and tenant required")
	}
	var cluster native.Cluster
	if err := s.Client.Get(ctx, client.ObjectKey{Name: name}, &cluster); err != nil {
		return nil, nil, err
	}
	if string(cluster.UID) != uid || tenant.IDOf(&cluster) != tenantID || !cluster.DeletionTimestamp.IsZero() || cluster.Spec.AgentNamespace == "" {
		return nil, nil, ErrConflict
	}
	var all native.ClusterList
	if err := s.Client.List(ctx, &all); err != nil {
		return nil, nil, err
	}
	for _, other := range all.Items {
		if other.UID != cluster.UID && other.Spec.AgentNamespace == cluster.Spec.AgentNamespace {
			return nil, nil, ErrConflict
		}
	}
	var list ext.ContainerClusterList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, nil, err
	}
	var companion *ext.ContainerCluster
	for i := range list.Items {
		cc := &list.Items[i]
		if cc.Spec.Cluster.Name != cluster.Name {
			continue
		}
		if companion != nil || ownership.MatchesNative(cc.Spec.Cluster, &cluster) != nil || tenant.IDOf(cc) != tenantID || cc.Namespace != cluster.Spec.AgentNamespace || cc.Spec.AgentNamespace != cc.Namespace || !cc.DeletionTimestamp.IsZero() {
			return nil, nil, ErrConflict
		}
		companion = cc
	}
	if companion == nil || companion.UID == "" {
		return nil, nil, ErrConflict
	}
	return &cluster, companion, nil
}

// RegisterNode records operator-authorized capability. It does not report
// readiness, observe devices, issue a certificate, or uncordon existing nodes.
func (s Service) RegisterNode(ctx context.Context, req NodeRequest) (*ext.ContainerNode, error) {
	if s.Client == nil {
		return nil, errors.New("management client required")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	req.AuthorizedGPUUUIDs = slices.Clone(req.AuthorizedGPUUUIDs)
	slices.Sort(req.AuthorizedGPUUUIDs)
	intent, err := fingerprint(req)
	if err != nil {
		return nil, err
	}
	cluster, cc, err := s.cluster(ctx, req.Cluster, req.ClusterUID, req.TenantID)
	if err != nil {
		return nil, err
	}
	if req.ContainerClusterUID != "" && req.ContainerClusterUID != string(cc.UID) {
		return nil, ErrConflict
	}
	if cc.Spec.Runtime != "Raw" && cc.Spec.Runtime != "Docker" {
		return nil, errors.New("unsupported container runtime")
	}
	var retained ext.ContainerNode
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: cc.Namespace, Name: req.Name}, &retained); err == nil {
		if _, err := s.ReadNode(ctx, NodeIdentity{Namespace: retained.Namespace, Name: retained.Name, UID: string(retained.UID), TenantID: req.TenantID, CredentialID: req.OperationID}); err != nil {
			return nil, fmt.Errorf("%w: existing companion Node cannot be recovered: %w", ErrConflict, err)
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	var inventory ext.ContainerNodeList
	if err := s.Client.List(ctx, &inventory); err != nil {
		return nil, err
	}
	for _, other := range inventory.Items {
		if other.Name == req.Name && other.Namespace == cc.Namespace {
			continue
		}
		if other.Spec.NetworkScope == req.NetworkScope && other.Spec.PrivateAddress == req.PrivateAddress {
			return nil, fmt.Errorf("%w: private address already registered", ErrConflict)
		}
		for _, gpu := range other.Spec.AuthorizedGPUUUIDs {
			if slices.Contains(req.AuthorizedGPUUUIDs, gpu) {
				return nil, fmt.Errorf("%w: GPU already registered", ErrConflict)
			}
		}
	}
	agentType := native.AgentTypeRaw
	if cc.Spec.Runtime == "Docker" {
		agentType = native.AgentTypeDocker
	}
	node := &native.Node{ObjectMeta: metadata(req.Name, cc.Namespace, req.TenantID, req.OperationID, intent), Spec: native.NodeSpec{AgentType: agentType}}
	existing := &native.Node{}
	if err := s.ensureCreate(ctx, node, existing, func() bool {
		return existing.Spec.AgentType == node.Spec.AgentType && existing.Status.Reservation == nil && len(existing.Status.DockerReservations) == 0
	}); err != nil {
		return nil, err
	}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		return nil, err
	}
	if node.UID == "" {
		return nil, errors.New("management API did not assign Node UID")
	}
	cn := &ext.ContainerNode{ObjectMeta: metadata(req.Name, cc.Namespace, req.TenantID, req.OperationID, intent), Spec: ext.ContainerNodeSpec{Cluster: ref(cluster), Node: ref(node), Runtime: cc.Spec.Runtime, InstanceID: req.InstanceID, PrivateAddress: req.PrivateAddress, Environment: req.Environment, NetworkScope: req.NetworkScope, AuthorizedGPUUUIDs: slices.Clone(req.AuthorizedGPUUUIDs)}}
	slices.Sort(cn.Spec.AuthorizedGPUUUIDs)
	cn.Annotations[ext.AgentCredentialAnnotation] = req.OperationID
	current := &ext.ContainerNode{}
	if err := s.ensureCreate(ctx, cn, current, func() bool {
		return reflect.DeepEqual(current.Spec, cn.Spec) && current.Annotations[ext.AgentCredentialAnnotation] == req.OperationID
	}); err != nil {
		return nil, err
	}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(cn), cn); err != nil {
		return nil, err
	}
	return cn, nil
}

// NodeIdentity selects an exact registered node and active credential for export.
type NodeIdentity struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	TenantID     string `json:"tenantID"`
	CredentialID string `json:"credentialID"`
}

// ReadNode revalidates the current native and companion bindings for packaging.
func (s Service) ReadNode(ctx context.Context, id NodeIdentity) (*ext.ContainerNode, error) {
	if s.Client == nil || id.Namespace == "" || id.Name == "" || id.UID == "" || id.CredentialID == "" || id.TenantID == "" {
		return nil, errors.New("exact node identity required")
	}
	var node ext.ContainerNode
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, &node); err != nil {
		return nil, err
	}
	if string(node.UID) != id.UID || tenant.IDOf(&node) != id.TenantID || node.Annotations[ext.AgentCredentialAnnotation] != id.CredentialID || !node.DeletionTimestamp.IsZero() || node.Spec.Runtime != "Raw" && node.Spec.Runtime != "Docker" {
		return nil, ErrConflict
	}
	cluster, cc, err := s.cluster(ctx, node.Spec.Cluster.Name, node.Spec.Cluster.UID, id.TenantID)
	if err != nil {
		return nil, err
	}
	if ref(cluster) != node.Spec.Cluster || cc.Spec.Runtime != node.Spec.Runtime || node.Namespace != cluster.Spec.AgentNamespace {
		return nil, ErrConflict
	}
	var nn native.Node
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: node.Spec.Node.Namespace, Name: node.Spec.Node.Name}, &nn); err != nil {
		return nil, err
	}
	if ownership.MatchesNative(node.Spec.Node, &nn) != nil || nn.Namespace != node.Namespace || string(nn.Spec.AgentType) != string(node.Spec.Runtime) || tenant.IDOf(&nn) != id.TenantID || !nn.DeletionTimestamp.IsZero() || nn.Status.Reservation != nil || len(nn.Status.DockerReservations) != 0 {
		return nil, ErrConflict
	}
	if err := rawnode.ValidateCapability(&native.RawNodeStatus{Incarnation: node.Spec.InstanceID, TrainingIP: node.Spec.PrivateAddress, Environment: node.Spec.Environment, NetworkScope: node.Spec.NetworkScope, AuthorizedGPUUUIDs: node.Spec.AuthorizedGPUUUIDs}); err != nil {
		return nil, err
	}
	return &node, nil
}
