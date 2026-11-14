package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

type fakeStationBootstrap struct{ paths []string }

func (f *fakeStationBootstrap) Address() string { return "10.11.14.128:18443" }

func (f *fakeStationBootstrap) Digest(_ context.Context, path string) (string, error) {
	f.paths = append(f.paths, path)
	if path == "/v1/bootstrap/ca" {
		return "cadigest", nil
	}
	return "uplinkdigest", nil
}

func nodeAdmissionGateway(objects ...runtime.Object) (*Gateway, *fakeStationBootstrap) {
	network := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "SiteNetwork",
		"metadata": map[string]interface{}{"name": "default", "namespace": "station-system"},
		"spec": map[string]interface{}{"pool": "10.254.240.0/24", "sites": []interface{}{
			map[string]interface{}{"name": "office", "clusters": []interface{}{"cluster-a"}},
			map[string]interface{}{"name": "farm", "leafUplink": map[string]interface{}{"gateway": "192.168.0.11",
				"setup": map[string]interface{}{"addon": "s1u-robot", "path": "uplink.sh", "login": "nvidia"}}},
		}},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		stationClusterGVR: "ClusterList", nodeAdmissionGVR: "NodeAdmissionList", siteNetworkGVR: "SiteNetworkList",
	}, append([]runtime.Object{stationHandlerCluster("cluster-a", "agent-a"), network}, objects...)...)
	bootstrap := &fakeStationBootstrap{}
	return &Gateway{stationClient: client, stationBootstrap: bootstrap}, bootstrap
}

func nodeAdmissionCall(t *testing.T, handler gin.HandlerFunc, method, path, name, body string) (int, []byte) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	ctx.Params = gin.Params{{Key: "name", Value: name}}
	ctx.Set(authSubjectKey, "alice")
	handler(ctx)
	return recorder.Code, recorder.Body.Bytes()
}

func storedAdmission(t *testing.T, g *Gateway, name string) *unstructured.Unstructured {
	t.Helper()
	object, err := g.stationClient.Resource(nodeAdmissionGVR).Namespace("agent-a").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func TestIssueNodeAdmissionStoresOnlyTheHashAndAnswersTheCommandOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, bootstrap := nodeAdmissionGateway()
	status, body := nodeAdmissionCall(t, g.handleIssueNodeAdmission, "POST", "/api/v1/station/node-admissions", "",
		`{"cluster_id":"cluster-a","node_name":"s1u-09-x86","api_server":"rs-cp:6443","labels":{"rlark.io/robot-id":"s1u-09"},"taints":["rlark.io/node-category=robot:NoSchedule"],"ttl_seconds":600}`)
	if status != 201 {
		t.Fatalf("status %d: %s", status, body)
	}
	var issued issuedNodeAdmission
	if err := json.Unmarshal(body, &issued); err != nil {
		t.Fatal(err)
	}
	if len(issued.Code) != joinCodeLength || strings.Trim(issued.Code, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
		t.Fatalf("code %q", issued.Code)
	}
	want := `c=$(mktemp) && curl --connect-timeout 15 -fsSk https://10.11.14.128:18443/v1/bootstrap/ca -o $c && echo "cadigest  $c" | sha256sum -c - && curl -fsS --cacert $c https://10.11.14.128:18443/b/` + issued.Code + ` | sudo sh`
	if issued.Command != want {
		t.Fatalf("command\n%s\nwant\n%s", issued.Command, want)
	}
	if issued.Admission.State != "Pending" || issued.Admission.IssuedBy != "alice" || issued.Admission.Labels["rlark.io/robot-id"] != "s1u-09" {
		t.Fatalf("admission %+v", issued.Admission)
	}
	stored := storedAdmission(t, g, "s1u-09-x86")
	digest := sha256.Sum256([]byte(issued.Code))
	if hash, _, _ := unstructured.NestedString(stored.Object, "spec", "codeHash"); hash != hex.EncodeToString(digest[:]) {
		t.Fatalf("stored hash %q", hash)
	}
	raw, _ := json.Marshal(stored.Object)
	if strings.Contains(string(raw), issued.Code) {
		t.Fatal("the code itself is stored")
	}
	expires, _ := time.Parse(time.RFC3339, issued.Admission.ExpiresAt)
	if d := time.Until(expires); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("expires in %s", d)
	}
	if strings.Join(bootstrap.paths, ",") != "/v1/bootstrap/ca" {
		t.Fatalf("Station API reads %v", bootstrap.paths)
	}

	// The list never carries the code or its hash.
	status, body = nodeAdmissionCall(t, g.handleListNodeAdmissions, "GET", "/api/v1/station/node-admissions?cluster_id=cluster-a", "", "")
	if status != 200 || strings.Contains(string(body), issued.Code) || strings.Contains(string(body), hex.EncodeToString(digest[:])) || !strings.Contains(string(body), `"state":"Pending"`) {
		t.Fatalf("list %d: %s", status, body)
	}
}

