package storageconfig

import (
	"context"
	"fmt"
	"github.com/rlinf/rlark/apps/rlark/astribot/storageref"
	"github.com/rlinf/rlark/apps/rlark/pkg/gateway/storage"
	"k8s.io/apimachinery/pkg/util/validation"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
)

// ClassRequest defines the payload for creating a rclone-backed
// StorageClass across one or more clusters. The S3-compatible remote config is
// stored in a per-class Secret referenced by the StorageClass.
type ClassRequest struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Description string `json:"description"`

	Clusters []string `json:"clusters"`

	Provider        string `json:"provider"`
	Endpoint        string `json:"endpoint"`
	AccessKeyId     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	PathStyle       bool   `json:"path_style"`
	SpecialClassRequest
}

// SpecialClassRequest is embedded in ClassRequest. Type
// selects the storage: "s3" (default) for S3-compatible object storage, "smb"
// for an SMB/CIFS share, "host-path" for a directory the nodes have mounted.
type SpecialClassRequest struct {
	Type string `json:"type"`

	MountEndpoint string `json:"mount_endpoint"`
	// ClusterMountEndpoints replaces the stored per-cluster endpoints when
	// given; omitted on update keeps them.
	ClusterMountEndpoints *map[string]string `json:"cluster_mount_endpoints"`
	Prefix                string             `json:"prefix"`
	VFSCacheMode          string             `json:"vfs_cache_mode"`
	MountProfile          string             `json:"mount_profile"`
	MountOptions          *[]string          `json:"mount_options"`
	// CredentialsFrom names an existing S3 storage whose provider, endpoint,
	// region and access key a new storage uses, so that several mounts of one
	// bucket (read, archive) are declared without entering the key again.
	CredentialsFrom string `json:"credentials_from"`

	Host     string `json:"host"`
	Share    string `json:"share"`
	SubPath  string `json:"sub_path"`
	Username string `json:"username"`
	Password string `json:"password"`
	Domain   string `json:"domain"`

	Path   string `json:"path"`
	FSType string `json:"fs_type"`
}

const (
	// TypeS3 is the default for records written before types existed.
	TypeS3       = "s3"
	TypeSMB      = "smb"
	TypeHostPath = storageref.TypeHostPath
)

func EffectiveVFSCacheMode(mode string) string {
	if mode == "" {
		return "writes"
	}
	return mode
}

func CheckType(value string) error {
	switch NormalizeType(value) {
	case TypeS3, TypeSMB, TypeHostPath:
		return nil
	}
	return fmt.Errorf("unsupported storage type %q", value)
}

// RequiredSpecialFields lists the missing fields of a storage that is
// not S3, besides its name.
func RequiredSpecialFields(req ClassRequest) []string {
	var fields []string
	if NormalizeType(req.Type) == TypeHostPath {
		if strings.TrimSpace(req.Path) == "" {
			fields = append(fields, "path")
		}
		return fields
	}
	for _, field := range []struct{ name, value string }{
		{"host", req.Host}, {"share", req.Share}, {"username", req.Username}, {"password", req.Password},
	} {
		if strings.TrimSpace(field.value) == "" {
			fields = append(fields, field.name)
		}
	}
	return fields
}

// ValidateSpecial checks a storage that is not S3.
func ValidateSpecial(ctx context.Context, stored StoredClass, port string) error {
	if stored.Type == TypeSMB {
		return validateSMBReachable(ctx, stored.Host, port)
	}
	// A host-path directory lives on the nodes; a Pod that cannot mount it
	// reports FailedMount on its node.
	return nil
}

func CredentialErrorMessage(err error, port string) string {
	if strings.Contains(err.Error(), "connect smb server") {
		return "smb server is not reachable on port " + port
	}
	return "storage credentials cannot access the configured bucket"
}

