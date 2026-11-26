package storageconfig

const (
	DescriptionAnnotation = "rlark.io/description"
	NameAnnotation        = "rlark.io/name"
	ReplicationLabel      = "rlark.io/storageclass-replication"
	DeliveryLabel         = "rlark.io/storageclass-delivery"
)

// ReplicationName identifies the primary declaration of a storage.
func ReplicationName(id string) string { return id + "-replication" }

// DeliveryName identifies the declaration delivered to the member cluster.
func DeliveryName(id string) string { return id + "-delivery" }

// StoredClass is the versioned storage declaration persisted by the Gateway.
type StoredClass struct {
	Version     string   `json:"version"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Clusters    []string `json:"clusters"`
	Provider    string   `json:"provider"`
	Endpoint    string   `json:"endpoint"`
	AccessKeyID string   `json:"access_key_id"`
	AccessKey   string   `json:"access_key_secret"`
	Bucket      string   `json:"bucket"`
	Region      string   `json:"region"`
	PathStyle   bool     `json:"path_style"`
	MountSettings
}

// MountSettings is embedded in StoredClass. Every field is
// optional, so the record stays on version v1.
type MountSettings struct {
	// Type is "s3", "smb" or "host-path". A record without it is "s3".
	Type          string `json:"type,omitempty"`
	MountEndpoint string `json:"mount_endpoint,omitempty"`
	// ClusterMountEndpoints gives a cluster its own mount endpoint, for
	// example a cloud's internal address for the clusters in that cloud.
	ClusterMountEndpoints map[string]string `json:"cluster_mount_endpoints,omitempty"`
	Prefix                string            `json:"prefix,omitempty"`
	VFSCacheMode          string            `json:"vfs_cache_mode,omitempty"`
	MountProfile          string            `json:"mount_profile,omitempty"`
	MountOptions          []string          `json:"mount_options,omitempty"`
	Host                  string            `json:"host,omitempty"`
	Share                 string            `json:"share,omitempty"`
	SubPath               string            `json:"sub_path,omitempty"`
	Username              string            `json:"username,omitempty"`
	Password              string            `json:"password,omitempty"`
	Domain                string            `json:"domain,omitempty"`
	// Path and FSType describe a host-path storage: the directory the nodes
	// that have the storage have mounted, and the type of the file system it
	// must be on. Which nodes have it the driver's node plugin finds out.
	Path   string `json:"path,omitempty"`
	FSType string `json:"fs_type,omitempty"`
}
