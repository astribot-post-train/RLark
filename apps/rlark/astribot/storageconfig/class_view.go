package storageconfig

import "strings"

// SpecialClassView is the public projection of mount settings and profiles.
type SpecialClassView struct {
	Type                    string            `json:"type"`
	MountEndpoint           string            `json:"mount_endpoint,omitempty"`
	ClusterMountEndpoints   map[string]string `json:"cluster_mount_endpoints,omitempty"`
	Prefix                  string            `json:"prefix,omitempty"`
	VFSCacheMode            string            `json:"vfs_cache_mode,omitempty"`
	MountProfile            string            `json:"mount_profile,omitempty"`
	MountOptions            []string          `json:"mount_options"`
	EffectiveMountOptions   []string          `json:"effective_mount_options"`
	EffectiveBackendOptions map[string]string `json:"effective_backend_options,omitempty"`
	Host                    string            `json:"host,omitempty"`
	Share                   string            `json:"share,omitempty"`
	SubPath                 string            `json:"subPath,omitempty"`
	Username                string            `json:"username,omitempty"`
	Domain                  string            `json:"domain,omitempty"`
	Path                    string            `json:"path,omitempty"`
	FSType                  string            `json:"fs_type,omitempty"`
	// Profiles maps each mount profile a volume can choose to the
	// StorageClass it gets; the storage's own profile keeps storageClassName.
	Profiles       map[string]string `json:"profiles,omitempty"`
	DefaultProfile string            `json:"default_profile"`
}

func DescribeSpecialClass(id string, stored StoredClass) (SpecialClassView, error) {
	var entry SpecialClassView
	entry.Type, entry.MountEndpoint, entry.Prefix, entry.VFSCacheMode = stored.Type, stored.MountEndpoint, stored.Prefix, EffectiveVFSCacheMode(stored.VFSCacheMode)
	entry.Host, entry.Share, entry.SubPath, entry.Username, entry.Domain = stored.Host, stored.Share, stored.SubPath, stored.Username, stored.Domain
	entry.Path, entry.FSType, entry.ClusterMountEndpoints = stored.Path, stored.FSType, stored.ClusterMountEndpoints
	view, err := View(id, stored)
	if err != nil {
		return SpecialClassView{}, err
	}
	entry.Profiles, entry.DefaultProfile = view.StorageClasses, view.DefaultProfile
	if stored.Type != TypeS3 {
		return entry, nil
	}
	entry.MountProfile = stored.MountProfile
	if entry.MountProfile == "" {
		entry.MountProfile = "default"
	}
	entry.MountOptions = append([]string{}, stored.MountOptions...)
	entry.EffectiveMountOptions, entry.EffectiveBackendOptions, _, err = ResolveS3MountOptions(stored.MountProfile, stored.VFSCacheMode, stored.MountOptions)
	if err != nil {
		return SpecialClassView{}, err
	}
	return entry, nil
}

const storageAccessKeyMask = "*****"

func MaskAccessKeyID(value string) string {
	if len(value) <= 4 {
		return storageAccessKeyMask
	}
	return value[:2] + storageAccessKeyMask + value[len(value)-2:]
}

func isMaskedStorageAccessKeyID(value string) bool {
	return strings.Contains(value, storageAccessKeyMask)
}
