package storageconfig

// Rendering a storage declaration into what the distribution controllers
// apply. A storage has one primary Replication declaration, which holds the
// stored declaration and reaches every cluster that mounts it through the
// storage's default endpoint, and one endpoint declaration per cluster that
// mounts it through an endpoint of its own. Each declaration delivers, to each
// of its clusters:
//
//   - the storage's View (ConfigMap <id>-storage in the cluster's namespace of
//     the management plane), which Job and Addon volumes are resolved against;
//   - a Delivery of one StorageClass per mount profile to the cluster itself:
//     rclone's, with its Secret, for object storage and SMB; the node
//     directory driver's (apps/rlark/astribot/nodepath) for a host-path storage.

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"text/template"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/rlinf/rlark/apps/rlark/astribot/nodepath"
	"github.com/rlinf/rlark/apps/rlark/astribot/storageref"
	"github.com/rlinf/rlark/apps/rlark/pkg/distribution"
)

// EndpointLabel marks an endpoint declaration; its value is the
// storage's id.
const EndpointLabel = "rlark.io/storageclass-endpoint"

func storageEndpointSecretName(id, cluster string) string { return id + "-at-" + cluster }

// storageProfileClass is one StorageClass of a storage: the one a volume gets
// when it chooses the profile.
type storageProfileClass struct {
	Profile      string
	Name         string
	Secret       string
	MountOptions []string
	Backend      map[string]string
}

// storageOwnProfile is the profile of a volume that names none.
func storageOwnProfile(stored StoredClass) string {
	if stored.Type == storageref.TypeS3 && stored.MountProfile != "" {
		return stored.MountProfile
	}
	return storageref.ProfileDefault
}

// storageProfileClasses lists a storage's StorageClasses. The storage's own
// profile keeps the class sc-<id> it always had; every other profile is
// sc-<id>-<profile>; the own profile comes first. A profile the storage's mount options cannot be combined
// with (a record from before profiles were chosen per volume) is not offered.
func storageProfileClasses(id string, stored StoredClass) ([]storageProfileClass, error) {
	own := storageOwnProfile(stored)
	profiles := []string{own}
	for _, profile := range storageref.Profiles(stored.Type) {
		if profile != own {
			profiles = append(profiles, profile)
		}
	}
	var classes []storageProfileClass
	for _, profile := range profiles {
		class := storageProfileClass{Profile: profile, Name: id + "-" + profile, Secret: id + "-" + profile + "-secret"}
		if profile == own {
			class.Name, class.Secret = id, id+"-secret"
		}
		switch stored.Type {
		case storageref.TypeHostPath:
			// The node plugin binds the directory read-only for the read profile.
		case storageref.TypeSMB:
			class.MountOptions = slices.Clone(smbMountOptions)
			if profile == storageref.ProfileRead {
				class.MountOptions = append(class.MountOptions, "read-only=true")
			}
		default:
			cacheMode := ""
			if profile == own {
				cacheMode = stored.VFSCacheMode
			}
			options, backend, _, err := ResolveS3MountOptions(profile, cacheMode, stored.MountOptions)
			if err != nil {
				if profile == own {
					return nil, err
				}
				continue
			}
			class.MountOptions, class.Backend = options, backend
		}
		classes = append(classes, class)
	}
	return classes, nil
}

// View is what every cluster of the storage resolves volumes against.
func View(id string, stored StoredClass) (storageref.View, error) {
	view := storageref.View{Name: stored.Name, Type: stored.Type, DefaultProfile: storageOwnProfile(stored)}
	if stored.Type == storageref.TypeHostPath {
		// The nodes the driver's node plugin found to have it.
		view.NodeSelector = map[string]string{nodepath.NodeLabel(id): "true"}
	}
	classes, err := storageProfileClasses(id, stored)
	if err != nil {
		return storageref.View{}, err
	}
	view.StorageClasses = make(map[string]string, len(classes))
	for _, class := range classes {
		view.StorageClasses[class.Profile] = class.Name
	}
	return view, nil
}

// storageMountEndpoint is the endpoint the cluster's rclone mounts use.
func storageMountEndpoint(stored StoredClass, cluster string) string {
	if endpoint := stored.ClusterMountEndpoints[cluster]; endpoint != "" {
		return endpoint
	}
	if stored.MountEndpoint != "" {
		return stored.MountEndpoint
	}
	return stored.Endpoint
}

