package gateway

import (
	"testing"

	"github.com/rlinf/rlark/apps/rlark/astribot/storageconfig"
	"github.com/rlinf/rlark/apps/rlark/pkg/distribution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestStorageClassPrefixAndVFSCacheMode(t *testing.T) {
	base := CreateStorageClassRequest{
		Name: "fleet-ks3", Clusters: []string{"rlark-ksyun-gpu"}, Provider: "Other",
		Endpoint: "https://ks3-cn-ningxia.ksyuncs.com", Bucket: "stardust", Region: "cn-ningxia",
		AccessKeyId: "access", AccessKeySecret: "secret",
	}

	legacy, err := storageconfig.NormalizeClass(base)
	require.NoError(t, err)
	assert.Empty(t, legacy.Prefix)
	assert.Equal(t, "writes", storageconfig.EffectiveVFSCacheMode(legacy.VFSCacheMode))

	base.Prefix = "ks3-regular"
	base.VFSCacheMode = "off"
	stored, err := storageconfig.NormalizeClass(base)
	require.NoError(t, err)
	decl, err := storageconfig.RenderReplication("management", "sc-0123456789abcdef", stored)
	require.NoError(t, err)
	replication, err := distribution.Parse(decl.Data)
	require.NoError(t, err)
	data := nestedData(t, replication.Objects[0])
	delivery, err := distribution.Parse(map[string][]byte{
		"config":    decodeStorageManifestData(t, data["config"]),
		"manifests": decodeStorageManifestData(t, data["manifests"]),
	})
	require.NoError(t, err)
	for _, object := range delivery.Objects {
		if object.GetKind() != "StorageClass" {
			continue
		}
		assert.Equal(t, "stardust/ks3-regular", nestedString(t, object.Object, "parameters", "remotePath"))
		options, _, err := unstructured.NestedStringSlice(object.Object, "mountOptions")
		require.NoError(t, err)
		assert.Contains(t, options, "vfs-cache-mode=off")
		assert.NotContains(t, options, "vfs-cache-mode=writes")
		return
	}
	t.Fatal("StorageClass manifest not found")
}

func TestNormalizeStorageClassRejectsInvalidPrefixAndCacheMode(t *testing.T) {
	base := CreateStorageClassRequest{
		Name: "fleet-ks3", Clusters: []string{"rlark-ksyun-gpu"}, Provider: "Other",
		Endpoint: "https://ks3-cn-ningxia.ksyuncs.com", Bucket: "stardust", Region: "cn-ningxia",
		AccessKeySecret: "secret",
	}
	for _, prefix := range []string{"a//b", "../private", "a/../b", `a\\b`} {
		base.Prefix = prefix
		_, err := storageconfig.NormalizeClass(base)
		require.Error(t, err, "prefix %q", prefix)
	}
	base.Prefix = ""
	base.VFSCacheMode = "full"
	_, err := storageconfig.NormalizeClass(base)
	require.ErrorContains(t, err, "vfs_cache_mode")
}
