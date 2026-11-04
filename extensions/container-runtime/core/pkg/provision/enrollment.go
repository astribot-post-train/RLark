package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/artifact"
	tenant "github.com/rlinf/rlark/api/metadata"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnrollmentRequest contains no SSH secrets, paths, or remote commands. Empty
// Release selects the most recently published available Linux/amd64 release.
type EnrollmentRequest struct {
	OperationID string               `json:"operationID"`
	Namespace   string               `json:"namespace"`
	Cluster     string               `json:"cluster"`
	ClusterUID  string               `json:"clusterUID"`
	TenantID    string               `json:"tenantID"`
	Release     string               `json:"release"`
	Nodes       []ext.EnrollmentNode `json:"nodes"`
}

// ValidateEnrollmentNodes validates the whole topology, including direct-API input.
func ValidateEnrollmentNodes(nodes []ext.EnrollmentNode) error {
	if len(nodes) == 0 || len(nodes) > 64 {
		return errors.New("one to 64 nodes required")
	}
	names, addresses := map[string]bool{}, map[string]bool{}
	gateways := 0
	for _, n := range nodes {
		ip, private := net.ParseIP(n.Address), net.ParseIP(n.PrivateAddress)
		if len(validation.IsDNS1123Subdomain(n.Name)) != 0 || names[n.Name] || ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || n.PrivateAddress != "" && (private == nil || private.To4() == nil || !private.IsPrivate()) || n.PrivateAddress != "" && addresses[n.PrivateAddress] || n.Port < 1 || n.Port > 65535 {
			return errors.New("unique names/private IPv4 addresses and valid SSH targets/ports required")
		}
		names[n.Name] = true
		if n.PrivateAddress != "" {
			addresses[n.PrivateAddress] = true
		}
		switch n.Role {
		case ext.NodeRoleGateway:
			gateways++
		case ext.NodeRoleWorker:
			if !ip.IsPrivate() {
				return errors.New("worker SSH target must use its private IPv4 address")
			}
		default:
			return errors.New("Gateway or Worker role required")
		}
	}
	if gateways != 1 {
		return errors.New("exactly one Gateway is required")
	}
	return nil
}

// CreateEnrollment pins the selected release in an immutable operation before
// SSH effects. Replaying its UUID cannot change its targets or selected bytes.
func (s Service) CreateEnrollment(ctx context.Context, req EnrollmentRequest) (*ext.NodeEnrollment, error) {
	if err := validateOperation(req.OperationID, req.TenantID, req.Cluster); err != nil {
		return nil, err
	}
	req.Nodes = append([]ext.EnrollmentNode(nil), req.Nodes...)
	for i := range req.Nodes {
		if req.Nodes[i].Port == 0 {
			req.Nodes[i].Port = 22
		}
	}
	if err := ValidateEnrollmentNodes(req.Nodes); err != nil {
		return nil, err
	}
	var cc ext.ContainerCluster
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Cluster}, &cc); err != nil {
		return nil, err
	}
	_, bound, err := s.cluster(ctx, cc.Spec.Cluster.Name, cc.Spec.Cluster.UID, req.TenantID)
	if err != nil {
		return nil, err
	}
	if cc.UID == "" || string(cc.UID) != req.ClusterUID || cc.UID != bound.UID || cc.Spec.Runtime != "Raw" {
		return nil, ErrConflict
	}
	req.Nodes = append([]ext.EnrollmentNode(nil), req.Nodes...)
	sort.Slice(req.Nodes, func(i, j int) bool { return req.Nodes[i].Name < req.Nodes[j].Name })
	intent, err := fingerprint(req)
	if err != nil {
		return nil, err
	}
	name := "enroll-" + req.OperationID
	// Recover latest using the retained exact request fingerprint, not today's catalog.
	var existing ext.NodeEnrollment
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: cc.Namespace, Name: name}, &existing); err == nil {
		wanted := &ext.NodeEnrollment{}
		wanted.ObjectMeta = metadata(name, cc.Namespace, req.TenantID, req.OperationID, intent)
		if !owned(&existing, wanted) || existing.Spec.ClusterRecord != ref(&cc) || existing.Spec.OperationID != req.OperationID || !reflect.DeepEqual(existing.Spec.Nodes, req.Nodes) {
			return nil, ErrConflict
		}
		return &existing, nil
	} else if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	var releases ext.AgentReleaseList
	if err := s.Client.List(ctx, &releases, client.InNamespace(cc.Namespace)); err != nil {
		return nil, err
	}
	sort.Slice(releases.Items, func(i, j int) bool {
		a, b := releases.Items[i], releases.Items[j]
		if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.Name > b.Name
		}
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	})
	var selected *ext.AgentRelease
	for i := range releases.Items {
		r := &releases.Items[i]
		if req.Release != "" && req.Release != r.Name {
			continue
		}
		if r.UID != "" && tenant.IDOf(r) == req.TenantID && r.DeletionTimestamp.IsZero() && r.Spec.Architecture == "amd64" && artifact.ValidateSpec(r.Spec) == nil && r.Status.Available && r.Status.ObservedGeneration == r.Generation {
			selected = r
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("%w: no available compatible Agent release", ErrConflict)
	}
	op := &ext.NodeEnrollment{ObjectMeta: metadata(name, cc.Namespace, req.TenantID, req.OperationID, intent), Spec: ext.NodeEnrollmentSpec{ClusterRecord: ref(&cc), TenantID: req.TenantID, OperationID: req.OperationID, Release: ref(selected), ReleaseSpec: selected.Spec, Nodes: req.Nodes}}
	current := &ext.NodeEnrollment{}
	if err := s.ensureCreate(ctx, op, current, func() bool { return reflect.DeepEqual(current.Spec, op.Spec) }); err != nil {
		return nil, err
	}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(op), op); err != nil {
		return nil, err
	}
	return op, nil
}