// NormalizeSpecial normalizes a storage that is not S3 (ours is then
// true), and rejects the fields of another type on any storage.
func NormalizeSpecial(req ClassRequest, name string, clusters []string) (stored StoredClass, ours bool, err error) {
	storageType := NormalizeType(req.Type)
	if storageType != TypeHostPath && (strings.TrimSpace(req.Path) != "" || strings.TrimSpace(req.FSType) != "") {
		return StoredClass{}, false, fmt.Errorf("path and fs_type are only supported for host-path storage")
	}
	if storageType != TypeS3 && req.ClusterMountEndpoints != nil && len(*req.ClusterMountEndpoints) != 0 {
		return StoredClass{}, false, fmt.Errorf("cluster_mount_endpoints is only supported for S3 storage")
	}
	switch storageType {
	case TypeHostPath:
		stored, err = normalizeHostPathStorage(req, name, clusters)
		return stored, true, err
	case TypeSMB:
		if strings.TrimSpace(req.MountEndpoint) != "" {
			return StoredClass{}, false, fmt.Errorf("mount_endpoint is only supported for S3 storage")
		}
		if strings.TrimSpace(req.Prefix) != "" || strings.TrimSpace(req.VFSCacheMode) != "" || req.MountProfile != "" || req.MountOptions != nil {
			return StoredClass{}, false, fmt.Errorf("prefix and vfs_cache_mode are only supported for S3 storage")
		}
		stored = StoredClass{Version: DeclarationVersion, Name: name, Description: req.Description, Clusters: clusters}
		if err := normalizeSMBFields(req, &stored); err != nil {
			return StoredClass{}, false, err
		}
		return stored, true, nil
	}
	return StoredClass{}, false, nil
}

type RequiredFieldsError struct {
	Fields []string
}

func (e *RequiredFieldsError) Error() string {
	return strings.Join(e.Fields, ", ") + " are required"
}

func requiredStorageClassFields(req ClassRequest) []string {
	var fields []string
	if strings.TrimSpace(req.Name) == "" {
		fields = append(fields, "name")
	}
	if NormalizeType(req.Type) != TypeS3 {
		return append(fields, RequiredSpecialFields(req)...)
	}
	if strings.TrimSpace(req.Bucket) == "" {
		fields = append(fields, "bucket")
	}
	if strings.TrimSpace(req.AccessKeySecret) == "" {
		fields = append(fields, "access_key_secret")
	}
	return fields
}

func NormalizeClass(req ClassRequest) (StoredClass, error) {
	name := strings.TrimSpace(req.Name)
	if err := CheckType(req.Type); err != nil {
		return StoredClass{}, err
	}
	if fields := requiredStorageClassFields(req); len(fields) != 0 {
		return StoredClass{}, &RequiredFieldsError{Fields: fields}
	}
	clusters := make([]string, 0, len(req.Clusters))
	for _, cluster := range req.Clusters {
		cluster = strings.TrimSpace(cluster)
		if cluster == "" {
			return StoredClass{}, fmt.Errorf("cluster IDs must not be empty")
		}
		if problems := validation.IsDNS1123Label(cluster); len(problems) != 0 {
			return StoredClass{}, fmt.Errorf("invalid cluster ID %q: %s", cluster, strings.Join(problems, "; "))
		}
		clusters = append(clusters, cluster)
	}
	if len(clusters) == 0 {
		return StoredClass{}, fmt.Errorf("clusters is required")
	}
	sort.Strings(clusters)
	clusters = slices.Compact(clusters)
	if stored, ours, err := NormalizeSpecial(req, name, clusters); ours || err != nil {
		return stored, err
	}
	// Provider 必须来自已知目录: rclone 对未知 provider 只记录告警并回退到通用
	// 配置, 错误取值在数据面只表现为挂载后的 I/O 错误. 这里同时把历史客户端提交
	// 的展示名(如 "Aliyun OSS")改写为 canonical 取值.
	provider, ok := storage.LookupProvider(req.Provider)
	if !ok {
		return StoredClass{}, fmt.Errorf("unsupported provider %q", req.Provider)
	}
	// Endpoint 只描述服务端地址, 不含 bucket: AWS SDK 与 rclone 都会自行按
	// virtual-hosted 规则拼接 bucket.
	endpoint, err := storage.NormalizeEndpoint(provider.Value, req.Endpoint, req.Region)
	if err != nil {
		return StoredClass{}, err
	}
	return normalizeS3StorageMount(req, StoredClass{
		Version: DeclarationVersion, Name: name, Description: req.Description, Clusters: clusters,
		Provider: provider.Value, Endpoint: endpoint, AccessKeyID: req.AccessKeyId, AccessKey: req.AccessKeySecret,
		Bucket: req.Bucket, Region: strings.TrimSpace(req.Region),
		PathStyle: storage.EffectivePathStyle(provider.Value, req.PathStyle),
	})
}

