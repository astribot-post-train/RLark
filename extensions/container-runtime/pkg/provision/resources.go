package provision

import (
	"context"
	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	management "github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/provision"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Service struct{ Client client.Client }
type ClusterRequest = management.ClusterRequest
type NodeRequest = management.NodeRequest
type NodeIdentity = management.NodeIdentity
type UpgradeRequest = management.UpgradeRequest

const OperationAnnotation = management.OperationAnnotation
const ParentUIDAnnotation = management.ParentUIDAnnotation
const RequestAnnotation = management.RequestAnnotation

var ErrConflict = management.ErrConflict

func (s Service) CreateCluster(ctx context.Context, r ClusterRequest) (*ext.ContainerCluster, error) {
	return management.Service{Client: s.Client}.CreateCluster(ctx, r)
}
func (s Service) RegisterNode(ctx context.Context, r NodeRequest) (*ext.ContainerNode, error) {
	return management.Service{Client: s.Client}.RegisterNode(ctx, r)
}
func (s Service) ReadNode(ctx context.Context, r NodeIdentity) (*ext.ContainerNode, error) {
	return management.Service{Client: s.Client}.ReadNode(ctx, r)
}
func (s Service) CreateUpgrade(ctx context.Context, r UpgradeRequest) (*ext.AgentUpgrade, error) {
	return management.Service{Client: s.Client}.CreateUpgrade(ctx, r)
}