func TestIssueNodeAdmissionBehindADeviceLetsTheMachineOutFirst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, bootstrap := nodeAdmissionGateway()
	status, body := nodeAdmissionCall(t, g.handleIssueNodeAdmission, "POST", "/api/v1/station/node-admissions", "",
		`{"cluster_id":"cluster-a","node_name":"s1u-10-x86","api_server":"rs-cp:6443","site":"farm"}`)
	var issued issuedNodeAdmission
	_ = json.Unmarshal(body, &issued)
	if status != 201 || !strings.HasPrefix(issued.Command, `ssh -t nvidia@192.168.0.11 'f=$(mktemp) c=$(mktemp) && `) ||
		!strings.Contains(issued.Command, `/b/`+issued.Code+`/uplink -o $f && echo "uplinkdigest  $f" | sha256sum -c - && sudo sh $f' && c=$(mktemp)`) {
		t.Fatalf("status %d command %s", status, issued.Command)
	}
	if strings.Join(bootstrap.paths, ",") != "/v1/bootstrap/ca,/b/"+issued.Code+"/uplink" {
		t.Fatalf("Station API reads %v", bootstrap.paths)
	}
}

func TestIssueNodeAdmissionRejectsWhatTheManagementPlaneWouldNot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, _ := nodeAdmissionGateway()
	for _, body := range []string{
		`{"cluster_id":"cluster-a","node_name":"S1U","api_server":"rs-cp:6443"}`,
		`{"cluster_id":"cluster-a","node_name":"n","api_server":"rs-cp"}`,
		`{"cluster_id":"cluster-a","node_name":"n","api_server":"rs-cp:6443","taints":["a=b"]}`,
		`{"cluster_id":"cluster-a","node_name":"n","api_server":"rs-cp:6443","address":"fd00::1"}`,
		`{"cluster_id":"cluster-a","node_name":"n","api_server":"rs-cp:6443","site":"moon"}`,
		`{"cluster_id":"cluster-a","node_name":"n","api_server":"rs-cp:6443","ttl_seconds":60}`,
		`{"cluster_id":"cluster-a","node_name":"n","api_server":"rs-cp:6443","code":"abcdefgh"}`,
	} {
		if status, answer := nodeAdmissionCall(t, g.handleIssueNodeAdmission, "POST", "/api/v1/station/node-admissions", "", body); status != 400 {
			t.Errorf("%s: %d %s", body, status, answer)
		}
	}
	g.stationBootstrap = nil
	if status, _ := nodeAdmissionCall(t, g.handleIssueNodeAdmission, "POST", "/api/v1/station/node-admissions", "", `{}`); status != 503 {
		t.Errorf("without bootstrap: %d", status)
	}
}

func redeemedAdmission() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission",
		"metadata": map[string]interface{}{"name": "s1u-01-x86", "namespace": "agent-a", "generation": int64(3)},
		"spec": map[string]interface{}{"clusterID": "cluster-a", "codeHash": strings.Repeat("a", 64), "expiresAt": "2026-11-14T10:00:00Z",
			"apiServer": "rs-cp:6443", "address": "10.254.240.9", "labels": map[string]interface{}{"old": "label"},
			"publicKey": "pk", "redeemedAt": "2026-11-14T09:00:00Z", "issuedBy": "bob", "issuedAt": "2026-11-14T08:00:00Z"},
		"status": map[string]interface{}{"observedGeneration": int64(3), "phase": "Joined", "address": "10.254.240.9"},
	}}
}

