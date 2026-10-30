package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const Group = "containers.rlark.io"

// AgentCredentialAnnotation holds the non-secret enrollment identity currently
// authorized by an administrator. Rotation invalidates previous observations.
const AgentCredentialAnnotation = Group + "/agent-credential"

var GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(
		&ContainerCluster{}, &ContainerClusterList{},
		&ContainerNode{}, &ContainerNodeList{},
		&ContainerAllocation{}, &ContainerAllocationList{},
		&ContainerJob{}, &ContainerJobList{},
		&AgentRelease{}, &AgentReleaseList{},
	)
}