func normalizeS3StorageMount(req ClassRequest, stored StoredClass) (StoredClass, error) {
	return NormalizeS3Mount(S3MountRequest{
		Region: req.Region, MountEndpoint: req.MountEndpoint, Prefix: req.Prefix,
		MountProfile: req.MountProfile, VFSCacheMode: req.VFSCacheMode, MountOptions: req.MountOptions,
		ClusterMountEndpoints: req.ClusterMountEndpoints,
	}, stored, storage.NormalizeEndpoint)
}
func ValidateClassCredentials(ctx context.Context, stored StoredClass) error {
	if stored.Type != TypeS3 {
		return ValidateSpecial(ctx, stored, SMBPort)
	}
	config := storage.DefaultConfig()
	config.Provider = stored.Provider
	config.Endpoint = stored.Endpoint
	config.AccessKeyId = stored.AccessKeyID
	config.SecretAccessKey = stored.AccessKey
	config.Region = stored.Region
	config.UsePathStyle = stored.PathStyle
	config.Bucket = stored.Bucket
	client, err := storage.NewClient(config)
	if err != nil {
		return fmt.Errorf("create storage client: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, (10 * time.Second))
	defer cancel()
	if err := client.ValidateBucketCredentials(ctx); err != nil {
		return fmt.Errorf("head storage bucket: %w", err)
	}
	return nil
}

// CopyCredentials uses the endpoint and credentials of an existing declaration.
func (req *ClassRequest) CopyCredentials(source StoredClass) {
	req.Provider, req.Endpoint, req.Region, req.PathStyle = source.Provider, source.Endpoint, source.Region, source.PathStyle
	req.AccessKeyId, req.AccessKeySecret = source.AccessKeyID, source.AccessKey
}

// ApplyUpdateDefaults preserves existing fields omitted from an update.
func (req *ClassRequest) ApplyUpdateDefaults(existing StoredClass) error {
	if existing.Type != NormalizeType(req.Type) {
		return fmt.Errorf("storage class type cannot be changed")
	}
	switch existing.Type {
	case TypeSMB:
		if req.Password == "" {
			req.Password = existing.Password
		}
	case TypeS3:
		if req.AccessKeyId == "" || isMaskedStorageAccessKeyID(req.AccessKeyId) {
			req.AccessKeyId = existing.AccessKeyID
		}
		if req.AccessKeySecret == "" {
			req.AccessKeySecret = existing.AccessKey
		}
		if req.MountOptions == nil {
			options := append([]string{}, existing.MountOptions...)
			req.MountOptions = &options
		}
		if req.VFSCacheMode == "" && (req.MountProfile == "" || req.MountProfile == existing.MountProfile) {
			req.VFSCacheMode = EffectiveVFSCacheMode(existing.VFSCacheMode)
		}
		if req.MountProfile == "" {
			req.MountProfile = existing.MountProfile
		}
		if req.ClusterMountEndpoints == nil {
			endpoints := maps.Clone(existing.ClusterMountEndpoints)
			req.ClusterMountEndpoints = &endpoints
		}
	}
	return nil
}

const SMBPort = "445"
