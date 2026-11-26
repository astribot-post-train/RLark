package clusterupgrade

import (
	"context"
	"errors"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	PolicyConfigMap   = "rlark-agent-upgrade-policy"
	ClusterOnlyMode   = "astribot-cluster-only"
	JobServiceAccount = "rlark-upgrade"
)

// JobPolicy is provisioned by a member cluster administrator. UpgradeJob
// callers cannot select the policy or grant an image the executor's identity.
type JobPolicy struct {
	ExecutorImage string
	Deployment    string
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
	if config.Data["mode"] != ClusterOnlyMode || len(validation.IsDNS1123Subdomain(config.Data["deployment"])) != 0 {
		return nil, errors.New("invalid member upgrade mode or Deployment")
	}
	image, err := pinnedImage(config.Data["executorImage"])
	if err != nil {
		return nil, errors.New("member upgrade policy requires a digest-pinned executor image")
	}
	return &JobPolicy{ExecutorImage: image, Deployment: config.Data["deployment"]}, nil
}

func (p JobPolicy) Validate(ctx context.Context, kube client.Reader, image, config string) error {
	if image != p.ExecutorImage {
		return errors.New("upgrade image does not match the trusted member upgrade executor")
	}
	if _, err := ParseImages(config); err != nil {
		return err
	}
	return ValidateClusterOnly(ctx, kube)
}

func ValidateClusterOnly(ctx context.Context, kube client.Reader) error {
	var workloads appsv1.DaemonSetList
	if err := kube.List(ctx, &workloads, client.InNamespace(Namespace)); err != nil {
		return err
	}
	for _, ds := range workloads.Items {
		if ds.Name == "rlark-node-agent" {
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
