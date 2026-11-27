package gateway

import (
	"context"
	"encoding/json"
	"errors"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// prepareUpgradeJobForCreate sets GenerateName and validates the image before
// the generic create handler persists the object.
func (g *Gateway) prepareUpgradeJobForCreate(ctx context.Context, body []byte) ([]byte, error) {
	var job rlarkv1alpha1.UpgradeJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, err
	}

	if job.Spec.Image == "" {
		return nil, errors.New("spec.image is required")
	}

	clusters, err := g.kubeClient.RlinfV1alpha1().Clusters().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errors.New("could not verify upgrade target clusters")
	}
	if err := clusterupgrade.Preflight(job.Spec, clusters.Items); err != nil {
		return nil, err
	}

	job.GenerateName = "up-"
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}

	return json.Marshal(&job)
}