// storageDelivery renders the Delivery of a storage's StorageClasses to one
// cluster, with the rclone remote reaching the storage through mountEndpoint.
func storageDelivery(id string, stored StoredClass, mountEndpoint string) (v1.Secret, error) {
	classes, err := storageProfileClasses(id, stored)
	if err != nil {
		return v1.Secret{}, err
	}
	remote, remotePath := "s3", stored.Bucket
	if stored.Prefix != "" {
		remotePath += "/" + stored.Prefix
	}
	var base strings.Builder
	switch stored.Type {
	case storageref.TypeHostPath:
		return storageHostPathDelivery(id, stored, classes)
	case storageref.TypeSMB:
		// rclone's smb backend only accepts an obscured password.
		pass, err := RcloneObscure(stored.Password)
		if err != nil {
			return v1.Secret{}, fmt.Errorf("obscure smb password: %w", err)
		}
		data := struct{ Host, User, Pass, Domain string }{stored.Host, stored.Username, pass, stored.Domain}
		if err := smbSecretTmpl.Execute(&base, data); err != nil {
			return v1.Secret{}, fmt.Errorf("render secret config: %w", err)
		}
		remote, remotePath = "smb", smbRemotePath(stored)
	default:
		// The remote is "s3" for every class: a StorageClass's parameters are
		// immutable, and the node plugin (astribot/csi-rclone-ks3) gives each
		// published volume its own remote, so classes never share one.
		request := struct {
			Provider, Endpoint, AccessKeyId, AccessKeySecret, Region string
			PathStyle                                                bool
		}{
			Provider: stored.Provider, Endpoint: mountEndpoint, AccessKeyId: stored.AccessKeyID, AccessKeySecret: stored.AccessKey,
			Region: stored.Region, PathStyle: stored.PathStyle,
		}
		if err := ssSecretTmpl.Execute(&base, request); err != nil {
			return v1.Secret{}, fmt.Errorf("render secret config: %w", err)
		}
	}
	annotations := map[string]string{DescriptionAnnotation: stored.Description}
	if problems := validation.IsValidLabelValue(stored.Name); len(problems) == 0 {
		annotations[NameAnnotation] = stored.Name
	}
	manifests := make([]any, 0, 2*len(classes))
	for _, class := range classes {
		config := base.String()
		for _, key := range slices.Sorted(maps.Keys(class.Backend)) {
			config += fmt.Sprintf("%s = %s\n", key, class.Backend[key])
		}
		classAnnotations := maps.Clone(annotations)
		classAnnotations[ProfileAnnotation] = class.Profile
		manifests = append(manifests,
			v1.Secret{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
				ObjectMeta: metav1.ObjectMeta{Name: class.Secret, Namespace: metav1.NamespaceSystem, Labels: map[string]string{"controller": "rlark"}},
				StringData: map[string]string{"remote": remote, "remotePath": remotePath, "configData": config},
			},
			storagev1.StorageClass{
				TypeMeta:   metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass"},
				ObjectMeta: metav1.ObjectMeta{Name: class.Name, Labels: map[string]string{"controller": "rlark"}, Annotations: classAnnotations},
				// Delete removes the PersistentVolume object with its claim; the driver's
				// DeleteVolume does nothing to the remote. Retain left one Released
				// volume per Task per submission.
				Provisioner: "rclone.csi.veloxpack.io", ReclaimPolicy: ptr.To(v1.PersistentVolumeReclaimDelete),
				VolumeBindingMode: ptr.To(storagev1.VolumeBindingImmediate), AllowVolumeExpansion: ptr.To(true),
				Parameters: map[string]string{
					"remote": remote, "remotePath": remotePath,
					"csi.storage.k8s.io/node-publish-secret-name":      class.Secret,
					"csi.storage.k8s.io/node-publish-secret-namespace": metav1.NamespaceSystem,
				},
				MountOptions: slices.Clone(class.MountOptions),
			})
	}
	return storageDeliverySecret(id, manifests)
}

