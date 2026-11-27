package registry

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndexPlatformsPrefersLinuxAMD64(t *testing.T) {
	entry := func(os, arch, digit string) registryDescriptor {
		d := registryDescriptor{Digest: "sha256:" + strings.Repeat(digit, 64)}
		d.Platform = &struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		}{OS: os, Architecture: arch}
		return d
	}
	platforms, chosen := indexPlatforms([]registryDescriptor{entry("linux", "arm64", "1"), entry("linux", "amd64", "2"), entry("windows", "amd64", "3")})
	require.Equal(t, []string{"linux/arm64", "linux/amd64", "windows/amd64"}, platforms)
	require.Equal(t, "sha256:"+strings.Repeat("2", 64), chosen.Digest)
	_, none := indexPlatforms([]registryDescriptor{entry("unknown", "unknown", "4")})
	require.Nil(t, none)
}

func TestImageSizeRejectsOverflow(t *testing.T) {
	manifest := registryManifestDocument{
		Config: registryDescriptor{Size: 20, Digest: "sha256:" + strings.Repeat("a", 64)},
		Layers: []registryDescriptor{{Size: math.MaxInt64}},
	}
	_, err := imageInfoFromManifest(t.Context(), func(context.Context, string) (*http.Response, error) {
		t.Fatal("attempted to read the config of an invalid manifest")
		return nil, nil
	}, "example/image", manifest)
	require.ErrorIs(t, err, errRegistryImageUnavailable)
}
