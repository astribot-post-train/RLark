package storageconfig

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/rlinf/rlark/apps/rlark/pkg/distribution"
	"github.com/rlinf/rlark/apps/rlark/pkg/gateway/storage"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const DeclarationVersion = "v1"

var storageClassIDPattern = regexp.MustCompile(`^sc-[0-9a-f]{16}$`)

func ParseClassID(name string) (string, bool) {
	const suffix = "-replication"
	if !strings.HasSuffix(name, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(name, suffix)
	return id, storageClassIDPattern.MatchString(id)
}

func ClassFromSecret(secret *v1.Secret) (string, StoredClass, error) {
	id, ok := ParseClassID(secret.Name)
	if !ok || secret.Labels[ReplicationLabel] != "true" || secret.Type != distribution.ReplicationSecretType {
		return "", StoredClass{}, fmt.Errorf("secret %q is not a storage class replication", secret.Name)
	}
	var stored StoredClass
	if err := json.Unmarshal(secret.Data["storageclass"], &stored); err != nil {
		return "", StoredClass{}, fmt.Errorf("decode stored storage class: %w", err)
	}
	if stored.Version != DeclarationVersion {
		return "", StoredClass{}, fmt.Errorf("unsupported storage class version %q", stored.Version)
	}
	if stored.Type = NormalizeType(stored.Type); stored.Type != TypeS3 {
		return id, stored, nil
	}
	// 读取历史记录时把 provider 的展示名改写为 canonical 取值, 并按该 provider
	// 实际支持的寻址方式修正 path_style, 使旧记录与新写入的记录行为一致.
	if provider, ok := storage.LookupProvider(stored.Provider); ok {
		stored.Provider = provider.Value
		stored.PathStyle = storage.EffectivePathStyle(provider.Value, stored.PathStyle)
	}
	return id, stored, nil
}

func ListClassSecrets(ctx context.Context, rawClient kubernetes.Interface, namespace string) ([]v1.Secret, error) {
	secrets, err := rawClient.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{ReplicationLabel: "true"}.AsSelector().String(),
	})
	if err != nil {
		return nil, err
	}
	return secrets.Items, nil
}

func ClassSecretByName(ctx context.Context, rawClient kubernetes.Interface, namespace, name string) (*v1.Secret, error) {
	secrets, err := ListClassSecrets(ctx, rawClient, namespace)
	if err != nil {
		return nil, err
	}
	for i := range secrets {
		_, stored, err := ClassFromSecret(&secrets[i])
		if err != nil {
			continue
		}
		if stored.Name == name {
			return &secrets[i], nil
		}
	}
	return nil, errors.NewNotFound(v1.Resource("secrets"), name)
}

var ErrClassIDAllocation = stderrors.New("failed to allocate storage class id")

// CreateClass allocates a stable internal identity and persists the primary declaration.
func CreateClass(ctx context.Context, client kubernetes.Interface, namespace string, stored StoredClass, actor string) (*v1.Secret, error) {
	for range 3 {
		id := "sc-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
		secret, err := RenderReplication(namespace, id, stored)
		if err != nil {
			return nil, err
		}
		tenant.SetIdentity(secret, tenant.DefaultID, actor)
		created, err := client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			continue
		}
		return created, err
	}
	return nil, ErrClassIDAllocation
}

// UpdateClass replaces the definition only if the declaration read for the edit is still current.
func UpdateClass(ctx context.Context, client kubernetes.Interface, identity *v1.Secret, stored StoredClass) error {
	id, _, err := ClassFromSecret(identity)
	if err != nil {
		return err
	}
	rebuilt, err := RenderReplication(identity.Namespace, id, stored)
	if err != nil {
		return err
	}
	update := identity.DeepCopy()
	update.Data = rebuilt.Data
	_, err = client.CoreV1().Secrets(update.Namespace).Update(ctx, update, metav1.UpdateOptions{})
	return err
}

