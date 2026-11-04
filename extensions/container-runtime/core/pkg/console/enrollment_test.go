package console

import (
	"context"
	"net/http"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/provision"
	tenant "github.com/rlinf/rlark/api/metadata"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEnrollmentCancellationIsUIDBoundIdempotentAndRetained(t *testing.T) {
	for _, tc := range []struct {
		name, uid, clusterUID string
		foreign               bool
		want                  int
	}{
		{"valid", "operation-instance", "companion-cluster", false, 202},
		{"replaced operation", "old-instance", "companion-cluster", false, 409},
		{"replaced cluster", "operation-instance", "old-cluster", false, 409},
		{"cross tenant", "operation-instance", "companion-cluster", true, 409},
		{"missing UID", "", "companion-cluster", false, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, cc, node, _ := fixture(t)
			management := a.Client.(client.Client)
			a.Enrollments = provision.Service{Client: management}
			op := &ext.NodeEnrollment{ObjectMeta: metadata("enroll-test", cc.Namespace, "operation-instance"), Spec: ext.NodeEnrollmentSpec{ClusterRecord: ext.NativeObjectReference{Name: cc.Name, Namespace: cc.Namespace, UID: string(cc.UID)}, TenantID: tenant.IDOf(cc), OperationID: "operation-id"}}
			op.ResourceVersion = ""
			if tc.foreign {
				op.Labels[tenant.IDLabel] = "foreign"
			}
			require.NoError(t, management.Create(context.Background(), op))
			body := `{"uid":"` + tc.uid + `","clusterUID":"` + tc.clusterUID + `"}`
			path := prefix + "clusters/" + cc.Namespace + "/" + cc.Name + "/enrollments/" + op.Name + "/cancel"
			for i := 0; i < 2; i++ {
				response := request(t, a, http.MethodPost, path, body)
				require.Equal(t, tc.want, response.Code, response.Body.String())
			}
			require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(op), op))
			require.Equal(t, tc.want == 202, op.Spec.CancelRequested)
			require.NoError(t, a.Client.Get(context.Background(), client.ObjectKeyFromObject(node), node), "cancellation deleted a joined node")
		})
	}
}
