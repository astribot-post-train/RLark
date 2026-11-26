package clusterupgrade

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/rlarkadm/types"
	yamlv3 "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/validation"
	yaml "sigs.k8s.io/yaml"
)

const (
	BatchConfigKind           = "BatchDeployConfig"
	BatchConfigAPIVersion     = "astribot.rlark.io/v1alpha1"
	ManagedBatchAnnotation    = "rlark.io/managed-upgrade-batch"
	ChildClusterUIDAnnotation = "rlark.io/upgrade-target-cluster-uid"
)

type ManagedBatchConfig struct {
	ExecutorImage string `json:"executorImage"`
	DeployConfig  string `json:"deployConfig"`
}

type batchConfig struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Stock      *string             `json:"stockDeployConfig,omitempty"`
	Managed    *ManagedBatchConfig `json:"managed,omitempty"`
}

type BatchPlan struct {
	defaultImage, legacyDeployConfig string
	timeout                          int64
	batch                            *batchConfig
}

type BatchTarget struct {
	ExecutorImage, DeployConfig string
	Managed                     bool
}

// ParseBatchPlan preserves the existing stock input and accepts a versioned
// envelope for batches containing managed members. The envelope is resolved
// before constructing each upstream ClusterUpgradeJob.
func ParseBatchPlan(spec rlark.UpgradeJobSpec) (BatchPlan, error) {
	if spec.Image == "" {
		return BatchPlan{}, errors.New("spec.image is required")
	}
	plan := BatchPlan{defaultImage: spec.Image, legacyDeployConfig: spec.ConfigPatch, timeout: spec.Strategy.TimeoutSeconds}
	var meta struct {
		Kind       string `yaml:"kind"`
		APIVersion string `yaml:"apiVersion"`
	}
	if err := yamlv3.Unmarshal([]byte(spec.ConfigPatch), &meta); err != nil {
		return BatchPlan{}, errors.New("invalid upgrade configuration YAML")
	}
	if meta.Kind != BatchConfigKind && meta.APIVersion != BatchConfigAPIVersion {
		return plan, nil
	}
	decoder := yamlv3.NewDecoder(strings.NewReader(spec.ConfigPatch))
	var document yamlv3.Node
	if err := decoder.Decode(&document); err != nil || decoder.Decode(new(yamlv3.Node)) != io.EOF {
		return BatchPlan{}, errors.New("BatchDeployConfig must contain exactly one YAML document")
	}
	var batch batchConfig
	if err := yaml.UnmarshalStrict([]byte(spec.ConfigPatch), &batch); err != nil ||
		batch.Kind != BatchConfigKind || batch.APIVersion != BatchConfigAPIVersion {
		return BatchPlan{}, errors.New("invalid Astribot BatchDeployConfig")
	}
	if batch.Stock != nil && *batch.Stock != "" {
		stock, err := types.ParseDeployConfig([]byte(*batch.Stock))
		if err != nil {
			return BatchPlan{}, errors.New("invalid stock DeployConfig in batch")
		}
		if stock.Plane != types.PlaneData || stock.Kubernetes == nil {
			return BatchPlan{}, errors.New("stock batches require a Kubernetes data-plane DeployConfig")
		}
		if err := stock.ValidateForUpgrade(); err != nil {
			return BatchPlan{}, fmt.Errorf("stock DeployConfig: %w", err)
		}
	}
	if batch.Managed != nil {
		if _, err := pinnedImage(batch.Managed.ExecutorImage); err != nil {
			return BatchPlan{}, errors.New("managed executorImage must be pinned by sha256 digest")
		}
		if _, _, err := parseImagePlan(batch.Managed.DeployConfig, true); err != nil {
			return BatchPlan{}, fmt.Errorf("managed DeployConfig: %w", err)
		}
	}
	plan.batch = &batch
	return plan, nil
}

func (p BatchPlan) ForCluster(cluster rlark.Cluster) (BatchTarget, error) {
	if cluster.Annotations[DriverAnnotation] == DriverImageOnly {
		if p.batch == nil || p.batch.Managed == nil {
			if _, err := ParseImages(p.legacyDeployConfig); err != nil && p.batch == nil {
				return BatchTarget{}, err
			}
			return BatchTarget{}, errors.New("managed upgrade requires an Astribot BatchDeployConfig")
		}
		if cluster.Annotations[ManagedBatchAnnotation] != "enabled" || cluster.UID == "" || cluster.DeletionTimestamp != nil ||
			cluster.Spec.AgentNamespace != cluster.Name || len(validation.IsDNS1123Label(cluster.Name)) != 0 ||
			cluster.Spec.NodeAgents == nil || !cluster.Spec.NodeAgents.Enabled {
			return BatchTarget{}, errors.New("managed batch upgrades are not enabled for this cluster")
		}
		if _, err := pinnedImage(cluster.Spec.NodeAgents.Image); err != nil {
			return BatchTarget{}, errors.New("the current node Agent image must be pinned by sha256 digest")
		}
		if _, err := managedGrantTTL(p.timeout); err != nil {
			return BatchTarget{}, err
		}
		return BatchTarget{ExecutorImage: p.batch.Managed.ExecutorImage, DeployConfig: p.batch.Managed.DeployConfig, Managed: true}, nil
	}
	if err := ValidateStockTarget(cluster); err != nil {
		return BatchTarget{}, err
	}
	if p.batch == nil {
		return BatchTarget{ExecutorImage: p.defaultImage, DeployConfig: p.legacyDeployConfig}, nil
	}
	if p.batch.Stock == nil {
		return BatchTarget{}, errors.New("the batch has no stockDeployConfig for a stock member")
	}
	return BatchTarget{ExecutorImage: p.defaultImage, DeployConfig: *p.batch.Stock}, nil
}

// managedGrantTTL covers the Job and its rollback buffer.
func managedGrantTTL(timeout int64) (time.Duration, error) {
	if timeout == 0 {
		timeout = 600
	}
	if timeout < 60 || timeout > 3600 {
		return 0, errors.New("managed upgrade timeout must be between 60 and 3600 seconds")
	}
	return time.Duration(2*timeout+60) * time.Second, nil
}
