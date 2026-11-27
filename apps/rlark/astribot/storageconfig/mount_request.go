package storageconfig

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/rlinf/rlark/apps/rlark/astribot/storageref"
)

// S3MountRequest describes the mount options supplied alongside an S3 class.
type S3MountRequest struct {
	Region, MountEndpoint, Prefix, MountProfile, VFSCacheMode string
	MountOptions                                              *[]string
	ClusterMountEndpoints                                     *map[string]string
}

// NormalizeS3Mount adds to an S3 storage the official flow normalized
// how it is mounted: endpoints, prefix, profile and mount options.
func NormalizeS3Mount(req S3MountRequest, stored StoredClass, normalizeEndpoint func(provider, endpoint, region string) (string, error)) (StoredClass, error) {
	mountEndpoint := strings.TrimSpace(req.MountEndpoint)
	if mountEndpoint != "" {
		var err error
		mountEndpoint, err = normalizeEndpoint(stored.Provider, mountEndpoint, req.Region)
		if err != nil {
			return StoredClass{}, fmt.Errorf("mount_endpoint: %w", err)
		}
	}
	prefix := strings.Trim(strings.TrimSpace(req.Prefix), "/")
	if strings.Contains(prefix, "\\") || strings.Contains(prefix, "//") || strings.IndexFunc(prefix, unicode.IsControl) >= 0 {
		return StoredClass{}, fmt.Errorf("prefix must be a slash-separated object path")
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "." || segment == ".." {
			return StoredClass{}, fmt.Errorf("prefix must not contain dot segments")
		}
	}
	profile := strings.TrimSpace(req.MountProfile)
	if profile == "" {
		profile = "default"
	}
	var overrides []string
	if req.MountOptions != nil {
		overrides = append([]string{}, (*req.MountOptions)...)
	}
	_, _, cacheMode, err := ResolveS3MountOptions(profile, strings.TrimSpace(req.VFSCacheMode), overrides)
	if err != nil {
		return StoredClass{}, err
	}
	// A volume may choose any profile, so the storage's mount options must
	// combine with each of them.
	for _, other := range storageref.Profiles("s3") {
		if other == profile {
			continue
		}
		if _, _, _, err := ResolveS3MountOptions(other, "", overrides); err != nil {
			return StoredClass{}, fmt.Errorf("mount_options with profile %s: %w", other, err)
		}
	}
	var clusterEndpoints map[string]string
	if req.ClusterMountEndpoints != nil && len(*req.ClusterMountEndpoints) != 0 {
		clusterEndpoints = make(map[string]string, len(*req.ClusterMountEndpoints))
		for cluster, endpoint := range *req.ClusterMountEndpoints {
			if !slices.Contains(stored.Clusters, cluster) {
				return StoredClass{}, fmt.Errorf("cluster_mount_endpoints names cluster %q, which is not one of the storage's clusters", cluster)
			}
			normalized, err := normalizeEndpoint(stored.Provider, strings.TrimSpace(endpoint), req.Region)
			if err != nil || strings.TrimSpace(endpoint) == "" {
				return StoredClass{}, fmt.Errorf("cluster_mount_endpoints[%s]: endpoint must be an http or https URL", cluster)
			}
			clusterEndpoints[cluster] = normalized
		}
	}
	stored.Type, stored.MountEndpoint, stored.ClusterMountEndpoints = "s3", mountEndpoint, clusterEndpoints
	stored.Prefix, stored.VFSCacheMode, stored.MountProfile, stored.MountOptions = prefix, cacheMode, profile, overrides
	return stored, nil
}
