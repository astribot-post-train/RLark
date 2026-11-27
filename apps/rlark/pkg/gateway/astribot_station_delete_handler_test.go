package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A record is deleted when its node was removed, when another Node reports under its name, or when its node does
// not report and RLark has no Node of the name; the record of a node in the cluster, or of one that reports, stays,
// and one that is gone answers as deleted.
func TestDeleteStationDeletesARecordNoNodeStandsOn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	retired := stationHandlerRecord("retired", "agent-a")
	_ = unstructured.SetNestedField(retired.Object, true, "spec", "retired")
	conflict := stationHandlerRecord("conflict", "agent-a")
	_ = unstructured.SetNestedField(conflict.Object, "Conflict", "status", "node", "state")
	node := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "rlinf.io/v1alpha1", "kind": "Node", "metadata": map[string]interface{}{"name": name, "namespace": "agent-a"}}}
	}
	reporting := stationHandlerRecord("reporting", "agent-a")
	_ = unstructured.SetNestedField(reporting.Object, "Bound", "status", "node", "state")
	client := stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a"), retired, conflict, reporting, stationHandlerRecord("bound", "agent-a"), stationHandlerRecord("left", "agent-a"),
		node("retired"), node("conflict"), node("bound"))
	g := &Gateway{astribotState: astribotState{stationClient: client}}
	for name, want := range map[string]int{"bound": http.StatusConflict, "reporting": http.StatusConflict, "gone": http.StatusOK, "retired": http.StatusOK, "conflict": http.StatusOK, "left": http.StatusOK} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/station/stations/"+name+"?cluster_id=cluster-a", nil)
		ctx.Params = gin.Params{{Key: "name", Value: name}}
		g.handleDeleteStation(ctx)
		if recorder.Code != want {
			t.Fatalf("%s = %d, want %d", name, recorder.Code, want)
		}
		_, err := client.Resource(networkEnrollmentGVR).Namespace("agent-a").Get(t.Context(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) != (want == http.StatusOK) {
			t.Fatalf("%s after the request: %v", name, err)
		}
	}
}

func TestForgetConflictDeletesOnlySelectedEnrollment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	old := stationHandlerRecord("box", "agent-a")
	old.SetUID("enrollment-old")
	old.SetResourceVersion("1")
	for _, field := range []struct {
		value any
		path  []string
	}{
		{"old-node", []string{"spec", "nodeUID"}},
		{"new-node", []string{"status", "observation", "nodeUID"}},
		{"Conflict", []string{"status", "node", "state"}},
		{[]any{map[string]any{"type": "IdentityMatched", "status": "False", "reason": "NodeUIDChanged"}}, []string{"status", "node", "conditions"}},
	} {
		if err := unstructured.SetNestedField(old.Object, field.value, field.path...); err != nil {
			t.Fatal(err)
		}
	}
	replacement := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "rlinf.io/v1alpha1", "kind": "Node",
		"metadata": map[string]any{"name": "box", "namespace": "agent-a", "uid": "new-node"}}}
	client := stationHandlerClient(stationHandlerCluster("cluster-a", "agent-a"), old, replacement)
	g := &Gateway{astribotState: astribotState{stationClient: client}}
	call := func(uid string, legacy bool) int {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/station/stations/box/conflict?cluster_id=cluster-a&record_uid="+uid, nil)
		ctx.Params = gin.Params{{Key: "name", Value: "box"}}
		if legacy {
			g.handleDeleteStation(ctx)
		} else {
			g.handleForgetConflictingStation(ctx)
		}
		return recorder.Code
	}
	if got := call("", false); got != http.StatusBadRequest {
		t.Fatalf("missing enrollment UID: %d", got)
	}
	if got := call("", true); got != http.StatusConflict {
		t.Fatalf("generic deletion bypassed the UID requirement: %d", got)
	}
	if got := call("same-name-replacement", false); got != http.StatusConflict {
		t.Fatalf("stale enrollment UID: %d", got)
	}
	recorder := httptest.NewRecorder()
	remove, _ := gin.CreateTestContext(recorder)
	remove.Request = httptest.NewRequest(http.MethodPost, "/api/v1/station/node-operations",
		strings.NewReader(`{"cluster_id":"cluster-a","request_id":"attempt","action":"Remove","node_name":"box"}`))
	remove.Request.Header.Set("Content-Type", "application/json")
	g.handleCreateNodeOperation(remove)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("removal by name let the replacement be selected: %d", recorder.Code)
	}
	if got := call("enrollment-old", false); got != http.StatusOK {
		t.Fatalf("forget old enrollment: %d", got)
	}
	if got := call("enrollment-old", false); got != http.StatusOK {
		t.Fatalf("idempotent retry: %d", got)
	}
	resource := client.Resource(networkEnrollmentGVR).Namespace("agent-a")
	newEnrollment := old.DeepCopy()
	newEnrollment.SetUID("enrollment-new")
	if _, err := resource.Create(t.Context(), newEnrollment, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := call("enrollment-old", false); got != http.StatusConflict {
		t.Fatalf("stale retry after same-name recreation: %d", got)
	}
	if got, err := client.Resource(siteNodeGVR).Namespace("agent-a").Get(t.Context(), "box", metav1.GetOptions{}); err != nil || got.GetUID() != "new-node" {
		t.Fatalf("native/management replacement Node affected: %v %v", got, err)
	}
}
