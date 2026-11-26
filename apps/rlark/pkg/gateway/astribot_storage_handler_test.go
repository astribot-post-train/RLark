package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rlinf/rlark/apps/rlark/astribot/storageconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

func TestRerenderStorageClassesRewritesOnlyStaleRenders(t *testing.T) {
	object, err := storageconfig.NormalizeClass(CreateStorageClassRequest{
		Name: "object-storage", Clusters: []string{"rlark-cluster-a"}, Provider: "Minio",
		Endpoint: "https://minio.example.com", AccessKeyId: "access", AccessKeySecret: "stored-secret", Bucket: "bucket",
	})
	require.NoError(t, err)
	share, err := storageconfig.NormalizeClass(CreateStorageClassRequest{
		Name: "share", Clusters: []string{"rlark-cluster-a"},
		SpecialClassRequest: storageconfig.SpecialClassRequest{Type: "smb", Host: "nas.example.com", Share: "group", Username: "user", Password: "stored-password"},
	})
	require.NoError(t, err)
	stale, err := storageconfig.RenderReplication("management", "sc-0123456789abcdef", object)
	require.NoError(t, err)
	// The render of an earlier Gateway: the class kept its volumes.
	var objects []json.RawMessage
	manifests, err := yaml.YAMLToJSON(stale.Data["manifests"])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(manifests, &objects))
	var delivery v1.Secret
	require.NoError(t, json.Unmarshal(objects[0], &delivery))
	delivery.Data["manifests"] = bytes.Replace(delivery.Data["manifests"], []byte("reclaimPolicy: Delete"), []byte("reclaimPolicy: Retain"), 1)
	stale.Data["manifests"], err = yaml.Marshal([]any{delivery, objects[1]})
	require.NoError(t, err)
	current, err := storageconfig.RenderReplication("management", "sc-fedcba9876543210", share)
	require.NoError(t, err)
	// A declaration stored with its fields in another order is the same render.
	var fields map[string]any
	require.NoError(t, json.Unmarshal(current.Data["storageclass"], &fields))
	current.Data["storageclass"], err = json.Marshal(fields)
	require.NoError(t, err)
	client := fake.NewSimpleClientset(stale, current)

	storageconfig.RerenderClasses(context.Background(), client, "management")
	storageconfig.RerenderClasses(context.Background(), client, "management")

	var updated []string
	for _, action := range client.Actions() {
		if update, ok := action.(k8stesting.UpdateAction); ok {
			updated = append(updated, update.GetObject().(*v1.Secret).Name)
		}
	}
	// The SMB password is obscured differently on every render and is not a change.
	assert.Equal(t, []string{stale.Name}, updated)
	rewritten, err := client.CoreV1().Secrets("management").Get(context.Background(), stale.Name, metav1.GetOptions{})
	require.NoError(t, err)
	manifests, err = yaml.YAMLToJSON(rewritten.Data["manifests"])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(manifests, &objects))
	require.NoError(t, json.Unmarshal(objects[0], &delivery))
	assert.NotContains(t, string(delivery.Data["manifests"]), "reclaimPolicy: Retain")
	_, stored, err := storageconfig.ClassFromSecret(rewritten)
	require.NoError(t, err)
	assert.Equal(t, "stored-secret", stored.AccessKey)
}
