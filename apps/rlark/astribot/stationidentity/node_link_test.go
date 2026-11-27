package stationidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func rlarkNode(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rlinf.io/v1alpha1", "kind": "Node",
		"metadata": map[string]interface{}{"namespace": namespace, "name": name, "annotations": map[string]interface{}{"rlark.io/cameras": "{}"}},
	}}
}

// sendLink posts a link record for the node's key with the proof of a fresh challenge.
func (n documentNode) sendLink(server http.Handler, link string) (int, string) {
	n.t.Helper()
	_, challenged := n.ask(server, "", "")
	body := `{"public_key":"` + n.public() + `","challenge":"` + challenged.Challenge + `","proof":"` + n.prove(challenged) + `","link":` + link + `}`
	response := apiRequest(server, http.MethodPost, "/v1/network/link", body, "")
	return response.Code, response.Body.String()
}

// An enrolled node's record lands on its RLark Node as the typed record; nothing else does.
func TestNodeLinkIsKeptOnTheRLarkNodeOfAnEnrolledNode(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	node, admitted, unplaced := newDocumentNode(t), newDocumentNode(t), newDocumentNode(t)
	api, client := testNetworkDocuments(t, &now,
		documentHolder("NetworkEnrollment", "farm", "gpu-01", "Enrolled", node.public(), `{}`),
		documentHolder("NetworkEnrollment", "farm", "gpu-02", "Enrolled", unplaced.public(), `{}`),
		documentHolder("NodeAdmission", "farm", "desk", "Redeemed", admitted.public(), `{}`),
		rlarkNode("farm", "gpu-01"), rlarkNode("farm", "desk"))
	record := `{"state":"up","since":1800000000,"restartOwner":"s1u-robot","restarts":1,"actions":[{"at":1799999000,"step":"restart","result":"restart 1 asked of s1u-robot"},{"at":1800000000,"step":"recovered","result":"r"}]}`
	if status, body := node.sendLink(api, record); status != http.StatusNoContent {
		t.Fatalf("enrolled: %d %s", status, body)
	}
	stored, err := client.Resource(rlarkNodeGVR).Namespace("farm").Get(context.Background(), "gpu-01", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.GetAnnotations(); got["rlark.io/link"] != record || got["rlark.io/cameras"] != "{}" {
		t.Fatalf("annotations %v", got)
	}

	// A record that is not one: said so, and nothing written.
	actions := make([]string, 11)
	for i := range actions {
		actions[i] = `{"at":1,"step":"down","result":"r"}`
	}
	for name, bad := range map[string]string{
		"with a field it does not have": `{"state":"up","since":1,"restarts":0,"actions":[],"rlark":1800000000}`,
		"of no state":                   `{"state":"lost","since":1,"restarts":0,"actions":[]}`,
		"of eleven actions":             `{"state":"down","since":1,"restarts":0,"actions":[` + strings.Join(actions, ",") + `]}`,
		"too long":                      `{"state":"down","since":1,"restarts":0,"uplink":"` + strings.Repeat("x", nodeLinkLimit) + `","actions":[]}`,
	} {
		if status, body := node.sendLink(api, bad); status != http.StatusBadRequest {
			t.Errorf("a record %s: %d %s", name, status, body)
		}
	}
	if stored, _ := client.Resource(rlarkNodeGVR).Namespace("farm").Get(context.Background(), "gpu-01", metav1.GetOptions{}); stored.GetAnnotations()["rlark.io/link"] != record {
		t.Fatalf("a refused record was written: %v", stored.GetAnnotations())
	}

	// A machine admitted and not enrolled has no RLark Node yet, even where one of its name stands; an enrolled
	// node without one is told so.
	if status, body := admitted.sendLink(api, record); status != http.StatusNotFound {
		t.Fatalf("admitted: %d %s", status, body)
	}
	if desk, _ := client.Resource(rlarkNodeGVR).Namespace("farm").Get(context.Background(), "desk", metav1.GetOptions{}); desk.GetAnnotations()["rlark.io/link"] != "" {
		t.Fatalf("an admission's record was written: %v", desk.GetAnnotations())
	}
	if status, body := unplaced.sendLink(api, record); status != http.StatusNotFound || !strings.Contains(body, "no RLark Node") {
		t.Fatalf("no RLark Node: %d %s", status, body)
	}

	// Without the proof of the key: a challenge, and nothing written.
	response := apiRequest(api, http.MethodPost, "/v1/network/link", `{"public_key":"`+node.public()+`","link":{"state":"down","since":1,"restarts":0,"actions":[]}}`, "")
	var challenged documentAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &challenged); err != nil || response.Code != http.StatusUnauthorized || challenged.Challenge == "" {
		t.Fatalf("no proof: %d %s", response.Code, response.Body)
	}
	if stored, _ := client.Resource(rlarkNodeGVR).Namespace("farm").Get(context.Background(), "gpu-01", metav1.GetOptions{}); stored.GetAnnotations()["rlark.io/link"] != record {
		t.Fatalf("an unproven record was written: %v", stored.GetAnnotations())
	}
}
