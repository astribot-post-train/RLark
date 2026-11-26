package clusterupgrade

import (
	"fmt"
	"slices"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Preflight validates the same named/selector precedence as the upstream
// controller and stops an Astribot target before the stock installer can run.
func Preflight(spec rlark.UpgradeJobSpec, clusters []rlark.Cluster) error {
	candidates := make(map[string]rlark.Cluster, len(clusters))
	for _, cluster := range clusters {
		candidates[cluster.Name] = cluster
	}
	targets := slices.Clone(spec.ClusterNames)
	if len(targets) == 0 {
		selector := labels.Everything()
		if spec.ClusterSelector != nil {
			var err error
			selector, err = metav1.LabelSelectorAsSelector(spec.ClusterSelector)
			if err != nil {
				return fmt.Errorf("invalid upgrade cluster selector: %w", err)
			}
		}
		for _, cluster := range clusters {
			if selector.Matches(labels.Set(cluster.Labels)) {
				targets = append(targets, cluster.Name)
			}
		}
		slices.Sort(targets)
	}
	if len(targets) == 0 {
		return fmt.Errorf("no clusters match the upgrade targets")
	}
	for _, name := range targets {
		cluster, ok := candidates[name]
		if !ok {
			return fmt.Errorf("upgrade target cluster %s does not exist", name)
		}
		switch driver := cluster.Annotations[DriverAnnotation]; driver {
		case "":
		case DriverImageOnly:
			if _, err := ParseImages(spec.ConfigPatch); err != nil {
				return fmt.Errorf("upgrade target %s: %w", name, err)
			}
			// The Job launcher and scoped node-agent endpoint are a separate
			// activation gate. Never send an image-only target to stock execution.
			return fmt.Errorf("upgrade target %s: the Astribot upgrade executor is not enabled", name)
		default:
			return fmt.Errorf("upgrade target %s has an unsupported upgrade driver", name)
		}
	}
	return nil
}
