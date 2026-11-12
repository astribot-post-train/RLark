package gateway

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A storage class is mounted with rclone VFS options. One set does not fit
// every use of the same bucket: training reads large files that never change,
// archiving writes files that must be on the object store when the write
// returns. A class therefore names a mount profile, or lists its own options.
const (
	// mountProfileDefault is what every class had before profiles: writes go
	// through a local cache and reach the object store about a second later.
	mountProfileDefault = "default"
	// mountProfileRead is for data that is only read: listings are cached for
	// an hour, requests are rate-limited and retried, checksums are not
	// fetched, nothing is cached on the node's disk.
	mountProfileRead = "read"
	// mountProfileArchive is for writers that must not report a file written
	// before the object store has it: no write cache, a listing that is at
	// most a few seconds old.
	mountProfileArchive = "archive"
)

var s3MountProfiles = map[string][]string{
	mountProfileDefault: s3MountOptions,
	mountProfileRead: {"vfs-cache-mode=off", "read-only", "dir-cache-time=1h", "attr-timeout=1h", "ignore-checksum", "tpslimit=40",
		"low-level-retries=20", "buffer-size=64M", "vfs-read-chunk-size=32M", "transfers=8", "checkers=16", "fast-list"},
	mountProfileArchive: {"vfs-cache-mode=off", "dir-cache-time=5s", "attr-timeout=5s", "ignore-checksum", "low-level-retries=20",
		"transfers=8", "checkers=16", "fast-list"},
}

// A mount option is an rclone flag without its dashes, optionally with a
// value. It ends up on the CSI driver's rclone command line, so anything that
// is not a plain flag is refused.
var mountOptionPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}(=[A-Za-z0-9._:/,-]{1,128})?$`)

// normalizeMount validates the profile and the explicit options of a request.
func normalizeMount(storageType, profile string, options []string) (string, []string, error) {
	profile = strings.TrimSpace(profile)
	if profile == mountProfileDefault {
		profile = ""
	}
	if profile != "" {
		if _, ok := s3MountProfiles[profile]; !ok || storageType != storageTypeS3 {
			return "", nil, fmt.Errorf("unsupported mount profile %q for %s storage; s3 has default, read and archive", profile, storageType)
		}
	}
	if len(options) > 32 {
		return "", nil, fmt.Errorf("at most 32 mount options")
	}
	var cleaned []string
	for _, option := range options {
		option = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(option), "-"))
		if !mountOptionPattern.MatchString(option) {
			return "", nil, fmt.Errorf("invalid mount option %q", option)
		}
		cleaned = append(cleaned, option)
	}
	if len(cleaned) != 0 && profile != "" {
		return "", nil, fmt.Errorf("give a mount profile or mount options, not both")
	}
	return profile, cleaned, nil
}

// mountOptionsFor returns what the class is mounted with: its own options,
// else its profile's, else the storage type's defaults.
func mountOptionsFor(stored storedStorageClass) []string {
	if len(stored.MountOptions) != 0 {
		return slices.Clone(stored.MountOptions)
	}
	if stored.Type == storageTypeSMB {
		return slices.Clone(smbMountOptions)
	}
	if options, ok := s3MountProfiles[stored.MountProfile]; ok {
		return slices.Clone(options)
	}
	return slices.Clone(s3MountOptions)
}
