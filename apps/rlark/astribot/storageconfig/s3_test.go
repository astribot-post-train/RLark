package storageconfig

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorageMountProfiles(t *testing.T) {
	options, backend, cache, err := ResolveS3MountOptions("archive", "", nil)
	require.NoError(t, err)
	require.Equal(t, "off", cache)
	require.Contains(t, options, "dir-cache-time=5s")
	require.Equal(t, map[string]string{"chunk_size": "32Mi", "upload_concurrency": "4"}, backend)
	require.Contains(t, options, "attr-timeout=1s")
	read, _, _, err := ResolveS3MountOptions("read", "", []string{"low-level-retries=30"})
	require.NoError(t, err)
	require.Contains(t, read, "read-only=true")
	require.Contains(t, read, "low-level-retries=30")
	require.NotContains(t, read, "ignore-checksum=true")
	// Resolving one mount must not change another mount's defaults.
	defaults, _, _, err := ResolveS3MountOptions("default", "", nil)
	require.NoError(t, err)
	require.NotContains(t, defaults, "read-only=true")
	require.Contains(t, defaults, "vfs-cache-mode=writes")
	for _, tc := range []struct {
		profile, cache string
		options        []string
	}{
		{"invalid", "", nil}, {"archive", "writes", nil}, {"read", "", []string{"read-only=false"}},
		{"default", "", []string{"ignore-checksum"}}, {"default", "", []string{"tpslimit=2"}},
		{"default", "", []string{"low-level-retries=0"}}, {"default", "", []string{"s3-chunk-size=4Mi"}},
		{"default", "", []string{"s3-chunk-size=6Gi"}}, {"default", "", []string{"dir-cache-time=-1s"}},
		{"default", "", []string{"transfers=0"}}, {"default", "", []string{"checkers=1", "checkers=2"}},
		{"default", "", []string{"config=/tmp/credentials"}}, {"default", "", []string{"read-only=true\nfoo=bar"}},
	} {
		_, _, _, err := ResolveS3MountOptions(tc.profile, tc.cache, tc.options)
		require.Error(t, err, "%+v", tc)
	}
}
