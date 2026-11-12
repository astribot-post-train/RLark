package gateway

import (
	"encoding/base64"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"

	"github.com/rlinf/rlark/apps/rlark/pkg/distribution"
)

func TestStorageMountProfiles(t *testing.T) {
	options, backend, cache, err := resolveS3MountOptions("archive", "", nil)
	require.NoError(t, err)
	require.Equal(t, "off", cache)
	require.Contains(t, options, "dir-cache-time=5s")
	require.Equal(t, map[string]string{"chunk_size": "32Mi", "upload_concurrency": "4"}, backend)
	read, _, _, err := resolveS3MountOptions("read", "", []string{"tpslimit=2", "low-level-retries=10"})
	require.NoError(t, err)
	require.Contains(t, read, "read-only=true")
	require.Contains(t, read, "tpslimit=2")
	require.NotContains(t, read, "ignore-checksum=true")
	// Resolving one mount must not change another mount's defaults.
	defaults, _, _, err := resolveS3MountOptions("default", "", nil)
	require.NoError(t, err)
	require.NotContains(t, defaults, "read-only=true")
	require.Contains(t, defaults, "vfs-cache-mode=writes")
	for _, tc := range []struct {
		profile, cache string
		options        []string
	}{
		{"invalid", "", nil}, {"archive", "writes", nil}, {"read", "", []string{"read-only=false"}},
		{"default", "", []string{"ignore-checksum"}}, {"default", "", []string{"tpslimit=NaN"}},
		{"default", "", []string{"tpslimit=0"}}, {"default", "", []string{"s3-chunk-size=4Mi"}},
		{"default", "", []string{"s3-chunk-size=6Gi"}}, {"default", "", []string{"dir-cache-time=-1s"}},
		{"default", "", []string{"transfers=0"}}, {"default", "", []string{"tpslimit=1", "tpslimit=2"}},
		{"default", "", []string{"config=/tmp/credentials"}}, {"default", "", []string{"read-only=true\nfoo=bar"}},
	} {
		_, _, _, err := resolveS3MountOptions(tc.profile, tc.cache, tc.options)
		require.Error(t, err, "%+v", tc)
	}
}

func TestStorageMountPolicyRenderBackendAndRoundTrip(t *testing.T) {
	options := []string{"s3-chunk-size=64Mi", "s3-upload-concurrency=2", "tpslimit=2"}
	s, err := normalizeStorageClass(CreateStorageClassRequest{Name: "archive", Clusters: []string{"cloud"}, Provider: "Other", Endpoint: "https://s3.example.com", Bucket: "bucket", AccessKeySecret: "secret", MountProfile: "archive", MountOptions: &options})
	require.NoError(t, err)
	require.Equal(t, "off", s.VFSCacheMode)
	declaration, err := buildStorageClassReplicationSecret("default", "sc-0123456789abcdef", s)
	require.NoError(t, err)
	_, decoded, err := storageClassFromSecret(declaration)
	require.NoError(t, err)
	require.Equal(t, s, decoded)
	_, backend, _, err := resolveS3MountOptions(s.MountProfile, s.VFSCacheMode, s.MountOptions)
	require.NoError(t, err)
	require.Equal(t, "64Mi", backend["chunk_size"])
	require.Equal(t, "2", backend["upload_concurrency"])
}

func TestStorageClassesUseIndependentRcloneRemoteNames(t *testing.T) {
	read, err := normalizeStorageClass(CreateStorageClassRequest{
		Name: "read", Clusters: []string{"cloud"}, Provider: "Other", Endpoint: "https://s3.example.com",
		Bucket: "bucket", AccessKeySecret: "read-secret", MountProfile: "read",
	})
	require.NoError(t, err)
	archive, err := normalizeStorageClass(CreateStorageClassRequest{
		Name: "archive", Clusters: []string{"cloud"}, Provider: "Other", Endpoint: "https://s3.example.com",
		Bucket: "bucket", AccessKeySecret: "archive-secret", MountProfile: "archive",
	})
	require.NoError(t, err)

	readSecret, err := buildStorageClassReplicationSecret("default", "sc-read", read)
	require.NoError(t, err)
	archiveSecret, err := buildStorageClassReplicationSecret("default", "sc-archive", archive)
	require.NoError(t, err)
	readManifest := parseStorageClassObjects(t, readSecret)
	archiveManifest := parseStorageClassObjects(t, archiveSecret)

	readRemote := nestedString(t, readManifest[0].Object, "stringData", "remote")
	archiveRemote := nestedString(t, archiveManifest[0].Object, "stringData", "remote")
	require.Equal(t, "s3-sc-read", readRemote)
	require.Equal(t, "s3-sc-archive", archiveRemote)
	require.NotEqual(t, readRemote, archiveRemote)
	require.Contains(t, nestedString(t, readManifest[0].Object, "stringData", "configData"), "["+readRemote+"]")
	require.Contains(t, nestedString(t, archiveManifest[0].Object, "stringData", "configData"), "["+archiveRemote+"]")
	require.Equal(t, readRemote, nestedString(t, readManifest[1].Object, "parameters", "remote"))
	require.Equal(t, archiveRemote, nestedString(t, archiveManifest[1].Object, "parameters", "remote"))
}

func parseStorageClassObjects(t *testing.T, secret *v1.Secret) []*unstructured.Unstructured {
	t.Helper()
	replication, err := distribution.Parse(secret.Data)
	require.NoError(t, err)
	deliveryData := nestedData(t, replication.Objects[0])
	decode := func(value string) []byte {
		data, err := base64.StdEncoding.DecodeString(value)
		require.NoError(t, err)
		return data
	}
	delivery, err := distribution.Parse(map[string][]byte{
		"config": decode(deliveryData["config"]), "manifests": decode(deliveryData["manifests"]),
	})
	require.NoError(t, err)
	return delivery.Objects
}
