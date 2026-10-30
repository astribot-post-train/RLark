package rawnode

import (
	"testing"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
)

func TestIdentityRequiresEverySignedScope(t *testing.T) {
	identity := Identity{Cluster: "cloud", ClusterUID: "cluster", Namespace: "agents", Node: "node", NodeUID: "uid", Incarnation: "container", TenantID: "tenant", CredentialID: "key"}
	decoded, err := FromMetadata(identity.Metadata())
	require.NoError(t, err)
	require.Equal(t, identity, decoded)
	for key := range identity.Metadata() {
		t.Run(key, func(t *testing.T) {
			metadata := identity.Metadata()
			delete(metadata, key)
			_, err := FromMetadata(metadata)
			require.Error(t, err)
		})
	}
	metadata := identity.Metadata()
	metadata["cert-role"] = "agent"
	_, err = FromMetadata(metadata)
	require.Error(t, err)
}

func TestRESTMapperHasOnlyTheScopedContract(t *testing.T) {
	mapper := RESTMapper()
	for _, kind := range []string{"Node", "Task", "Job", "Cluster"} {
		mapping, err := mapper.RESTMapping(api.GroupVersion.WithKind(kind).GroupKind(), api.GroupVersion.Version)
		require.NoError(t, err)
		expected := meta.RESTScopeNameRoot
		if kind == "Node" || kind == "Task" {
			expected = meta.RESTScopeNameNamespace
		}
		require.Equal(t, expected, mapping.Scope.Name())
	}
	_, err := mapper.RESTMapping(api.GroupVersion.WithKind("Pod").GroupKind(), api.GroupVersion.Version)
	require.Error(t, err)
}
