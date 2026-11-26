package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	clientset "github.com/rlinf/rlark/api/kubeclients/clientset/versioned/fake"
	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/workflow"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestRobotModelListCountsReferencesAndDeleteBlocksThenAllows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	model := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "station.rlark.io/v1alpha1", "kind": "RobotModel", "metadata": map[string]interface{}{"name": "s1u"}, "spec": map[string]interface{}{"displayName": "S1U", "vendor": "RLark"}}}
	class := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeClass", "metadata": map[string]interface{}{"name": "s1u"}, "spec": map[string]interface{}{"model": "s1u"}}}
	node := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "rlinf.io/v1alpha1", "kind": "Node", "metadata": map[string]interface{}{"name": "robot-01", "namespace": "agent-a", "labels": map[string]interface{}{"rlark.io/robot-model": "s1u"}}}}
	admission := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission", "metadata": map[string]interface{}{"name": "robot-01", "namespace": "agent-a"}, "spec": map[string]interface{}{"machine": map[string]interface{}{"category": "Robot", "model": "s1u"}}}}
	listKinds := map[schema.GroupVersionResource]string{stations.RobotModelsResource: "RobotModelList", nodeClassGVR: "NodeClassList", siteNodeGVR: "NodeList", networkEnrollmentGVR: "NetworkEnrollmentList", machineGVR: "MachineList", nodeAdmissionGVR: "NodeAdmissionList"}
	station := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, model, class, node, admission)
	joins := clientset.NewSimpleClientset(&rlarkv1alpha1.NodeJoin{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.robot-01", Annotations: map[string]string{workflow.NodeJoinModelAnnotation: "s1u"}}})
	gateway := &Gateway{kubeClient: joins, astribotState: astribotState{stationClient: station}}

	router := gin.New()
	gateway.registerRobotModelRoutes(router.Group("/api/v1"))
	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/v1/station/robot-models", nil))
	var listed struct {
		Items []struct {
			References struct{ NodeClasses, Nodes, NodeAdmissions, NodeJoins int }
		}
	}

	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listResponse.Code != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].References.NodeClasses != 1 || listed.Items[0].References.Nodes != 1 || listed.Items[0].References.NodeAdmissions != 1 || listed.Items[0].References.NodeJoins != 1 {
		t.Fatalf("list result %d: %+v", listResponse.Code, listed)
	}

	delete := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/v1/station/robot-models/s1u", nil))
		return response
	}
	if response := delete(); response.Code != http.StatusConflict {
		t.Fatalf("referenced model delete status %d: %s", response.Code, response.Body.String())
	}
	if err := station.Resource(siteNodeGVR).Namespace("agent-a").Delete(t.Context(), "robot-01", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := station.Resource(nodeAdmissionGVR).Namespace("agent-a").Delete(t.Context(), "robot-01", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := joins.RlinfV1alpha1().NodeJoins().Delete(t.Context(), "cluster-a.robot-01", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := station.Resource(nodeClassGVR).Delete(t.Context(), "s1u", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if response := delete(); response.Code != http.StatusNoContent {
		t.Fatalf("unreferenced model delete status %d: %s", response.Code, response.Body.String())
	}
}
