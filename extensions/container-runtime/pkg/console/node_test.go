package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/golang-jwt/jwt/v5"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func schedulingRequest(t *testing.T, a *API, n *ext.ContainerNode, unschedulable bool, mutate func(*patchNodeSchedulingRequest)) int {
	t.Helper()
	var nn native.Node
	require.NoError(t, a.Client.Get(context.Background(), client.ObjectKey{Namespace: n.Spec.Node.Namespace, Name: n.Spec.Node.Name}, &nn))
	req := patchNodeSchedulingRequest{UID: string(n.UID), ResourceVersion: nn.ResourceVersion, Unschedulable: &unschedulable}
	if mutate != nil {
		mutate(&req)
	}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	return request(t, a, "PATCH", prefix+"clusters/cloud-agents/cloud/nodes/node-1", string(body)).Code
}
func TestNodeSchedulingCordonPreservesWorkAndRecoversLostResponse(t *testing.T) {
	a, _, cn, _ := fixture(t)
	ctx := context.Background()
	allocation := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: companion.Name(cn.Spec.Node.UID), Namespace: cn.Namespace}, Spec: ext.ContainerAllocationSpec{Cluster: cn.Spec.Cluster, Node: cn.Spec.Node, NodeRecordUID: string(cn.UID)}}
	base := a.Client.(client.Client)
	require.NoError(t, base.Create(ctx, allocation))
	a.Client = &uncertainPatch{Management: a.Client}
	require.Equal(t, http.StatusServiceUnavailable, schedulingRequest(t, a, cn, true, nil))
	d := getDetail(t, a)
	require.True(t, d.Nodes[0].Unschedulable)
	require.False(t, d.Nodes[0].Schedulable)
	require.True(t, d.Nodes[0].Ready)
	var lock ext.ContainerAllocation
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(allocation), &lock))
	require.Equal(t, allocation.Spec, lock.Spec)
	require.False(t, lock.Status.CleanupConfirmed)
	// Recover by reading current intent, then repeating the same explicit value.
	require.Equal(t, http.StatusOK, schedulingRequest(t, a, cn, true, nil))
	require.Equal(t, http.StatusConflict, schedulingRequest(t, a, cn, false, nil))
	require.NoError(t, base.Delete(ctx, &lock))
	require.Equal(t, http.StatusOK, schedulingRequest(t, a, cn, false, nil))
	require.False(t, getDetail(t, a).Nodes[0].Unschedulable)
}
func TestNodeSchedulingRejectsStaleAndForeignAuthority(t *testing.T) {
	tests := []struct {
		name          string
		change        func(*testing.T, *API, *ext.ContainerNode)
		requestChange func(*patchNodeSchedulingRequest)
		want          int
	}{
		{name: "stale resourceVersion", requestChange: func(r *patchNodeSchedulingRequest) { r.ResourceVersion = "old" }, want: 409},
		{name: "replaced companion UID", requestChange: func(r *patchNodeSchedulingRequest) { r.UID = "old" }, want: 409},
		{name: "missing boolean", requestChange: func(r *patchNodeSchedulingRequest) { r.Unschedulable = nil }, want: 400},
		{name: "foreign node tenant", change: func(t *testing.T, a *API, n *ext.ContainerNode) {
			n.Labels[tenant.IDLabel] = "other"
			require.NoError(t, a.Client.(client.Client).Update(context.Background(), n))
		}, want: 409},
		{name: "foreign native tenant", change: func(t *testing.T, a *API, n *ext.ContainerNode) {
			var nn native.Node
			require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(n), &nn))
			nn.Labels[tenant.IDLabel] = "other"
			require.NoError(t, a.Client.(client.Client).Update(context.Background(), &nn))
		}, want: 409},
		{name: "runtime mismatch", change: func(t *testing.T, a *API, n *ext.ContainerNode) {
			var nn native.Node
			require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(n), &nn))
			nn.Spec.AgentType = native.AgentTypeKubernetes
			require.NoError(t, a.Client.(client.Client).Update(context.Background(), &nn))
		}, want: 409},
		{name: "replaced native UID", change: func(t *testing.T, a *API, n *ext.ContainerNode) {
			var nn native.Node
			require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(n), &nn))
			require.NoError(t, a.Client.(client.Client).Delete(context.Background(), &nn))
			nn.UID = "new"
			nn.ResourceVersion = ""
			require.NoError(t, a.Client.(client.Client).Create(context.Background(), &nn))
		}, want: 409},
		{name: "cross namespace reference", change: func(t *testing.T, a *API, n *ext.ContainerNode) {
			n.Spec.Node.Namespace = "foreign"
			require.NoError(t, a.Client.(client.Client).Update(context.Background(), n))
		}, want: 409},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, n, _ := fixture(t)
			if tt.change != nil {
				tt.change(t, a, n)
			}
			// Use known native resourceVersion to avoid needing to follow a forged reference.
			require.Equal(t, tt.want, requestNodeBody(t, a, n, true, tt.requestChange))
			var nn native.Node
			require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(n), &nn))
			require.False(t, nn.Spec.Unschedulable)
		})
	}
}
func requestNodeBody(t *testing.T, a *API, n *ext.ContainerNode, value bool, change func(*patchNodeSchedulingRequest)) int {
	t.Helper()
	var nn native.Node
	require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(n), &nn))
	req := patchNodeSchedulingRequest{UID: string(n.UID), ResourceVersion: nn.ResourceVersion, Unschedulable: &value}
	if change != nil {
		change(&req)
	}
	data, err := json.Marshal(req)
	require.NoError(t, err)
	return request(t, a, "PATCH", prefix+"clusters/cloud-agents/cloud/nodes/node-1", string(data)).Code
}
func TestUncordonFailsClosedForUnknownDeletingAndUpgradeLocks(t *testing.T) {
	for _, mode := range []string{"native UID", "companion UID", "lock name", "deleting", "upgrade", "legacy", "outage"} {
		t.Run(mode, func(t *testing.T) {
			a, cc, n, _ := fixture(t)
			ctx := context.Background()
			c := a.Client.(client.Client)
			require.Equal(t, 200, schedulingRequest(t, a, n, true, nil))
			lock := &ext.ContainerAllocation{ObjectMeta: metav1.ObjectMeta{Name: "unknown", Namespace: n.Namespace}, Spec: ext.ContainerAllocationSpec{TenantID: "foreign"}}
			switch mode {
			case "native UID":
				lock.Spec.Node.UID = n.Spec.Node.UID
			case "companion UID":
				lock.Spec.NodeRecordUID = string(n.UID)
			case "lock name":
				lock.Name = companion.Name(n.Spec.Node.UID)
			case "deleting":
				lock.Spec.Node = n.Spec.Node
				lock.Finalizers = []string{companion.Finalizer}
			case "upgrade":
				lock.Spec.Node = n.Spec.Node
				lock.Spec.Cluster = cc.Spec.Cluster
				lock.Spec.Upgrade = &ext.UpgradeAllocation{}
			case "legacy":
				var nn native.Node
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(n), &nn))
				nn.Status.Reservation = &native.RawNodeReservation{TaskUID: "held"}
				require.NoError(t, c.Update(ctx, &nn))
			case "outage":
				a.Client = listFailure{a.Client}
			}
			if mode != "legacy" && mode != "outage" {
				require.NoError(t, c.Create(ctx, lock))
				if mode == "deleting" {
					require.NoError(t, c.Delete(ctx, lock))
				}
			}
			want := 409
			if mode == "outage" {
				want = 503
			}
			require.Equal(t, want, schedulingRequest(t, a, n, false, nil))
			var nn native.Node
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(n), &nn))
			require.True(t, nn.Spec.Unschedulable)
		})
	}
}
func TestSchedulingProtocolRejectsMalformedAndNonAdminWrites(t *testing.T) {
	a, _, _, _ := fixture(t)
	for _, body := range []string{`null`, `{}`, `{"uid":"x","resourceVersion":"1","unschedulable":true,"agentType":"Kubernetes"}`, `{} {}`} {
		w := request(t, a, "PATCH", prefix+"clusters/cloud-agents/cloud/nodes/node-1", body)
		require.Equal(t, 400, w.Code)
	}
	for _, admin := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodPatch, prefix+"clusters/cloud-agents/cloud/nodes/node-1", strings.NewReader(`{}`))
		want := http.StatusUnauthorized
		if admin {
			r.Header.Set("Authorization", "Bearer "+token(t, claims{Role: "user", RegisteredClaims: jwt.RegisteredClaims{Issuer: "rlark-gateway", Subject: "user", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}))
			want = http.StatusForbidden
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		require.Equal(t, want, w.Code)
	}
	// A patch conflict retains the old desired value.
	a.Client = rejectPatch{a.Client}
	require.Equal(t, 503, requestNodeBody(t, a, &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "cloud-agents", UID: types.UID("companion-node")}}, true, nil))
}