// DeleteClass removes the exact declaration selected by the caller.
func DeleteClass(ctx context.Context, client kubernetes.Interface, identity *v1.Secret) error {
	return deleteSecret(ctx, client, identity)
}

func deleteSecret(ctx context.Context, client kubernetes.Interface, identity *v1.Secret) error {
	options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{}}
	if identity.UID != "" {
		options.Preconditions.UID = &identity.UID
	}
	if identity.ResourceVersion != "" {
		options.Preconditions.ResourceVersion = &identity.ResourceVersion
	}
	return client.CoreV1().Secrets(identity.Namespace).Delete(ctx, identity.Name, options)
}

// AddClusterToSiteClasses extends only S3 classes shared by all other clusters.
// A concurrent storage edit is re-read before computing the new membership.
func AddClusterToSiteClasses(ctx context.Context, client kubernetes.Interface, namespace, cluster string, others []string) ([]string, error) {
	secrets, err := ListClassSecrets(ctx, client, namespace)
	if err != nil {
		return nil, fmt.Errorf("list storages: %w", err)
	}
	var added []string
	for i := range secrets {
		identity := &secrets[i]
		var included string
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, err := client.CoreV1().Secrets(namespace).Get(ctx, identity.Name, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if current.UID != identity.UID || current.DeletionTimestamp != nil {
				return nil
			}
			id, stored, err := ClassFromSecret(current)
			if err != nil || stored.Type != TypeS3 {
				return nil
			}
			if !slices.Contains(stored.Clusters, cluster) {
				if slices.ContainsFunc(others, func(other string) bool { return !slices.Contains(stored.Clusters, other) }) {
					return nil
				}
				stored.Clusters = append(stored.Clusters, cluster)
				if stored.MountEndpoint != "" && stored.MountEndpoint != stored.Endpoint {
					if stored.ClusterMountEndpoints == nil {
						stored.ClusterMountEndpoints = map[string]string{}
					}
					stored.ClusterMountEndpoints[cluster] = stored.Endpoint
				}
				if err := UpdateClass(ctx, client, current, stored); err != nil {
					return err
				}
			}
			if err := SyncEndpointSecrets(ctx, client, namespace, id, &stored, current); err != nil {
				return err
			}
			included = stored.Name
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("give storage declaration %s to %s: %w", identity.Name, cluster, err)
		}
		if included != "" {
			added = append(added, included)
		}
	}
	return added, nil
}

// RerenderClasses rewrites every storage declaration whose rendered
// objects are not what this Gateway renders from its stored fields. A storage
// is otherwise rendered only when it is created or edited, so a change in the
// render (reclaim policy, mount options of a profile) would reach existing
// storages one manual edit at a time. One storage failing does not stop the rest.
func RerenderClasses(ctx context.Context, client kubernetes.Interface, namespace string) {
	logger := log.FromContext(ctx)
	secrets, err := ListClassSecrets(ctx, client, namespace)
	if err != nil {
		logger.Error(err, "list storage class replications to re-render")
		return
	}
	for i := range secrets {
		secret := &secrets[i]
		id, stored, err := ClassFromSecret(secret)
		if err != nil {
			logger.Error(err, "skip re-rendering storage class", "secret", secret.Name)
			continue
		}
		rebuilt, err := RenderReplication(secret.Namespace, id, stored)
		if err != nil {
			logger.Error(err, "re-render storage class", "storage", stored.Name)
			continue
		}
		if err := SyncEndpointSecrets(ctx, client, namespace, id, &stored, secret); err != nil {
			logger.Error(err, "re-render storage endpoint declarations", "storage", stored.Name)
		}
		if RenderKey(secret.Data) == RenderKey(rebuilt.Data) {
			continue
		}
		secret.Data = rebuilt.Data
		if _, err := client.CoreV1().Secrets(secret.Namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			logger.Error(err, "update re-rendered storage class", "storage", stored.Name)
			continue
		}
		logger.Info("re-rendered storage class", "storage", stored.Name, "storageClass", id)
	}
}
