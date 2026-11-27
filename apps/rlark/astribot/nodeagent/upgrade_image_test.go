package nodeagent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestChangeDeclaredImageWritesOnlyTheImage(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "rlinf.io", Version: "v1alpha1", Resource: "clusters"}
	object := func(uid types.UID, image string, enabled bool) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "rlinf.io/v1alpha1", "kind": "Cluster",
			"metadata": map[string]any{"name": "lab", "uid": string(uid), "resourceVersion": "15", "labels": map[string]any{"site": "lab"}},
			"spec": map[string]any{"nodeAgents": map[string]any{
				"image": image, "enabled": enabled, "nodeSelector": map[string]any{"robot": "true"},
				"rollout": map[string]any{"canary": int64(1)},
			}, "agentNamespace": "lab"},
			"status": map[string]any{"phase": "Online"},
		}}
	}
	change := ImageChange{Cluster: "lab", UID: "cluster-uid", From: "previous", To: "target"}
	lostLease := errors.New("lease lost")
	for _, tt := range []struct {
		name, image string
		uid         types.UID
		enabled     bool
		leaseError  error
		want        error
		write       bool
	}{
		{name: "forward", uid: change.UID, image: change.From, enabled: true, write: true},
		{name: "retry after write", uid: change.UID, image: change.To, enabled: true},
		{name: "replaced Cluster", uid: "other-uid", image: change.From, enabled: true, want: ErrImageDeclarationChanged},
		{name: "concurrent image edit", uid: change.UID, image: "operator-image", enabled: true, want: ErrImageDeclarationChanged},
		{name: "disabled declaration", uid: change.UID, image: change.From, want: ErrImageDeclarationChanged},
		{name: "lost lease", uid: change.UID, image: change.From, enabled: true, leaseError: lostLease, want: lostLease},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := object(tt.uid, tt.image, tt.enabled)
			kube := fake.NewSimpleDynamicClient(runtime.NewScheme(), before)
			clusters := kube.Resource(gvr)
			err := ChangeDeclaredImage(t.Context(), clusters, change, func(ctx context.Context) error { return tt.leaseError })
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
			} else {
				require.NoError(t, err)
			}
			actions := kube.Actions()
			if tt.write {
				require.Len(t, actions, 2)
				expected := before.DeepCopy()
				require.NoError(t, unstructured.SetNestedField(expected.Object, change.To, "spec", "nodeAgents", "image"))
				require.Equal(t, expected, actions[1].(ktesting.UpdateAction).GetObject())
			} else {
				require.Len(t, actions, 1)
			}
		})
	}

	t.Run("resourceVersion conflict is preserved", func(t *testing.T) {
		kube := fake.NewSimpleDynamicClient(runtime.NewScheme(), object(change.UID, change.From, true))
		kube.PrependReactor("update", "clusters", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewConflict(gvr.GroupResource(), change.Cluster, errors.New("changed during write"))
		})
		err := ChangeDeclaredImage(t.Context(), kube.Resource(gvr), change, func(context.Context) error { return nil })
		require.True(t, apierrors.IsConflict(err), "%v", err)
	})
}