type rejectPatch struct{ Management }

func (c rejectPatch) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return errors.New("transport unavailable")
}

type racingNodePatch struct {
	Management
	base    client.Client
	replace bool
}

func (c racingNodePatch) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	var node native.Node
	if err := c.base.Get(ctx, client.ObjectKeyFromObject(obj), &node); err != nil {
		return err
	}
	if c.replace {
		if err := c.base.Delete(ctx, &node); err != nil {
			return err
		}
		node.UID = "replacement"
		node.ResourceVersion = ""
		if err := c.base.Create(ctx, &node); err != nil {
			return err
		}
	} else {
		node.Annotations = map[string]string{"concurrent": "writer"}
		if err := c.base.Update(ctx, &node); err != nil {
			return err
		}
	}
	return c.Management.Patch(ctx, obj, p, opts...)
}
func TestSchedulingPatchAtomicallyTestsNativeUIDAndVersion(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "version race", true: "replacement race"}[replace], func(t *testing.T) {
			a, _, cn, _ := fixture(t)
			base := a.Client.(client.Client)
			a.Client = racingNodePatch{Management: a.Client, base: base, replace: replace}
			status := schedulingRequest(t, a, cn, true, nil)
			require.NotEqual(t, http.StatusOK, status)
			var nn native.Node
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(cn), &nn))
			require.False(t, nn.Spec.Unschedulable)
		})
	}
}