// storageHostPathDelivery renders a host-path storage's StorageClasses: the
// node directory CSI driver (apps/rlark/astribot/nodepath) binds the storage's directory,
// and a claim binds once its Pod is scheduled, on a node that has it.
func storageHostPathDelivery(id string, stored StoredClass, classes []storageProfileClass) (v1.Secret, error) {
	manifests := make([]any, 0, len(classes))
	for _, class := range classes {
		annotations := map[string]string{DescriptionAnnotation: stored.Description, ProfileAnnotation: class.Profile}
		if problems := validation.IsValidLabelValue(stored.Name); len(problems) == 0 {
			annotations[NameAnnotation] = stored.Name
		}
		parameters := map[string]string{nodepath.ParameterRoot: stored.Path}
		if stored.FSType != "" {
			parameters[nodepath.ParameterFSType] = stored.FSType
		}
		if class.Profile == storageref.ProfileRead {
			parameters[nodepath.ParameterReadOnly] = "true"
		}
		manifests = append(manifests, storagev1.StorageClass{
			TypeMeta:   metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass"},
			ObjectMeta: metav1.ObjectMeta{Name: class.Name, Labels: map[string]string{"controller": "rlark", nodepath.StorageIDLabel: id}, Annotations: annotations},
			// Delete removes the PersistentVolume with its claim; the driver's
			// DeleteVolume leaves the directory.
			Provisioner: nodepath.Name, ReclaimPolicy: ptr.To(v1.PersistentVolumeReclaimDelete),
			VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
			Parameters:        parameters,
		})
	}
	return storageDeliverySecret(id, manifests)
}

// storageDeliverySecret wraps a storage's manifests for one cluster.
func storageDeliverySecret(id string, manifests []any) (v1.Secret, error) {
	storageManifests, err := yaml.Marshal(manifests)
	if err != nil {
		return v1.Secret{}, fmt.Errorf("marshal storage class manifests: %w", err)
	}
	deliveryConfig := distribution.Config{Apply: distribution.ApplyPolicy{Mode: "ServerSideApply", ConflictPolicy: "Fail", DeletionPolicy: "Delete"}}
	deliveryConfig.Security.AllowClusterScoped = true
	deliveryConfigYAML, err := yaml.Marshal(deliveryConfig)
	if err != nil {
		return v1.Secret{}, fmt.Errorf("marshal delivery config: %w", err)
	}
	return v1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   DeliveryName(id),
			Labels: map[string]string{DeliveryLabel: "true"},
		},
		Type: distribution.DeliverySecretType,
		Data: map[string][]byte{"config": deliveryConfigYAML, "manifests": storageManifests},
	}, nil
}

// ProfileAnnotation names the mount profile of a delivered StorageClass.
const ProfileAnnotation = "rlark.io/storage-profile"

func storageViewConfigMap(id string, view storageref.View) (v1.ConfigMap, error) {
	data, err := json.Marshal(view)
	if err != nil {
		return v1.ConfigMap{}, fmt.Errorf("marshal storage view: %w", err)
	}
	return v1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: storageref.ViewConfigMapName(id), Labels: map[string]string{storageref.ViewLabel: "true"}},
		Data:       map[string]string{storageref.ViewKey: string(data)},
	}, nil
}

// storageReplication renders one Replication declaration delivering the
// storage to clusters through mountEndpoint.
func storageReplication(namespace, name string, labels map[string]string, id string, stored StoredClass, clusters []string, mountEndpoint string) (*v1.Secret, error) {
	view, err := View(id, stored)
	if err != nil {
		return nil, err
	}
	viewConfigMap, err := storageViewConfigMap(id, view)
	if err != nil {
		return nil, err
	}
	// The Delivery comes first: the distribution engine applies in order.
	delivery, err := storageDelivery(id, stored, mountEndpoint)
	if err != nil {
		return nil, err
	}
	objects := []any{delivery, viewConfigMap}
	manifest, err := yaml.Marshal(objects)
	if err != nil {
		return nil, fmt.Errorf("marshal replication manifests: %w", err)
	}
	config := distribution.Config{Apply: distribution.ApplyPolicy{Mode: "ServerSideApply", ConflictPolicy: "Fail", DeletionPolicy: "Delete"}}
	config.Targets.Namespaces = &distribution.GlobSelector{Include: append([]string{}, clusters...), Exclude: []string{"rlark-system"}}
	configYAML, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal replication config: %w", err)
	}
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Type:       distribution.ReplicationSecretType,
		Data:       map[string][]byte{"config": configYAML, "manifests": manifest},
	}, nil
}

