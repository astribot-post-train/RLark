package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const Group = "containers.rlark.io"

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
