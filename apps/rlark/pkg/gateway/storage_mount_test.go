package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rlinf/rlark/apps/rlark/pkg/distribution"
)

// mountOptionsOf returns the mount options of the StorageClass a declaration delivers.
func mountOptionsOf(t *testing.T, stored storedStorageClass) []string {
	t.Helper()
	secret, err := buildStorageClassReplicationSecret("management", "sc-0123456789abcdef", stored)
	require.NoError(t, err)
	replication, err := distribution.Parse(secret.Data)
	require.NoError(t, err)
	delivery, err := distribution.Parse(map[string][]byte{
		"config":    decodeStorageManifestData(t, nestedData(t, replication.Objects[0])["config"]),
		"manifests": decodeStorageManifestData(t, nestedData(t, replication.Objects[0])["manifests"]),
	})
	require.NoError(t, err)
	for _, object := range delivery.Objects {
		if object.GetKind() == "StorageClass" {
			data, err := yaml.Marshal(object.Object["mountOptions"])
			require.NoError(t, err)
			var options []string
			require.NoError(t, yaml.Unmarshal(data, &options))
			return options
		}
	}
	t.Fatal("no StorageClass delivered")
	return nil
}

func TestStorageClassMountProfilesAndOptions(t *testing.T) {
	req := CreateStorageClassRequest{
		Name: "ks3", Clusters: []string{"rlark-ksyun-gpu"}, Provider: "Minio", Endpoint: "https://minio.example.com",
		AccessKeyId: "access", AccessKeySecret: "secret", Bucket: "bucket",
	}
	stored, err := normalizeStorageClass(req)
	require.NoError(t, err)
	assert.Equal(t, s3MountOptions, mountOptionsOf(t, stored), "a class without a profile is mounted as before")

	req.MountProfile = "archive"
	stored, err = normalizeStorageClass(req)
	require.NoError(t, err)
	archive := mountOptionsOf(t, stored)
	assert.Contains(t, archive, "vfs-cache-mode=off", "an archived file is on the object store when the write returns")
	assert.Contains(t, archive, "dir-cache-time=5s")
	assert.NotContains(t, archive, "vfs-cache-mode=writes")

	req.MountProfile = "read"
	stored, err = normalizeStorageClass(req)
	require.NoError(t, err)
	read := mountOptionsOf(t, stored)
	for _, option := range []string{"ignore-checksum", "tpslimit=40", "low-level-retries=20", "dir-cache-time=1h", "read-only"} {
		assert.Contains(t, read, option)
	}

	req.MountProfile, req.MountOptions = "", []string{"--vfs-cache-mode=off", " dir-cache-time=30s "}
	stored, err = normalizeStorageClass(req)
	require.NoError(t, err)
	assert.Equal(t, []string{"vfs-cache-mode=off", "dir-cache-time=30s"}, mountOptionsOf(t, stored))

	for name, bad := range map[string]CreateStorageClassRequest{
		"unknown profile":     {MountProfile: "fast"},
		"profile and options": {MountProfile: "read", MountOptions: []string{"read-only"}},
		"not a flag":          {MountOptions: []string{"config=/etc/passwd; rm -rf /"}},
		"shell":               {MountOptions: []string{"$(id)"}},
	} {
		invalid := req
		invalid.MountProfile, invalid.MountOptions = bad.MountProfile, bad.MountOptions
		_, err := normalizeStorageClass(invalid)
		assert.Error(t, err, name)
	}

	smb := CreateStorageClassRequest{Name: "nas", Clusters: []string{"c"}, Type: "smb", Host: "10.11.5.12", Share: "share", Username: "u", Password: "p", MountProfile: "archive"}
	_, err = normalizeStorageClass(smb)
	assert.Error(t, err, "profiles are defined for s3 only")
}
