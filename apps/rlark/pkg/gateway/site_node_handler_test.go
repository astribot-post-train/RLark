package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func siteNodeRequest(t *testing.T, gateway *Gateway, node, query string) (int, siteNode) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/api/v1/site-nodes/"+node+query, nil)
	ctx.Params = gin.Params{{Key: "name", Value: node}}
	gateway.handleGetSiteNode(ctx)
	var result siteNode
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	return recorder.Code, result
}

func TestGetSiteNodeJoinsIdentityNetworkAndRobot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	station := stationHandlerStation("stn-7f3a", "agent-a")
	_ = unstructured.SetNestedField(station.Object, "s1u-01-x86", "spec", "binding", "nodeName")
	_ = unstructured.SetNestedField(station.Object, int64(2), "spec", "binding", "version")
	_ = unstructured.SetNestedField(station.Object, int64(3), "spec", "sequence")
	_ = unstructured.SetNestedMap(station.Object, map[string]interface{}{"phase": "Bound", "lastSeenTime": "2026-11-12T08:00:00Z"}, "status")
	enrollment := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NetworkEnrollment",
		"metadata": map[string]interface{}{"name": "s1u-01-x86", "namespace": "agent-a"},
		"spec":     map[string]interface{}{"bindingVersion": int64(2)},
		"status":   map[string]interface{}{"phase": "Enrolled", "site": "lab", "role": "Leaf", "address": "10.254.240.3", "tunnelAddress": "10.254.240.3", "config": "{}"},
	}}
	node := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rlinf.io/v1alpha1", "kind": "Node",
		"metadata": map[string]interface{}{"name": "s1u-01-x86", "namespace": "agent-a", "labels": map[string]interface{}{"rlark.io/robot-id": "s1u-01"}},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		stationClusterGVR: "ClusterList", stationRecordGVR: "StationList", networkEnrollmentGVR: "NetworkEnrollmentList", siteNodeGVR: "NodeList",
	}, stationHandlerCluster("cluster-a", "agent-a"), station, enrollment, node)
	gateway := &Gateway{stationClient: client}

	code, got := siteNodeRequest(t, gateway, "s1u-01-x86", "?cluster_id=cluster-a")
	want := siteNode{ClusterID: "cluster-a", NodeName: "s1u-01-x86", SiteNodeID: "stn-7f3a", Sequence: 3, BindingVersion: 2, Phase: "Bound", Online: true,
		LastSeenTime: "2026-11-12T08:00:00Z", RobotID: "s1u-01"}
	if code != 200 || got.Network == nil || *got.Network != (siteNodeNetwork{State: "Enrolled", Site: "lab", Role: "Leaf", Address: "10.254.240.3", TunnelAddress: "10.254.240.3"}) {
		t.Fatalf("status %d, network %+v", code, got.Network)
	}
	got.Network = nil
	if got != want {
		t.Fatalf("site node %+v, want %+v", got, want)
	}

	// After a rebind the old enrollment's address is not served.
	_ = unstructured.SetNestedField(station.Object, int64(3), "spec", "binding", "version")
	if _, err := client.Resource(stationRecordGVR).Namespace("agent-a").Update(t.Context(), station, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if code, got = siteNodeRequest(t, gateway, "s1u-01-x86", "?cluster_id=cluster-a"); code != 200 || got.BindingVersion != 3 || got.Network != nil {
		t.Fatalf("after rebind: %d %+v", code, got)
	}
	if err := client.Resource(networkEnrollmentGVR).Namespace("agent-a").Delete(t.Context(), "s1u-01-x86", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if code, got = siteNodeRequest(t, gateway, "s1u-01-x86", "?cluster_id=cluster-a"); code != 200 || got.Network != nil || got.RobotID != "s1u-01" {
		t.Fatalf("identity without network enrollment: %d %+v", code, got)
	}
	if code, _ = siteNodeRequest(t, gateway, "unknown", "?cluster_id=cluster-a"); code != 404 {
		t.Fatalf("unknown node returned %d", code)
	}
	if code, _ = siteNodeRequest(t, gateway, "s1u-01-x86", ""); code != 400 {
		t.Fatalf("missing cluster returned %d", code)
	}
}
