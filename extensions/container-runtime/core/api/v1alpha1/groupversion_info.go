package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const Group = "containers.rlark.io"

// AgentCredentialAnnotation holds the non-secret enrollment identity currently
// authorized by an administrator. Rotation invalidates previous observations.
const AgentCredentialAnnotation = Group + "/agent-credential"

var GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&ContainerCluster{}, &ContainerClusterList{},
		&ContainerNode{}, &ContainerNodeList{},
		&ContainerAllocation{}, &ContainerAllocationList{},
		&ContainerJob{}, &ContainerJobList{},
		&AgentRelease{}, &AgentReleaseList{},
		&AgentUpgrade{}, &AgentUpgradeList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

// AgentUpgradeCapability identifies the enabled scoped Linux updater protocol.
const AgentUpgradeCapability = "agent-upgrade-v1"