// RenderReplication renders the primary declaration: the
// stored declaration, delivered to the clusters that use the default endpoint.
func RenderReplication(namespace, id string, stored StoredClass) (*v1.Secret, error) {
	clusters := make([]string, 0, len(stored.Clusters))
	for _, cluster := range stored.Clusters {
		if _, own := stored.ClusterMountEndpoints[cluster]; !own {
			clusters = append(clusters, cluster)
		}
	}
	secret, err := storageReplication(namespace, ReplicationName(id), map[string]string{ReplicationLabel: "true"}, id, stored, clusters, storageMountEndpoint(stored, ""))
	if err != nil {
		return nil, err
	}
	storedJSON, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("marshal storage class: %w", err)
	}
	secret.Data["storageclass"] = storedJSON
	return secret, nil
}

// RenderEndpoints renders one declaration per cluster that mounts
// the storage through an endpoint of its own, sorted by cluster.
func RenderEndpoints(namespace, id string, stored StoredClass) ([]*v1.Secret, error) {
	clusters := slices.Sorted(maps.Keys(stored.ClusterMountEndpoints))
	secrets := make([]*v1.Secret, 0, len(clusters))
	for _, cluster := range clusters {
		secret, err := storageReplication(namespace, storageEndpointSecretName(id, cluster), map[string]string{EndpointLabel: id}, id, stored, []string{cluster}, storageMountEndpoint(stored, cluster))
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

// smbPasswordLine matches the obscured SMB password in a rendered rclone
// config. rclone obscures with a random IV, so the line differs on every render.
var smbPasswordLine = regexp.MustCompile(`(?m)^(\s*pass = ).*$`)

// RenderKey reduces a Replication declaration to what its render
// means, so that two renders of one stored declaration compare equal.
func RenderKey(data map[string][]byte) string {
	manifests, err := yaml.YAMLToJSON(data["manifests"])
	if err != nil {
		return ""
	}
	var objects []json.RawMessage
	if err := json.Unmarshal(manifests, &objects); err != nil {
		return ""
	}
	// The stored fields are compared by value, not by their order in the JSON. An endpoint declaration has none.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data["storageclass"], &fields)
	stored, _ := json.Marshal(fields)
	parts := []string{string(stored), string(data["config"])}
	for _, object := range objects {
		var delivery v1.Secret
		if err := json.Unmarshal(object, &delivery); err == nil && delivery.Type == distribution.DeliverySecretType {
			parts = append(parts, string(delivery.Data["config"]), string(smbPasswordLine.ReplaceAll(delivery.Data["manifests"], []byte("${1}*"))))
			continue
		}
		parts = append(parts, string(object))
	}
	return strings.Join(parts, "\x00")
}

// S3SecretTemplate is the rclone remote config rendered into the StorageClass
// Secret from a CreateStorageClassRequest.
const S3SecretTemplate = `
[s3]
type = s3
provider = {{ .Provider }}
endpoint = {{ .Endpoint }}
access_key_id = {{ .AccessKeyId }}
secret_access_key = {{ .AccessKeySecret }}
{{- if .Region }}
region = {{ .Region}}
{{- end }}
force_path_style = {{ .PathStyle }}
no_check_bucket = true
`

var ssSecretTmpl = template.Must(template.New("ss-secret").Parse(S3SecretTemplate))

// smbSecretTemplate is the rclone smb remote config. Pass must already be in
// rclone's obscured form.
const smbSecretTemplate = `
[smb]
type = smb
host = {{ .Host }}
user = {{ .User }}
pass = {{ .Pass }}
{{- if .Domain }}
domain = {{ .Domain }}
{{- end }}
`

var smbSecretTmpl = template.Must(template.New("smb-secret").Parse(smbSecretTemplate))

// smbRemotePath is the rclone path for an smb remote: "share" or "share/sub".
func smbRemotePath(stored StoredClass) string {
	if stored.SubPath == "" {
		return stored.Share
	}
	return stored.Share + "/" + stored.SubPath
}

// smbMountOptions are rclone VFS flags for the CSI mount.
var (
	smbMountOptions = []string{"vfs-cache-mode=writes", "dir-cache-time=5m", "attr-timeout=1m", "poll-interval=5s", "vfs-write-back=1s", "vfs-cache-max-age=10m", "buffer-size=64M", "transfers=8", "checkers=8"}
)
