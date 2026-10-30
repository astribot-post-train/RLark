package controller

import (
	"context"
	"fmt"

	containerapi "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// JobOwner is the generic control-plane seam: an extension may own a Job only
// when a typed binding proves the exact native object UID and tenant.
type JobOwner struct{ Client client.Reader }

// Owns checks current API state. Binding names and labels are lookup hints, not
// ownership evidence; duplicate bindings and UID reuse fail closed.
func (r JobOwner) Owns(ctx context.Context, job *native.Job) (bool, error) {
	if r.Client == nil || job == nil || job.UID == "" {
		return false, fmt.Errorf("container Job ownership requires a client and persisted native Job")
	}
	var bindings containerapi.ContainerJobList
	if err := r.Client.List(ctx, &bindings); err != nil {
		return false, fmt.Errorf("list container Job bindings: %w", err)
	}
	var match *containerapi.ContainerJob
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if binding.Spec.Job.Name != job.Name || binding.Spec.Job.Namespace != job.Namespace {
			continue
		}
		if match != nil {
			return false, fmt.Errorf("multiple container bindings refer to Job %s/%s", job.Namespace, job.Name)
		}
		match = binding
	}
	if match == nil {
		return false, nil
	}
	if job.Annotations[nativejob.ControllerOwnerAnnotation] != containerapi.Group || tenant.IDOf(match) != tenant.IDOf(job) {
		return false, fmt.Errorf("container Job declared owner or tenant metadata conflicts")
	}
	if err := ownership.JobBindingMatches(match, job, tenant.IDOf(job)); err != nil {
		return false, fmt.Errorf("invalid container Job binding %s/%s: %w", match.Namespace, match.Name, err)
	}
	if match.Spec.Runtime != "Raw" && match.Spec.Runtime != "Docker" {
		return false, fmt.Errorf("unsupported container Job runtime")
	}
	return true, nil
}

func getNative(ctx context.Context, c client.Client, ref containerapi.NativeObjectReference, obj client.Object) error {
	if ref.Name == "" || ref.UID == "" {
		return invalidBinding("native object name and UID are required")
	}
	err := c.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("referenced native object %s/%s no longer exists: %w", ref.Namespace, ref.Name, err)
	}
	if err != nil {
		return err
	}
	if err := ownership.MatchesNative(ref, obj); err != nil {
		return invalidBinding("native object UID does not match")
	}
	return nil
}
