package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// handlerMachine is a Machine RLark has read: its entry device let it in and the computer behind it answered.
func handlerMachine(name, phase, problem, serial string) *unstructured.Unstructured {
	status := map[string]interface{}{"phase": phase, "serial": serial, "inspectedAt": "2027-01-15T07:58:00Z", "nodeHostKeyFingerprint": "SHA256:x86",
		"entry": map[string]interface{}{"address": "192.168.31.56", "port": int64(22), "hostKeyFingerprint": "SHA256:entry", "hostKeyAlgorithm": "ssh-ed25519", "mac": "48:b0:2d:5c:9a:11", "lastSeen": "2027-01-15T08:00:" + name[len(name)-2:] + "Z"}}
	if problem != "" {
		status["problem"] = problem
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "Machine", "metadata": map[string]interface{}{"name": name, "namespace": "station-system"},
		"spec": map[string]interface{}{"site": "lab", "nodeClass": "s1u", "source": "Discovery"}, "status": status,
	}}
}

func TestMachinesAreListedAndAdmittedEachForItself(t *testing.T) {
	gin.SetMode(gin.TestMode)
	class := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeClass", "metadata": map[string]interface{}{"name": "s1u"},
		"spec": map[string]interface{}{"kind": "Robot", "model": "s1u", "discovery": map[string]interface{}{"hostKeyFingerprints": []interface{}{"SHA256:entry"}}, "labels": map[string]interface{}{"rlark.io/robot-model": "s1u", "rlark.io/robot-body": "s1u"}},
	}}
	earlier := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission", "metadata": map[string]interface{}{"name": "s1u-01-x86", "namespace": "agent-a"},
		"spec": map[string]interface{}{"clusterID": "cluster-a"},
	}}
	// The join of lab-03, stopped after its first stage.
	blocked := handlerMachine("lab-03", "Admitted", "JoinBlocked", "1794925030999")
	blocked.SetGeneration(2)
	_ = unstructured.SetNestedField(blocked.Object, "join-s1u-02-x86-2", "status", "operation")
	_ = unstructured.SetNestedField(blocked.Object, map[string]interface{}{"clusterID": "cluster-a", "requestedBy": "alice", "requestedAt": "2027-01-15T07:59:00Z"}, "spec", "admit")
	join := func(name, generation, phase, stage string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeOperation",
			"metadata": map[string]interface{}{"name": name, "namespace": "agent-a", "labels": map[string]interface{}{machineOperationLabel: "lab-03"},
				"annotations": map[string]interface{}{machineRequestAnnotation: generation}},
			"status": map[string]interface{}{"phase": phase, "stage": stage},
		}}
	}
	// An earlier request's join, which made a node removed since, is not this request's.
	blockedJoin, spentJoin := join("join-s1u-02-x86-2", "2", "Blocked", "Preflight"), join("join-s1u-02-x86-1", "1", "Succeeded", "RotateEntryHostKey")
	// lab-05 was seen again since it was read last: it is read anew before it can be admitted.
	unread := handlerMachine("lab-05", "Discovered", "", "1794925031001")
	unstructured.RemoveNestedField(unread.Object, "status", "inspectedAt")
	const scanID = "d3feca83-fccd-435d-b270-82f6072f27c7"
	_ = unstructured.SetNestedField(unread.Object, scanID, "status", "entry", "discoveryScanID")
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		stationClusterGVR: "ClusterList", nodeAdmissionGVR: "NodeAdmissionList", machineGVR: "MachineList", nodeClassGVR: "NodeClassList", nodeOperationGVR: "NodeOperationList",
	}, stationHandlerCluster("cluster-a", "agent-a"), class, earlier, blockedJoin, spentJoin,
		handlerMachine("lab-01", "Discovered", "", "1794925030610"), handlerMachine("lab-02", "Discovered", "CredentialRejected", "1794925029733"),
		blocked, handlerMachine("lab-04", "Joined", "", "1794925031000"), unread)
	g := &Gateway{astribotState: astribotState{stationClient: client}}

	status, body := nodeAdmissionCall(t, g.handleListMachines, "GET", "/api/v1/station/machines?phase=Discovered", "", "")
	var listed struct {
		Items   []stations.Machine               `json:"items"`
		Classes map[string]stations.MachineClass `json:"classes"`
	}
	_ = json.Unmarshal(body, &listed)
	if status != 200 || len(listed.Items) != 3 || listed.Items[0].Name != "lab-05" || listed.Items[0].DiscoveryScanID != scanID || listed.Items[0].Admissible || listed.Items[1].Name != "lab-02" || listed.Items[1].Admissible ||
		!listed.Items[2].Admissible || listed.Items[2].Serial != "1794925030610" || listed.Items[2].NodeHostKeyFingerprint != "SHA256:x86" ||
		listed.Classes["s1u"].Labels["rlark.io/robot-body"] != "s1u" {
		t.Fatalf("list %d: %s", status, body)
	}
	// An admitted stations.Machine is listed with where its join stands.
	status, body = nodeAdmissionCall(t, g.handleListMachines, "GET", "/api/v1/station/machines?phase=Admitted", "", "")
	_ = json.Unmarshal(body, &listed)
	if status != 200 || len(listed.Items) != 1 || listed.Items[0].AdmitClusterID != "cluster-a" || listed.Items[0].Join == nil ||
		*listed.Items[0].Join != (stations.MachineJoin{Phase: "Blocked", Stage: "Preflight"}) {
		t.Fatalf("an admitted machine's join %d: %s", status, body)
	}

	status, body = nodeAdmissionCall(t, g.handleAdmitMachines, "POST", "/api/v1/station/machines/admit", "",
		`{"cluster_id":"cluster-a","machines":["lab-01","lab-02","lab-03","lab-04","lab-99"],"labels":{"rlark.io/robot-body":"s1u_observer"}}`)
	var answer struct {
		Machines []stations.MachineAdmissionResult `json:"machines"`
	}
	_ = json.Unmarshal(body, &answer)
	admitted := ""
	for _, one := range answer.Machines {
		if one.Admitted {
			admitted += one.Name + " "
		}
	}
	if status != 200 || len(answer.Machines) != 5 || admitted != "lab-01 lab-03 " || !strings.Contains(answer.Machines[4].Error, "no such machine") {
		t.Fatalf("admit %d: %s", status, body)
	}
	stored, _ := client.Resource(machineGVR).Namespace("station-system").Get(context.Background(), "lab-01", metav1.GetOptions{})
	admit, _, _ := unstructured.NestedMap(stored.Object, "spec", "admit")
	labels, _, _ := unstructured.NestedStringMap(stored.Object, "spec", "admit", "labels")
	if admit["clusterID"] != "cluster-a" || admit["requestedBy"] != "alice" || admit["requestedAt"] == nil || labels["rlark.io/robot-body"] != "s1u_observer" {
		t.Fatalf("what was written on the Machine: %v", admit)
	}
	refused, _ := client.Resource(machineGVR).Namespace("station-system").Get(context.Background(), "lab-02", metav1.GetOptions{})
	if _, written, _ := unstructured.NestedMap(refused.Object, "spec", "admit"); written {
		t.Fatal("a machine that refuses its class's account was admitted")
	}

	// Until station-controller has acted on it, lab-01 is being joined: it is listed so and not admitted twice.
	status, body = nodeAdmissionCall(t, g.handleListMachines, "GET", "/api/v1/station/machines?phase=Discovered", "", "")
	_ = json.Unmarshal(body, &listed)
	if status != 200 || listed.Items[2].Name != "lab-01" || !listed.Items[2].Joining || listed.Items[2].Admissible {
		t.Fatalf("a machine whose admission is being acted on %d: %s", status, body)
	}
	status, body = nodeAdmissionCall(t, g.handleAdmitMachines, "POST", "/api/v1/station/machines/admit", "", `{"cluster_id":"cluster-a","machines":["lab-01"]}`)
	_ = json.Unmarshal(body, &answer)
	if status != 200 || answer.Machines[0].Admitted || !strings.Contains(answer.Machines[0].Error, "being joined to cluster cluster-a already, as alice asked at") {
		t.Fatalf("the same machine admitted twice: %d %s", status, body)
	}

	// A label the class does not declare is nobody's to set here.
	status, body = nodeAdmissionCall(t, g.handleAdmitMachines, "POST", "/api/v1/station/machines/admit", "",
		`{"cluster_id":"cluster-a","machines":["lab-03"],"labels":{"rlark.io/tenant":"other"}}`)
	_ = json.Unmarshal(body, &answer)
	if status != 200 || answer.Machines[0].Admitted || !strings.Contains(answer.Machines[0].Error, "rlark.io/tenant") {
		t.Fatalf("a label of no class: %d %s", status, body)
	}
	if status, _ := nodeAdmissionCall(t, g.handleAdmitMachines, "POST", "/api/v1/station/machines/admit", "", `{"cluster_id":"cluster-a","machines":[]}`); status != 400 {
		t.Fatalf("no machines: %d", status)
	}
}

