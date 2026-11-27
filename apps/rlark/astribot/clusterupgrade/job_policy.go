package clusterupgrade

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rlinf/rlark/apps/rlark/astribot/nodeagent"
	"github.com/rlinf/rlark/apps/rlark/pkg/rlarkadm/types"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	PolicyConfigMap        = "rlark-agent-upgrade-policy"
	ClusterOnlyMode        = "astribot-cluster-only"
	ManagedNodeMode        = "astribot-managed-node"
	JobServiceAccount      = "rlark-upgrade"
	StockJobServiceAccount = "rlark-agent"
)

// JobPolicy is provisioned by a member cluster administrator. UpgradeJob
// callers cannot select the policy or grant an image the executor's identity.
type JobPolicy struct {
	ExecutorImage string
	Deployment    string
	Mode          string
	BrokerOrigin  string
	BrokerCA      string
}

// ResolveJobPolicy returns a nil policy when the member has neither the Astribot
// lease nor an executor policy. The caller still has to check stock eligibility.
func ResolveJobPolicy(ctx context.Context, kube client.Reader) (*JobPolicy, error) {
	var config corev1.ConfigMap
	err := kube.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: PolicyConfigMap}, &config)
	if apierrors.IsNotFound(err) {
		var lease coordinationv1.Lease
		err = kube.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: LeaseName}, &lease)
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("verify member upgrade mode: %w", err)
		}
		return nil, errors.New("member has an Astribot upgrade lease but no enabled executor policy")
	}
	if err != nil {
		return nil, fmt.Errorf("read member upgrade policy: %w", err)
	}
	mode := config.Data["mode"]
	if mode != DriverStock && ((mode != ClusterOnlyMode && mode != ManagedNodeMode) || len(validation.IsDNS1123Subdomain(config.Data["deployment"])) != 0) {
		return nil, errors.New("invalid member upgrade mode or Deployment")
	}
	image, err := pinnedImage(config.Data["executorImage"])
	if err != nil {
		return nil, errors.New("member upgrade policy requires a digest-pinned executor image")
	}
	policy := &JobPolicy{ExecutorImage: image, Deployment: config.Data["deployment"], Mode: mode}
	if mode == ManagedNodeMode {
		policy.BrokerOrigin, policy.BrokerCA = config.Data["brokerOrigin"], config.Data["brokerCA"]
		if _, err := brokerTLSClient(policy.BrokerOrigin, policy.BrokerCA); err != nil {
			return nil, err
		}
	}
	return policy, nil
}

func (p JobPolicy) Validate(ctx context.Context, kube client.Reader, image, config string) error {
	if image != p.ExecutorImage {
		return errors.New("upgrade image does not match the trusted member upgrade executor")
	}
	if p.Mode == DriverStock {
		parsed, err := types.ParseDeployConfig([]byte(config))
		if err != nil || parsed.Plane != types.PlaneData || parsed.Kubernetes == nil {
			return errors.New("locked stock upgrade requires a Kubernetes data-plane DeployConfig")
		}
		return parsed.ValidateForUpgrade()
	}
	if _, _, err := parseImagePlan(config, p.Mode == ManagedNodeMode); err != nil {
		return err
	}
	if p.Mode == ManagedNodeMode {
		return nil
	}
	return ValidateClusterOnly(ctx, kube)
}

func ValidateClusterOnly(ctx context.Context, kube client.Reader) error {
	var workloads appsv1.DaemonSetList
	if err := kube.List(ctx, &workloads, client.InNamespace(Namespace)); err != nil {
		return err
	}
	for _, ds := range workloads.Items {
		if ds.Name == nodeagent.Name || ds.Name == "rlark-node-agent" {
			return errors.New("cluster-only upgrade cannot change a member with node Agents")
		}
		for _, container := range ds.Spec.Template.Spec.Containers {
			if _, _, nodeMode := argument(slices.Concat(container.Command, container.Args), "--mode", "node"); nodeMode {
				return errors.New("cluster-only upgrade cannot change a member with node Agents")
			}
		}
	}
	return nil
}

// ValidateExecutorImage requires a canonical digest-pinned executor reference.
func ValidateExecutorImage(image string) error {
	_, err := pinnedImage(image)
	return err
}
