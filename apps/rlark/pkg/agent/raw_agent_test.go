package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRawConfigRejectsImplicitScope(t *testing.T) {
	c := DefaultConfig()
	c.AgentType = "Raw"
	require.Error(t, c.validateRaw())
	c.RawSupervisorSocket = "/var/lib/rlark/supervisor.sock"
	c.RawTrainingIP = "10.0.0.1"
	c.RawNetworkScope = "ksyun-vpc"
	c.RawEnvironment = "prepared"
	c.RawAuthorizedGPUUUIDs = []string{"GPU-authorized"}
	require.NoError(t, c.validateRaw())
	for _, mode := range []string{"node", "both"} {
		copy := c
		copy.Mode = mode
		require.Error(t, copy.validateRaw())
	}
	for _, ip := range []string{"", "127.0.0.1", "8.8.8.8", "::1"} {
		copy := c
		copy.RawTrainingIP = ip
		require.Error(t, copy.validateRaw())
	}
	c.RawAuthorizedGPUUUIDs = append(c.RawAuthorizedGPUUUIDs, "GPU-authorized")
	require.Error(t, c.validateRaw())
}

func TestRawPollingRepairsCapabilitiesWithoutTouchingReservation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	identity := rawnode.Identity{Namespace: "agents", Node: "gpu", NodeUID: "uid", Incarnation: "container"}
	capability := &api.RawNodeStatus{Incarnation: "container", TrainingIP: "10.0.0.1", NetworkScope: "vpc", Environment: "prepared", AuthorizedGPUUUIDs: []string{"GPU-authorized"}}
	data, err := json.Marshal(capability)
	require.NoError(t, err)
	node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: identity.Node, Namespace: identity.Namespace, UID: "uid", Annotations: map[string]string{rawnode.CapabilityAnnotation: string(data)}}, Status: api.NodeStatus{Raw: capability, Reservation: &api.RawNodeReservation{ExecutionID: "reserved"}}}
	patches := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Node{}).WithObjects(node).WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		patches++
		return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
	}}).Build()
	h := &host.Host{}
	require.NoError(t, reconcileRawScan(ctx, c, h, identity, capability))
	require.Equal(t, 1, patches)
	var updated api.Node
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(node), &updated))
	require.Equal(t, node.Status.Reservation, updated.Status.Reservation)
	require.Len(t, updated.Status.Capacity, 1) // No guessed host CPU/memory capacity.
	require.NoError(t, reconcileRawScan(ctx, c, h, identity, capability))
	require.Equal(t, 1, patches)
	outage := interceptor.NewClient(c, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("API down")
	}})
	require.Error(t, reconcileRawScan(ctx, outage, h, identity, capability))
	require.NoError(t, reconcileRawScan(ctx, c, h, identity, capability))
	wrong := *capability
	wrong.AuthorizedGPUUUIDs = []string{"GPU-unapproved"}
	require.Error(t, reconcileRawScan(ctx, c, h, identity, &wrong))
	require.Equal(t, 1, patches)
}