func TestMachineRejoinIsIdempotentAndPreservesExistingNodeAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cluster := stationHandlerCluster("cluster-a", "agent-a")
	machine := handlerMachine("lab-88", "Joined", "", "1794925030888")
	_ = unstructured.SetNestedField(machine.Object, "cluster-a", "status", "clusterID")
	_ = unstructured.SetNestedField(machine.Object, "s1u-07-x86", "status", "nodeName")
	_ = unstructured.SetNestedField(machine.Object, "s1u", "spec", "admit", "model")
	class := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeClass", "metadata": map[string]any{"name": "s1u"}, "spec": map[string]any{
		"kind": "Robot", "model": "s1u", "access": map[string]any{"entry": map[string]any{"port": int64(22), "user": "orin"}, "node": map[string]any{"address": "192.168.0.10", "port": int64(22), "user": "robot"}},
	}}}
	admission := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission", "metadata": map[string]any{"name": "s1u-07-x86", "namespace": "agent-a"}, "spec": map[string]any{"clusterID": "cluster-a", "serial": "1794925030888", "nodeClass": "s1u", "machine": map[string]any{"category": "Robot", "model": "legacy-s1u"}}, "status": map[string]any{"phase": "Joined"}}}
	model := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "RobotModel", "metadata": map[string]any{"name": "s1u"}, "spec": map[string]any{"displayName": "S1U"}}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		stationClusterGVR: "ClusterList", machineGVR: "MachineList", nodeAdmissionGVR: "NodeAdmissionList", nodeClassGVR: "NodeClassList", robotModelGVR: "RobotModelList", nodeOperationGVR: "NodeOperationList",
	}, cluster, machine, admission, class, model)
	g := &Gateway{astribotState: astribotState{stationClient: client}}
	body := `{"cluster_id":"cluster-a","request_id":"retry-001","robot_model":"s1u"}`
	status, modelMismatch := nodeAdmissionCall(t, g.handleRejoinMachine, "POST", "/api/v1/station/machines/lab-88/rejoin", "lab-88", `{"cluster_id":"cluster-a","request_id":"retry-model","robot_model":"other"}`)
	if status != http.StatusConflict || !strings.Contains(string(modelMismatch), "ROBOT_MODEL_MISMATCH") {
		t.Fatalf("model mismatch status=%d response=%s", status, modelMismatch)
	}
	status, response := nodeAdmissionCall(t, g.handleRejoinMachine, "POST", "/api/v1/station/machines/lab-88/rejoin", "lab-88", body)
	if status != http.StatusAccepted || !strings.Contains(string(response), `"data"`) {
		t.Fatalf("rejoin status=%d response=%s", status, response)
	}
	status, again := nodeAdmissionCall(t, g.handleRejoinMachine, "POST", "/api/v1/station/machines/lab-88/rejoin", "lab-88", body)
	if status != http.StatusAccepted || !strings.Contains(string(again), rejoinOperationName("s1u-07-x86", "retry-001")) {
		t.Fatalf("replay status=%d response=%s", status, again)
	}
	stored, err := client.Resource(nodeAdmissionGVR).Namespace("agent-a").Get(context.Background(), admission.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	serial, _, _ := unstructured.NestedString(stored.Object, "spec", "serial")
	if serial != "1794925030888" {
		t.Fatalf("rejoin changed NodeAdmission serial: %s", serial)
	}
	storedMachine, _ := client.Resource(machineGVR).Namespace(machineNamespace).Get(context.Background(), machine.GetName(), metav1.GetOptions{})
	_ = unstructured.SetNestedField(storedMachine.Object, "Admitted", "status", "phase")
	_ = unstructured.SetNestedField(storedMachine.Object, "retry-001", "status", "rejoinRequestID")
	firstOperationName := rejoinOperationName("s1u-07-x86", "retry-001")
	_ = unstructured.SetNestedField(storedMachine.Object, firstOperationName, "status", "operation")
	if _, err := client.Resource(machineGVR).Namespace(machineNamespace).Update(context.Background(), storedMachine, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	active := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeOperation", "metadata": map[string]any{"name": firstOperationName, "namespace": "agent-a"}, "spec": map[string]any{"action": "Join", "nodeName": "s1u-07-x86", "clusterID": "cluster-a"}, "status": map[string]any{"phase": "Running"}}}
	if _, err := client.Resource(nodeOperationGVR).Namespace("agent-a").Create(context.Background(), active, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	status, again = nodeAdmissionCall(t, g.handleRejoinMachine, "POST", "/api/v1/station/machines/lab-88/rejoin", "lab-88", body)
	if status != http.StatusAccepted || !strings.Contains(string(again), `"phase":"Running"`) {
		t.Fatalf("replay while controller is running status=%d response=%s", status, again)
	}
	active, err = client.Resource(nodeOperationGVR).Namespace("agent-a").Get(context.Background(), firstOperationName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	active.Object["status"] = map[string]any{"phase": "Failed"}
	if _, err := client.Resource(nodeOperationGVR).Namespace("agent-a").Update(context.Background(), active, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	status, retry := nodeAdmissionCall(t, g.handleRejoinMachine, "POST", "/api/v1/station/machines/lab-88/rejoin", "lab-88", `{"cluster_id":"cluster-a","request_id":"retry-002","robot_model":"s1u"}`)
	if status != http.StatusAccepted || !strings.Contains(string(retry), "join-rejoin-") {
		t.Fatalf("retry after failed Join status=%d response=%s", status, retry)
	}
	storedMachine, _ = client.Resource(machineGVR).Namespace(machineNamespace).Get(context.Background(), machine.GetName(), metav1.GetOptions{})
	_ = unstructured.SetNestedField(storedMachine.Object, "retry-002", "status", "rejoinRequestID")
	_ = unstructured.SetNestedField(storedMachine.Object, rejoinOperationName("s1u-07-x86", "retry-002"), "status", "operation")
	if _, err := client.Resource(machineGVR).Namespace(machineNamespace).Update(context.Background(), storedMachine, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	active = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeOperation", "metadata": map[string]any{"name": rejoinOperationName("s1u-07-x86", "retry-002"), "namespace": "agent-a"}, "spec": map[string]any{"action": "Join", "nodeName": "s1u-07-x86", "clusterID": "cluster-a"}, "status": map[string]any{"phase": "Running"}}}
	if _, err := client.Resource(nodeOperationGVR).Namespace("agent-a").Create(context.Background(), active, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	status, conflict := nodeAdmissionCall(t, g.handleRejoinMachine, "POST", "/api/v1/station/machines/lab-88/rejoin", "lab-88", `{"cluster_id":"cluster-a","request_id":"retry-003","robot_model":"s1u"}`)
	if status != http.StatusConflict || !strings.Contains(string(conflict), "REJOIN_IN_PROGRESS") {
		t.Fatalf("active Join status=%d response=%s", status, conflict)
	}
}

func TestMachineRejoinHTTPRoutesReturnAndPollTheCurrentOperation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cluster := stationHandlerCluster("cluster-a", "agent-a")
	machine := handlerMachine("lab-88", "Joined", "", "1794925030888")
	_ = unstructured.SetNestedField(machine.Object, "cluster-a", "status", "clusterID")
	_ = unstructured.SetNestedField(machine.Object, "s1u-07-x86", "status", "nodeName")
	_ = unstructured.SetNestedField(machine.Object, "s1u", "spec", "admit", "model")
	class := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeClass", "metadata": map[string]any{"name": "s1u"}, "spec": map[string]any{
		"kind": "Robot", "model": "s1u", "access": map[string]any{"entry": map[string]any{"port": int64(22), "user": "orin"}, "node": map[string]any{"address": "192.168.0.10", "port": int64(22), "user": "robot"}},
	}}}
	admission := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission", "metadata": map[string]any{"name": "s1u-07-x86", "namespace": "agent-a"}, "spec": map[string]any{"clusterID": "cluster-a", "serial": "1794925030888", "nodeClass": "s1u", "machine": map[string]any{"category": "Robot", "model": "s1u"}}, "status": map[string]any{"phase": "Joined"}}}
	model := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "RobotModel", "metadata": map[string]any{"name": "s1u"}, "spec": map[string]any{"displayName": "S1U"}}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		stationClusterGVR: "ClusterList", machineGVR: "MachineList", nodeAdmissionGVR: "NodeAdmissionList", nodeClassGVR: "NodeClassList", robotModelGVR: "RobotModelList", nodeOperationGVR: "NodeOperationList",
	}, cluster, machine, admission, class, model)
	key := []byte(strings.Repeat("r", 32))
	g := &Gateway{astribotState: astribotState{stationClient: client}, jwtSigningKey: key}
	token, _, err := g.issueJWT("fixture-admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	g.RegisterRoutes(router)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		router.ServeHTTP(recorder, request)
		return recorder
	}
	response := call(http.MethodPost, "/api/v1/station/machines/lab-88/rejoin", `{"cluster_id":"cluster-a","request_id":"retry-http-001"}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	var accepted struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	name, _, _ := unstructured.NestedString(accepted.Data, "metadata", "name")
	if name == "" || response.Header().Get("Location") != "/api/v1/station/node-operations/"+name+"?cluster_id=cluster-a" {
		t.Fatalf("POST must return a pollable Location: headers=%v body=%s", response.Header(), response.Body.String())
	}
	operation := &unstructured.Unstructured{Object: accepted.Data}
	operation.Object["metadata"].(map[string]any)["namespace"] = "agent-a"
	operation.Object["status"] = map[string]any{"phase": "Failed", "stage": "Preflight", "conditions": []any{map[string]any{"type": "Progressing", "status": "False", "reason": "RejoinStateUnavailable", "message": "local WireGuard identity does not match NodeAdmission"}}}
	if _, err := client.Resource(nodeOperationGVR).Namespace("agent-a").Create(t.Context(), operation, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pollPath := response.Header().Get("Location")
	poll := call(http.MethodGet, pollPath, "")
	var current map[string]any
	if poll.Code != http.StatusOK || json.Unmarshal(poll.Body.Bytes(), &current) != nil || current["data"] != nil {
		t.Fatalf("GET must return the raw NodeOperation: status=%d body=%s", poll.Code, poll.Body.String())
	}
	phase, _, _ := unstructured.NestedString(current, "status", "phase")
	if phase != "Failed" || !strings.Contains(poll.Body.String(), "RejoinStateUnavailable") {
		t.Fatalf("GET lost the terminal failure status: %s", poll.Body.String())
	}
}
