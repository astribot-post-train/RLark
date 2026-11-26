package storageconfig

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestClassUpdateDoesNotRetryUsingStaleValues(t *testing.T) {
	ctx := context.Background()
	stored := StoredClass{Version: DeclarationVersion, Name: "shared", Provider: "Other", Bucket: "bucket", Clusters: []string{"rlark-one"}}
	identity, err := RenderReplication("default", "sc-0123456789abcdef", stored)
	require.NoError(t, err)
	identity.ResourceVersion = "7"
	client := fake.NewSimpleClientset(identity)
	var attempts int
	client.PrependReactor("update", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		attempts++
		updated := action.(ktesting.UpdateAction).GetObject().(*corev1.Secret)
		require.Equal(t, "7", updated.ResourceVersion)
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), identity.Name, fmt.Errorf("edited concurrently"))
	})
	stored.Description = "requested edit"
	require.True(t, apierrors.IsConflict(UpdateClass(ctx, client, identity, stored)))
	require.Equal(t, 1, attempts)
	_, original, err := ClassFromSecret(identity)
	require.NoError(t, err)
	require.Empty(t, original.Description)
}

func TestClassDeleteCarriesIdentityPreconditions(t *testing.T) {
	identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ReplicationName("sc-0123456789abcdef"), Namespace: "default", UID: "uid-1", ResourceVersion: "5"}}
	client := fake.NewSimpleClientset(identity)
	client.PrependReactor("delete", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		options := action.(ktesting.DeleteAction).GetDeleteOptions()
		require.NotNil(t, options.Preconditions.UID)
		require.NotNil(t, options.Preconditions.ResourceVersion)
		require.Equal(t, identity.UID, *options.Preconditions.UID)
		require.Equal(t, identity.ResourceVersion, *options.Preconditions.ResourceVersion)
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), identity.Name, fmt.Errorf("replaced concurrently"))
	})
	require.True(t, apierrors.IsConflict(DeleteClass(context.Background(), client, identity)))
}

func TestAddSiteClassRepairsEndpointAfterPartialWrite(t *testing.T) {
	ctx := context.Background()
	const namespace, id, cluster = "default", "sc-0123456789abcdef", "rlark-two"
	stored := StoredClass{
		Version: DeclarationVersion, Name: "shared", Provider: "Other", Bucket: "bucket",
		Endpoint: "https://public.example.com", Clusters: []string{"rlark-one"},
		MountSettings: MountSettings{MountEndpoint: "https://private.example.com"},
	}
	identity, err := RenderReplication(namespace, id, stored)
	require.NoError(t, err)
	client := fake.NewSimpleClientset(identity)
	missingEndpoint := storageEndpointSecretName(id, cluster)
	first := true
	client.PrependReactor("create", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		secret := action.(ktesting.CreateAction).GetObject().(*corev1.Secret)
		if secret.Name == missingEndpoint && first {
			first = false
			return true, nil, apierrors.NewServiceUnavailable("endpoint declaration write interrupted")
		}
		return false, nil, nil
	})
	others := []string{"rlark-one"}
	_, err = AddClusterToSiteClasses(ctx, client, namespace, cluster, others)
	require.True(t, apierrors.IsServiceUnavailable(err))
	added, err := AddClusterToSiteClasses(ctx, client, namespace, cluster, others)
	require.NoError(t, err)
	require.Equal(t, []string{"shared"}, added)

	updated, err := client.CoreV1().Secrets(namespace).Get(ctx, identity.Name, metav1.GetOptions{})
	require.NoError(t, err)
	_, stored, err = ClassFromSecret(updated)
	require.NoError(t, err)
	require.Equal(t, []string{"rlark-one", cluster}, stored.Clusters)
	require.Equal(t, stored.Endpoint, stored.ClusterMountEndpoints[cluster])
	endpoint, err := client.CoreV1().Secrets(namespace).Get(ctx, missingEndpoint, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, id, endpoint.Labels[EndpointLabel])
}