func TestNewCodeForAnAdmittedNodeOpensItAgain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, _ := nodeAdmissionGateway(redeemedAdmission())
	status, body := nodeAdmissionCall(t, g.handleListNodeAdmissions, "GET", "/api/v1/station/node-admissions?cluster_id=cluster-a", "", "")
	if status != 200 || !strings.Contains(string(body), `"state":"Joined"`) || !strings.Contains(string(body), `"public_key":"pk"`) {
		t.Fatalf("list %d: %s", status, body)
	}
	// Revoking a code a machine has redeemed is refused.
	if status, _ := nodeAdmissionCall(t, g.handleRevokeNodeAdmission, "POST", "/api/v1/station/node-admissions/s1u-01-x86/revoke?cluster_id=cluster-a", "s1u-01-x86", ""); status != 409 {
		t.Fatalf("revoke of a redeemed code: %d", status)
	}
	// Reissue keeps what the admission declares and clears the redemption.
	status, body = nodeAdmissionCall(t, g.handleReissueNodeAdmission, "POST", "/api/v1/station/node-admissions/s1u-01-x86/reissue?cluster_id=cluster-a", "s1u-01-x86", "")
	var issued issuedNodeAdmission
	_ = json.Unmarshal(body, &issued)
	if status != 200 || issued.Admission.State != "Pending" || issued.Admission.IssuedBy != "alice" || issued.Admission.Address != "10.254.240.9" || issued.Admission.Labels["old"] != "label" || issued.Admission.PublicKey != "" {
		t.Fatalf("reissue %d: %s", status, body)
	}
	// A new admission for the node replaces what it declares, labels as one value.
	status, body = nodeAdmissionCall(t, g.handleIssueNodeAdmission, "POST", "/api/v1/station/node-admissions", "",
		`{"cluster_id":"cluster-a","node_name":"s1u-01-x86","api_server":"rs-cp:6443","labels":{"new":"label"}}`)
	if status != 200 {
		t.Fatalf("readmit %d: %s", status, body)
	}
	labels, _, _ := unstructured.NestedStringMap(storedAdmission(t, g, "s1u-01-x86").Object, "spec", "labels")
	if len(labels) != 1 || labels["new"] != "label" {
		t.Fatalf("labels %v", labels)
	}
}

func TestRevokeEndsAnOpenCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, _ := nodeAdmissionGateway()
	if status, body := nodeAdmissionCall(t, g.handleIssueNodeAdmission, "POST", "/api/v1/station/node-admissions", "",
		`{"cluster_id":"cluster-a","node_name":"n1","api_server":"rs-cp:6443"}`); status != 201 {
		t.Fatalf("issue %d: %s", status, body)
	}
	status, body := nodeAdmissionCall(t, g.handleRevokeNodeAdmission, "POST", "/api/v1/station/node-admissions/n1/revoke?cluster_id=cluster-a", "n1", "")
	if status != 200 || !strings.Contains(string(body), `"state":"Expired"`) {
		t.Fatalf("revoke %d: %s", status, body)
	}
	expires, _, _ := unstructured.NestedString(storedAdmission(t, g, "n1").Object, "spec", "expiresAt")
	if at, _ := time.Parse(time.RFC3339, expires); time.Now().Before(at) {
		t.Fatalf("still open until %s", expires)
	}
	if status, _ := nodeAdmissionCall(t, g.handleRevokeNodeAdmission, "POST", "/api/v1/station/node-admissions/n1/revoke?cluster_id=cluster-a", "n1", ""); status != 200 {
		t.Fatalf("second revoke %d", status)
	}
	if status, _ := nodeAdmissionCall(t, g.handleRevokeNodeAdmission, "POST", "/api/v1/station/node-admissions/none/revoke?cluster_id=cluster-a", "none", ""); status != 404 {
		t.Fatalf("revoke of nothing %d", status)
	}
}
