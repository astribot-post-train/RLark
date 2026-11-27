package nodeagent

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// ErrImageDeclarationChanged is terminal for the current attempt. A concurrent
// operator change must be reviewed before another attempt takes a new snapshot.
var ErrImageDeclarationChanged = errors.New("node Agent image declaration changed")

// ImageChange is the part of a Cluster declaration that a release may change.
// The UID and old image come from its snapshot, not the most recent read.
type ImageChange struct {
	Cluster  string
	UID      types.UID
	From, To string
}

type clusterImages interface {
	Get(context.Context, string, metav1.GetOptions, ...string) (*unstructured.Unstructured, error)
	Update(context.Context, *unstructured.Unstructured, metav1.UpdateOptions, ...string) (*unstructured.Unstructured, error)
}

// ChangeDeclaredImage compares an enabled declaration against the attempt's
// snapshot, then updates only spec.nodeAgents.image. The Kubernetes
// resourceVersion on the object fences changes between the read and write.
func ChangeDeclaredImage(ctx context.Context, clusters clusterImages, change ImageChange, checkLease func(context.Context) error) error {
	if change.Cluster == "" || change.UID == "" || change.From == "" || change.To == "" || checkLease == nil {
		return fmt.Errorf("incomplete node Agent image change")
	}
	cluster, err := clusters.Get(ctx, change.Cluster, metav1.GetOptions{})
	if err != nil {
		return err
	}
	enabled, _, err := unstructured.NestedBool(cluster.Object, "spec", "nodeAgents", "enabled")
	if err != nil {
		return fmt.Errorf("read node Agent enablement: %w", err)
	}
	image, _, err := unstructured.NestedString(cluster.Object, "spec", "nodeAgents", "image")
	if err != nil {
		return fmt.Errorf("read node Agent image: %w", err)
	}
	if cluster.GetUID() != change.UID || cluster.GetResourceVersion() == "" || cluster.GetDeletionTimestamp() != nil || !enabled || (image != change.From && image != change.To) {
		return fmt.Errorf("cluster %s: %w", change.Cluster, ErrImageDeclarationChanged)
	}
	if err := checkLease(ctx); err != nil {
		return fmt.Errorf("check member lease before changing node Agent image: %w", err)
	}
	if image == change.To {
		return nil
	}
	updated := cluster.DeepCopy()
	if err := unstructured.SetNestedField(updated.Object, change.To, "spec", "nodeAgents", "image"); err != nil {
		return err
	}
	_, err = clusters.Update(ctx, updated, metav1.UpdateOptions{})
	return err
}
