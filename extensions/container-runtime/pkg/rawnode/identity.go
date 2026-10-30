// Package rawnode defines the experimental node-scoped management contract for
// agents running inside existing containers. It grants no Kubernetes identity.
package rawnode

import (
	"errors"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Role                 = "raw-node"
	CredentialAnnotation = "raw.rlark.io/credential-id"
)

// Identity is signed by the administrator and checked against live resources on
// every request. Re-enrollment rotates CredentialID and invalidates older keys.
type Identity struct {
	Cluster      string
	ClusterUID   string
	Namespace    string
	Node         string
	NodeUID      string
	Incarnation  string
	TenantID     string
	CredentialID string
}

// HeartbeatHolder binds liveness to the current object, container and credential.
func (i Identity) HeartbeatHolder() string {
	return i.NodeUID + "/" + i.Incarnation + "/" + i.CredentialID
}

// HeartbeatMatches prevents a replaced container or rotated credential's Lease
// from making a newly provisioned Raw Node appear connected.
func HeartbeatMatches(node *api.Node, lease *coordinationv1.Lease) bool {
	if node == nil || lease == nil || node.UID == "" || node.Status.Raw == nil || node.Status.Raw.Incarnation == "" || node.Annotations[CredentialAnnotation] == "" {
		return false
	}
	owner := metav1.GetControllerOf(lease)
	identity := Identity{NodeUID: string(node.UID), Incarnation: node.Status.Raw.Incarnation, CredentialID: node.Annotations[CredentialAnnotation]}
	return lease.Namespace == node.Namespace && owner != nil && owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Node" && owner.Name == node.Name && owner.UID == node.UID && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == identity.HeartbeatHolder()
}

// Metadata returns certificate metadata without proxy or impersonation grants.
func (i Identity) Metadata() map[string]string {
	return map[string]string{"cert-role": Role, "namespace": i.Namespace,
		"raw-cluster": i.Cluster, "raw-cluster-uid": i.ClusterUID,
		"raw-node": i.Node, "raw-node-uid": i.NodeUID,
		"raw-incarnation": i.Incarnation, "raw-tenant": i.TenantID,
		"raw-credential": i.CredentialID}
}

// FromMetadata rejects incomplete identities before any API access.
func FromMetadata(m map[string]string) (Identity, error) {
	i := Identity{Cluster: m["raw-cluster"], ClusterUID: m["raw-cluster-uid"], Namespace: m["namespace"], Node: m["raw-node"], NodeUID: m["raw-node-uid"], Incarnation: m["raw-incarnation"], TenantID: m["raw-tenant"], CredentialID: m["raw-credential"]}
	if m["cert-role"] != Role {
		return Identity{}, errors.New("Raw node certificate required")
	}
	for _, v := range i.Metadata() {
		if v == "" {
			return Identity{}, errors.New("incomplete Raw node certificate")
		}
	}
	return i, nil
}

// RESTMapper avoids API discovery, which node credentials deliberately cannot
// access. Only the resources needed by Host and admission are registered.
func RESTMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{api.GroupVersion})
	for _, kind := range []string{"Node", "Task"} {
		m.Add(api.GroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	for _, kind := range []string{"Job", "Cluster"} {
		m.Add(api.GroupVersion.WithKind(kind), meta.RESTScopeRoot)
	}
	return m
}
