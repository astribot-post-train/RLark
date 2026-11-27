package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	clientset "github.com/rlinf/rlark/api/kubeclients/clientset/versioned/fake"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type networkClientStub struct {
	calls int
	err   error
}

func (s *networkClientStub) Read(context.Context, string, string) (stationidentity.NodeNetworkSummary, error) {
	s.calls++
	return stationidentity.NodeNetworkSummary{State: "current", Interface: "st-wg0", Addresses: []string{"10.254.240.1/32"}}, s.err
}

func TestNodeNetworkAuthorizationAndFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                                  string
		delisted, admin, missingNode, failure bool
		status, calls                         int
	}{
		{"visible", false, false, false, false, 200, 1},
		{"delisted user", true, false, false, false, 404, 0},
		{"delisted admin", true, true, false, false, 200, 1},
		{"node absent", false, false, true, false, 404, 0},
		{"station unavailable", false, false, false, true, 503, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: api.ClusterSpec{AgentNamespace: "agent", Delisted: tc.delisted}}
			node := &api.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "agent"}}
			client := clientset.NewSimpleClientset(cluster)
			if !tc.missingNode {
				_, err := client.RlinfV1alpha1().Nodes("agent").Create(context.Background(), node, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			objects := stationHandlerClient(stationHandlerCluster("cluster", "agent"))
			stub := &networkClientStub{}
			if tc.failure {
				stub.err = errors.New("down")
			}
			g := &Gateway{kubeClient: client, astribotState: astribotState{stationClient: objects, networkSummary: stub}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("GET", "/api/v1/node-networks/node?cluster_id=cluster", nil)
			c.Params = gin.Params{{Key: "name", Value: "node"}}
			if tc.admin {
				c.Set(authRoleKey, "admin")
			}
			g.handleNodeNetwork(c)
			if recorder.Code != tc.status || stub.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", recorder.Code, stub.calls, recorder.Body.String())
			}
		})
	}
}
