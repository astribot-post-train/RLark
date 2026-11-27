package workflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	clientset "github.com/rlinf/rlark/api/kubeclients/clientset/versioned/fake"
	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const (
	joinPassword = "Password-Of-The-Joined-Machine"
	joinKey      = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk="
	hubKey       = "hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh="
)

// joinBox is a machine joined over SSH: each script is answered by answer, on the join's own goroutine.
type joinBox struct {
	mu     sync.Mutex
	ran    []string
	stdin  []string
	answer func(script string) (string, error)
}

func (m *joinBox) Run(_ context.Context, script string, stdin io.Reader, line func(string)) (string, error) {
	m.mu.Lock()
	m.ran = append(m.ran, script)
	data := []byte{}
	if stdin != nil {
		data, _ = io.ReadAll(stdin)
	}
	m.stdin = append(m.stdin, string(data))
	m.mu.Unlock()
	out, err := m.answer(script)
	if line != nil {
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			line(l)
		}
	}
	return out, err
}

func (m *joinBox) Close() error { return nil }

func (m *joinBox) scripts(marker string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for i, script := range m.ran {
		if strings.Contains(script, marker) {
			out = append(out, script+"\x00"+m.stdin[i])
		}
	}
	return out
}

// joinScene is the management plane and the machine of a join test.
type joinScene struct {
	t         *testing.T
	g         *Service
	bootstrap *fakeStationBootstrap
	box       *joinBox
	// reach is what the machine reaches Station API at; precheck the extra lines of its precheck.
	reach    map[string]bool
	precheck string
	// probe answers the tunnel stage's rounds; follow is what the join's log says, active whether its unit runs,
	// last the log's last line when it does not; issued whether the NodeJoin held the code's issue when the line started.
	probe  func(round int) string
	rounds int
	follow string
	active bool
	last   string
	issued bool
}

func joinNetwork(nodes []interface{}, declared []interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "SiteNetwork",
		"metadata": map[string]interface{}{"name": "default", "namespace": "station-system"},
		"spec": map[string]interface{}{
			"pool": "10.254.240.0/24", "nodes": nodes,
			"sites": []interface{}{map[string]interface{}{"name": "baidu", "clusters": []interface{}{"cluster-a"}}},
		},
		"status": map[string]interface{}{"declared": declared},
	}}
}

// boxDocument is the document station-controller publishes for the machine: it dials one hub.
const boxDocument = `{"interface":"st-wg0","address":"10.254.240.6/32","peers":[{"publicKey":"` + hubKey + `","endpoint":"5.6.7.8:51820","keepaliveSeconds":25,"allowedIPs":["10.254.240.3/32"]}]}`

func newJoinScene(t *testing.T, network *unstructured.Unstructured, rlark ...runtime.Object) *joinScene {
	t.Helper()
	gin.SetMode(gin.TestMode)
	saved := joinWaitStep
	joinWaitStep = time.Millisecond
	t.Cleanup(func() { joinWaitStep = saved })
	s := &joinScene{t: t, reach: map[string]bool{}, follow: "10:00:00 loading the installer\n10:00:02 admitted as box in cluster-a\n10:01:00 joined\n" + sshJoinExit + "0"}
	s.bootstrap = &fakeStationBootstrap{}
	station := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		siteNetworkGVR: "SiteNetworkList", nodeAdmissionGVR: "NodeAdmissionList",
		networkEnrollmentGVR: "NetworkEnrollmentList", nodeOperationGVR: "NodeOperationList", nodeClassGVR: "NodeClassList",
	}, network)
	kube := clientset.NewSimpleClientset(rlark...)
	var serial int
	kube.PrependReactor("create", "nodejoins", func(action ktesting.Action) (bool, runtime.Object, error) {
		join := action.(ktesting.CreateAction).GetObject().(*rlarkv1alpha1.NodeJoin)
		serial++
		join.UID = types.UID(fmt.Sprintf("created-join-%d", serial))
		return false, nil, nil
	})
	s.g = &Service{kubeClient: kube, rawClient: kubefake.NewSimpleClientset(provisionTestRecord()), stationClient: station, stationBootstrap: s.bootstrap, http: testHTTP()}
	s.g.namespace = "default"
	s.box = &joinBox{answer: s.answer}
	s.g.provisionDial = func(_ context.Context, _ rlarkv1alpha1.ClusterProvisionMachine, password []byte, _ string) (provisioning.Machine, string, error) {
		if string(password) != joinPassword {
			return nil, "", &nodessh.Error{Reason: nodessh.LoginRejected, Err: errors.New("the machine refused the password")}
		}
		return s.box, "SHA256:box", nil
	}
	return s
}

func (s *joinScene) answer(script string) (string, error) {
	ca := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	switch {
	case script == "hostname":
		return "Box\n", nil
	case strings.Contains(script, `echo "hostname=`):
		return "hostname=box\nos=Ubuntu 22.04\nkernel=5.15\nsystemd=yes\ntar=yes\nwireguard=yes\ndiskfree=200000000000\naddress=192.168.22.2\n" + s.precheck, nil
	case strings.Contains(script, "/v1/bootstrap/ca || echo code=000") && !strings.Contains(script, "--status"):
		for address, reached := range s.reach {
			if reached && strings.Contains(script, "https://"+address+"/") {
				return "code=200\n", nil
			}
		}
		return "code=000\n", nil
	case strings.Contains(script, "installer.tar.new"):
		return "sha256=" + sha256Hex(fakeInstaller) + "\npublickey=" + joinKey + "\nca=" + ca + "\n", nil
	case strings.Contains(script, "install -m 755 $D/bin/station-gateway"):
		return "publickey=" + joinKey + "\nca=" + ca + "\n", nil
	case strings.Contains(script, "station-bootstrap tunnel"):
		return "10:00:00 tunnel service started on st-wg0: 10.254.240.6/32\n", nil
	case strings.Contains(script, "--status 2>/dev/null | sed"):
		s.rounds++
		return s.probe(s.rounds), nil
	case strings.Contains(script, "&& echo active=yes"):
		if s.active {
			return "active=yes\nlast=10:00:02 admitted as box in cluster-a\n", nil
		}
		return "last=" + s.last + "\n", nil
	case strings.Contains(script, "systemd-run --unit="+sshJoinUnit):
		j, err := s.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(context.Background(), "cluster-a.box", metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		s.issued = j.Status.Facts.AdmissionIssuedAt != nil
		// station-controller's part: the machine redeems and its admission becomes Joined.
		admissions := s.g.stationClient.Resource(nodeAdmissionGVR).Namespace("agent-a")
		admission, err := admissions.Get(context.Background(), "box", metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		_ = unstructured.SetNestedField(admission.Object, joinKey, "spec", "publicKey")
		_ = unstructured.SetNestedField(admission.Object, "Joined", "status", "phase")
		_, err = admissions.Update(context.Background(), admission, metav1.UpdateOptions{})
		return "", err
	case strings.Contains(script, "grep -E '^("+joinAttemptLogPrefix):
		return s.follow, nil
	case strings.Contains(script, "tail -n +1 -F "+sshJoinLog):
		return s.follow, nil
	case strings.Contains(script, "rlark-station-gateway.service"):
		return "cleared=yes\n", nil
	}
	return "", errors.New("no answer to " + script)
}

func (s *joinScene) call(method, path, body string) (int, map[string]any) {
	s.t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(authSubjectKey, "alice"); c.Next() })
	router.GET("/station/node-joins", s.g.HandleListNodeJoins)
	router.POST("/station/node-joins", s.g.HandleCreateNodeJoin)
	router.GET("/station/node-joins/:name", s.g.HandleGetNodeJoin)
	router.POST("/station/node-joins/:name/retry", s.g.HandleRetryNodeJoin)
	router.POST("/station/node-joins/:name/clear", s.g.HandleClearNodeJoin)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	var out map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &out)
	return recorder.Code, out
}

func (s *joinScene) create() {
	s.t.Helper()
	if code, out := s.call(http.MethodPost, "/station/node-joins", `{"cluster_id":"cluster-a","address":"1.2.3.4","port":8023,"user":"root","password":"`+joinPassword+`"}`); code != http.StatusCreated {
		s.t.Fatalf("create = %d %v", code, out)
	}
}

func (s *joinScene) wait() *rlarkv1alpha1.NodeJoin {
	s.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.g.nodeJoins.mu.Lock()
		running := s.g.nodeJoins.running["cluster-a.box"]
		s.g.nodeJoins.mu.Unlock()
		if !running {
			j, err := s.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(context.Background(), "cluster-a.box", metav1.GetOptions{})
			if err != nil {
				s.t.Fatal(err)
			}
			return j
		}
	}
	s.t.Fatal("the join did not end")
	return nil
}

func joinStageOf(j *rlarkv1alpha1.NodeJoin, name string) rlarkv1alpha1.ClusterProvisionStage {
	for _, stage := range j.Status.Stages {
		if stage.Name == name {
			return stage
		}
	}
	return rlarkv1alpha1.ClusterProvisionStage{}
}

// readyNode, enrolled: the node the join makes, as its cluster and station-controller show it.
func readyNode(gpus int64) *rlarkv1alpha1.Node {
	capacity := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("180"), corev1.ResourceMemory: resource.MustParse("1Ti")}
	if gpus > 0 {
		capacity["nvidia.com/gpu"] = *resource.NewQuantity(gpus, resource.DecimalSI)
	}
	return &rlarkv1alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "cluster-a"}, Status: rlarkv1alpha1.NodeStatus{
		Phase: rlarkv1alpha1.NodeOnline, Capacity: capacity, NodeInfo: rlarkv1alpha1.NodeInfo{OperatingSystem: "Ubuntu 22.04", KernelVersion: "5.15"},
		Conditions: []metav1.Condition{{Type: rlarkv1alpha1.ConditionTypeReady, Status: metav1.ConditionTrue}},
	}}
}

func (s *joinScene) enrolled() {
	s.t.Helper()
	_, err := s.g.stationClient.Resource(networkEnrollmentGVR).Namespace("agent-a").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NetworkEnrollment", "metadata": map[string]interface{}{"name": "box", "namespace": "agent-a"},
		"spec": map[string]interface{}{"clusterID": "cluster-a"}, "status": map[string]interface{}{"phase": "Enrolled", "address": "10.254.240.6", "publicKey": joinKey},
	}}, metav1.CreateOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
}

// A machine that reaches Station API at its bootstrap address joins without a tunnel of its own: the three tunnel
// stages are Skipped with why; the code's line is followed to its end, line by line; the node is Ready, enrolled, its
// GPUs labelled and offered. The password is nowhere in what is kept.
func TestAMachineThatReachesStationAPIJoinsInStages(t *testing.T) {
	s := newJoinScene(t, joinNetwork(nil, nil), readyNode(2),
		&rlarkv1alpha1.Addon{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-gpu", Namespace: "cluster-a"}})
	s.nativeNodeUID("original-native-uid")
	s.follow = "RLARK_SSH_JOIN created-join-1\nRLARK_JOIN_NODE_UID box original-native-uid\n" + s.follow
	s.reach["10.11.14.128:18443"] = true
	s.precheck = "gpu=NVIDIA H20\ngpu=NVIDIA H20\n"
	s.enrolled()
	s.create()
	j := s.wait()
	if j.Annotations[nodeJoinNodeUID] != "original-native-uid" {
		t.Fatalf("native node UID was not recorded: %v", j.Annotations)
	}
	if j.Status.Phase != rlarkv1alpha1.ClusterProvisionSucceeded || j.Status.Facts.Route != rlarkv1alpha1.NodeJoinRouteDirect || j.Spec.NodeName != "box" || j.Status.HostKey != "SHA256:box" {
		t.Fatalf("status %+v", j.Status)
	}
	for _, name := range []string{stageBundle, stageDeclare, stageTunnel} {
		if stage := joinStageOf(j, name); stage.State != rlarkv1alpha1.ClusterProvisionStageSkipped || !strings.Contains(stage.Readback, "直接到得了 Station API") {
			t.Fatalf("stage %s: %+v", name, stage)
		}
	}
	precheck := joinStageOf(j, stagePrecheck).Readback
	if !strings.Contains(precheck, "root@1.2.3.4:8023，主机密钥 SHA256:box") || !strings.Contains(precheck, "2 × NVIDIA H20") || !strings.Contains(precheck, "10.11.14.128:18443 直接可达") {
		t.Fatalf("precheck readback %q", precheck)
	}
	if install := joinStageOf(j, stageInstall); !strings.Contains(install.Readback, "码由 alice") || !strings.Contains(install.Readback, "rlark-join exit 0；10:01:00 joined") {
		t.Fatalf("install %+v", install)
	}
	if ready := joinStageOf(j, stageNodeReady).Readback; !strings.Contains(ready, "Ready，Online") || !strings.Contains(ready, "180 CPU") {
		t.Fatalf("node-ready %q", ready)
	}
	if station := joinStageOf(j, stageStation).Readback; station != "Enrolled 10.254.240.6；admission Joined" {
		t.Fatalf("station %q", station)
	}
	node, _ := s.g.kubeClient.RlinfV1alpha1().Nodes("cluster-a").Get(context.Background(), "box", metav1.GetOptions{})
	if node.Labels[acceleratorLabel] != "nvidia" || !strings.Contains(joinStageOf(j, stageGPU).Readback, "2 nvidia.com/gpu 可分配") {
		t.Fatalf("gpu: labels %v, %+v", node.Labels, joinStageOf(j, stageGPU))
	}
	line := s.box.scripts("systemd-run --unit=" + sshJoinUnit)
	if len(line) != 1 || !strings.Contains(line[0], "https://10.11.14.128:18443/b/") {
		t.Fatalf("the code's line %q", line)
	}
	stored, _ := json.Marshal(j)
	admission, _ := s.g.stationClient.Resource(nodeAdmissionGVR).Namespace("agent-a").Get(context.Background(), "box", metav1.GetOptions{})
	marks, _ := json.Marshal(admission.Object)
	if strings.Contains(string(stored), joinPassword) || strings.Contains(string(marks), joinPassword) || admission.GetAnnotations()[stationidentity.AdmissionSSHHostKeyAnnotation] != "SHA256:box" {
		t.Fatalf("the password is kept, or the host key is not: %s %s", stored, marks)
	}
	for _, script := range s.box.ran {
		if strings.Contains(script, joinPassword) {
			t.Fatalf("the password is on a command line: %q", script)
		}
	}
	joins, err := NodeJoinsOf(context.Background(), s.g.kubeClient, "cluster-a")
	if err != nil || len(joins) != 1 || joins[0].Status.Phase != rlarkv1alpha1.ClusterProvisionSucceeded || joins[0].Status.Stage != stageGPU {
		t.Fatalf("joins: %v, err: %v", joins, err)
	}
}

// 11-18, the second join of the Baidu machine: a machine declared with its key whose tunnel answers is joined through
// it, without its installer sent again or its declaration written again.
func TestAMachineWhoseTunnelAnswersIsNotGivenItAgain(t *testing.T) {
	declared := map[string]interface{}{"clusterID": "cluster-a", "nodeName": "box", "role": "Hub", "address": "10.254.240.6", "publicKey": joinKey}
	s := newJoinScene(t, joinNetwork([]interface{}{declared}, nil), readyNode(0))
	s.reach["10.254.240.1:18443"] = true
	s.precheck = "bundle=" + sha256Hex(fakeInstaller) + "\npublickey=" + joinKey + "\n"
	s.enrolled()
	before, _ := stationidentity.ReadSiteNetwork(context.Background(), s.g.stationClient)
	s.create()
	j := s.wait()
	if j.Status.Phase != rlarkv1alpha1.ClusterProvisionSucceeded || j.Status.Facts.Route != rlarkv1alpha1.NodeJoinRouteTunnelUp {
		t.Fatalf("status %+v", j.Status)
	}
	for _, name := range []string{stageBundle, stageDeclare, stageTunnel} {
		if stage := joinStageOf(j, name); stage.State != rlarkv1alpha1.ClusterProvisionStageSkipped || !strings.Contains(stage.Readback, "隧道已经通") {
			t.Fatalf("stage %s: %+v", name, stage)
		}
	}
	if joinStageOf(j, stageGPU).State != rlarkv1alpha1.ClusterProvisionStageSkipped {
		t.Fatalf("gpu %+v", joinStageOf(j, stageGPU))
	}
	for _, path := range s.bootstrap.paths {
		if path == "/v1/bootstrap/bundle" {
			t.Fatal("the installer was taken from Station API")
		}
	}
	after, _ := stationidentity.ReadSiteNetwork(context.Background(), s.g.stationClient)
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatal("the SiteNetwork was written")
	}
	if line := s.box.scripts("systemd-run"); len(line) != 1 || !strings.Contains(line[0], "https://10.254.240.1:18443/b/") {
		t.Fatalf("the line does not redeem through the tunnel: %q", line)
	}
}

// tunnelScene is a machine that reaches Station API only through a tunnel of its own, which dials one hub, enrolled
// as rlark-ksyun-gpu/10-0-2-57; the hub's reports are fresh. The machine holds Station API's installer already.
func tunnelScene(t *testing.T, step time.Duration) (*joinScene, func(peers ...map[string]interface{})) {
	t.Helper()
	s := newJoinScene(t, joinNetwork(nil, []interface{}{map[string]interface{}{"clusterID": "cluster-a", "nodeName": "box", "config": boxDocument}}))
	s.precheck = "bundle=" + sha256Hex(fakeInstaller) + "\n"
	ctx := context.Background()
	enrollments := s.g.stationClient.Resource(networkEnrollmentGVR).Namespace("ksyun")
	report := func(peers ...map[string]interface{}) {
		listed := []interface{}{}
		for _, peer := range peers {
			listed = append(listed, peer)
		}
		hub := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "station.rlark.io/v1alpha1", "kind": "NetworkEnrollment", "metadata": map[string]interface{}{"name": "10-0-2-57", "namespace": "ksyun"},
			"spec": map[string]interface{}{"clusterID": "rlark-ksyun-gpu"},
			// Reported now, as far as the wait's clock goes.
			"status": map[string]interface{}{"phase": "Enrolled", "publicKey": hubKey, "observation": map[string]interface{}{
				"reportedAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "wireguard": map[string]interface{}{"interface": "st-wg0", "peers": listed}}},
		}}
		if _, err := enrollments.Update(ctx, hub, metav1.UpdateOptions{}); apierrors.IsNotFound(err) {
			if _, err := enrollments.Create(ctx, hub, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	report()
	clock := time.Now()
	saved := joinClock
	joinClock = func() time.Time { clock = clock.Add(step); return clock }
	t.Cleanup(func() { joinClock = saved })
	return s, report
}

// The tunnel's wait goes on while the evidence moves: the hub takes the machine's key, the machine shakes hands, and
// Station API answers at the tunnel address, each moment recorded. The installer the machine holds is not sent again.
// The line that then fails stops the join at install, with its last line, on the admission too.
func TestTheTunnelIsWaitedForOnTheEvidenceAndTheInstallersFailureStopsTheJoin(t *testing.T) {
	s, report := tunnelScene(t, time.Second)
	s.probe = func(round int) string {
		switch round {
		case 1:
			return "status={\"peers\":[]}\ncode=000\n"
		case 2:
			report(map[string]interface{}{"name": "cluster-a/box"})
			return "status={\"peers\":[]}\ncode=000\n"
		case 3:
			return "status={\"peers\":[{\"publicKey\":\"" + hubKey + "\",\"lastHandshake\":\"2026-11-18T10:41:02Z\"}]}\ncode=000\n"
		}
		return "status={\"peers\":[{\"publicKey\":\"" + hubKey + "\",\"lastHandshake\":\"2026-11-18T10:41:02Z\"}]}\ncode=200\n"
	}
	s.follow = "10:00:00 loading the installer\n10:00:01 FAILED: kubeadm join: the API at cp.rlark-baidu-gpu does not answer\n" + sshJoinExit + "1"
	s.create()
	j := s.wait()
	if j.Status.Phase != rlarkv1alpha1.ClusterProvisionFailed || j.Status.Stage != stageInstall || !strings.Contains(joinStageOf(j, stageInstall).Message, "FAILED: kubeadm join") {
		t.Fatalf("status %+v", j.Status)
	}
	if bundle := joinStageOf(j, stageBundle); bundle.State != rlarkv1alpha1.ClusterProvisionStageDone || !strings.Contains(bundle.Readback, "not sent again") || len(s.box.scripts("installer.tar.new")) != 0 {
		t.Fatalf("bundle %+v", bundle)
	}
	facts := j.Status.Facts
	if !facts.DeclaredHub || facts.TunnelAddress == "" || facts.PublicKey != joinKey {
		t.Fatalf("facts %+v", facts)
	}
	tunnel := joinStageOf(j, stageTunnel)
	for _, want := range []string{"tunnel service started", "rlark-ksyun-gpu/10-0-2-57 拿到密钥", "rlark-ksyun-gpu/10-0-2-57 首次握手", "经隧道应答 200"} {
		if !strings.Contains(tunnel.Readback, want) {
			t.Fatalf("tunnel readback %q lacks %q", tunnel.Readback, want)
		}
	}
	if s.rounds != 4 {
		t.Fatalf("rounds %d", s.rounds)
	}
	admission, _ := s.g.stationClient.Resource(nodeAdmissionGVR).Namespace("agent-a").Get(context.Background(), "box", metav1.GetOptions{})
	if !strings.Contains(admission.GetAnnotations()[stationidentity.AdmissionInstallFailureAnnotation], "FAILED: kubeadm join") {
		t.Fatalf("annotations %v", admission.GetAnnotations())
	}
	if line := s.box.scripts("systemd-run"); len(line) != 1 || !strings.Contains(line[0], "https://10.254.240.1:18443/b/") {
		t.Fatalf("the line %q", line)
	}
}

// A hub whose fresh reports still lack the machine's key after the window its agent and gateway need is named.
func TestATunnelWhosePeerDoesNotTakeTheKeyFailsNamingIt(t *testing.T) {
	s, _ := tunnelScene(t, 30*time.Second)
	s.probe = func(int) string { return "status={\"peers\":[]}\ncode=000\n" }
	s.create()
	j := s.wait()
	if tunnel := joinStageOf(j, stageTunnel); j.Status.Stage != stageTunnel || !strings.Contains(tunnel.Message, "rlark-ksyun-gpu/10-0-2-57 在") || !strings.Contains(tunnel.Message, "没有取走新节点的文档") {
		t.Fatalf("tunnel %+v", tunnel)
	}
}

// Peers that hold the key and do not shake hands: UDP to the machine's endpoint does not arrive.
func TestATunnelWhosePeersHoldTheKeyButDoNotShakeHandsSaysUDP(t *testing.T) {
	s, report := tunnelScene(t, 30*time.Second)
	report(map[string]interface{}{"name": "cluster-a/box"})
	s.probe = func(int) string { return "status={\"peers\":[]}\ncode=000\n" }
	s.create()
	j := s.wait()
	if tunnel := joinStageOf(j, stageTunnel); j.Status.Stage != stageTunnel || !strings.Contains(tunnel.Message, "UDP 1.2.3.4:51820 没到这台机器") {
		t.Fatalf("tunnel %+v", tunnel)
	}
}

// A line whose end was not followed (the Gateway lost the machine) is retried by following the unit that still runs,
// with no new code.
func TestARetryOfInstallFollowsTheUnitThatStillRuns(t *testing.T) {
	s := newJoinScene(t, joinNetwork(nil, nil), readyNode(0))
	s.reach["10.11.14.128:18443"] = true
	s.enrolled()
	s.follow = "10:00:00 loading the installer"
	s.create()
	if j := s.wait(); j.Status.Stage != stageInstall || !strings.Contains(joinStageOf(j, stageInstall).Message, "没有写下它的结束") {
		t.Fatalf("status %+v", j.Status)
	}
	if code, _ := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/retry", `{}`); code != http.StatusBadRequest {
		t.Fatalf("a retry of install without the password = %d, want 400", code)
	}
	s.active, s.follow = true, "10:00:02 admitted as box in cluster-a\n10:01:00 joined\n"+sshJoinExit+"0"
	if code, out := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/retry", `{"password":"`+joinPassword+`"}`); code != http.StatusAccepted {
		t.Fatalf("retry = %d %v", code, out)
	}
	if j := s.wait(); j.Status.Phase != rlarkv1alpha1.ClusterProvisionSucceeded || len(s.box.scripts("systemd-run")) != 1 {
		t.Fatalf("status %+v, the line started %d times", j.Status, len(s.box.scripts("systemd-run")))
	}
}

// The code's issue is written before its line starts: a line that ended while the Gateway was down is read back on
// the retry, not run again with a new code.
func TestALineThatEndedWhileTheGatewayWasDownIsNotRunAgain(t *testing.T) {
	s := newJoinScene(t, joinNetwork(nil, nil), readyNode(0))
	s.nativeNodeUID("original-native-uid")
	s.reach["10.11.14.128:18443"] = true
	s.enrolled()
	s.follow = "10:00:00 loading the installer"
	s.create()
	if j := s.wait(); j.Status.Stage != stageInstall || !s.issued {
		t.Fatalf("status %+v; the issue was written before the line: %v", j.Status, s.issued)
	}
	s.last = sshJoinExit + "0"
	s.follow = "RLARK_SSH_JOIN created-join-1\nRLARK_JOIN_NODE_UID box original-native-uid\n"
	if code, out := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/retry", `{"password":"`+joinPassword+`"}`); code != http.StatusAccepted {
		t.Fatalf("retry = %d %v", code, out)
	}
	if j := s.wait(); j.Status.Phase != rlarkv1alpha1.ClusterProvisionSucceeded || j.Annotations[nodeJoinNodeUID] != "original-native-uid" || len(s.box.scripts("systemd-run")) != 1 {
		t.Fatalf("status %+v, the line started %d times", j.Status, len(s.box.scripts("systemd-run")))
	}
}

// A join running when the Gateway starts stopped there; the stages after install go on without the password; a join
// that is left holds its name until it is retried or cleared, one that succeeded gives it up.
func TestAnInterruptedJoinIsRetriedFromItsStageAndHoldsItsName(t *testing.T) {
	status := newJoinStatus("SHA256:box")
	status.Facts.Route = rlarkv1alpha1.NodeJoinRouteDirect
	setStage(&status.Stages, &status.Stage, stageNodeReady, rlarkv1alpha1.ClusterProvisionStageRunning, "", "")
	j := &rlarkv1alpha1.NodeJoin{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "previous-attempt", Labels: map[string]string{nodeJoinClusterLabel: "cluster-a"}},
		Spec: rlarkv1alpha1.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box", Machine: rlarkv1alpha1.NodeJoinMachine{Address: "1.2.3.4", Port: 8023, User: "root"}}, Status: status}
	s := newJoinScene(t, joinNetwork(nil, nil), j, readyNode(0))
	s.enrolled()
	if _, err := s.g.stationClient.Resource(nodeAdmissionGVR).Namespace("agent-a").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission", "metadata": map[string]interface{}{"name": "box", "namespace": "agent-a"},
		"spec": map[string]interface{}{"clusterID": "cluster-a", "publicKey": joinKey}, "status": map[string]interface{}{"phase": "Joined"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.g.InterruptNodeJoins(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped, _ := s.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(context.Background(), "cluster-a.box", metav1.GetOptions{})
	if stopped.Status.Phase != rlarkv1alpha1.ClusterProvisionFailed || !strings.Contains(joinStageOf(stopped, stageNodeReady).Message, "Gateway 重启") {
		t.Fatalf("status %+v", stopped.Status)
	}
	if code, out := s.call(http.MethodPost, "/station/node-joins", `{"cluster_id":"cluster-a","address":"1.2.3.4","port":8023,"user":"root","password":"`+joinPassword+`"}`); code != http.StatusConflict || !strings.Contains(out["error"].(string), "retry it, or clear it") {
		t.Fatalf("a new join over a stopped one = %d %v", code, out)
	}
	s.box.ran, s.box.stdin = nil, nil
	if code, out := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/retry", `{}`); code != http.StatusAccepted {
		t.Fatalf("retry = %d %v", code, out)
	}
	if j := s.wait(); j.Status.Phase != rlarkv1alpha1.ClusterProvisionSucceeded || len(s.box.ran) != 0 {
		t.Fatalf("status %+v; the machine ran %q", j.Status, s.box.ran)
	}
	// A join that succeeded gives the name up to a new one.
	s.reach["10.11.14.128:18443"] = true
	s.create()
	if j := s.wait(); j.Status.Phase != rlarkv1alpha1.ClusterProvisionSucceeded || joinStageOf(j, stagePrecheck).State != rlarkv1alpha1.ClusterProvisionStageDone {
		t.Fatalf("the new join %+v", j.Status)
	}
	if code, out := s.call(http.MethodGet, "/station/node-joins?cluster_id=cluster-a", ""); code != http.StatusOK || len(out["items"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
}

// A password the machine refuses is said at once, and nothing is recorded.
func TestAJoinWhoseLoginIsRefusedIsNotRecorded(t *testing.T) {
	s := newJoinScene(t, joinNetwork(nil, nil))
	code, out := s.call(http.MethodPost, "/station/node-joins", `{"cluster_id":"cluster-a","address":"1.2.3.4","user":"root","password":"wrong"}`)
	if code != http.StatusUnprocessableEntity || out["reason"] != nodessh.LoginRejected {
		t.Fatalf("%d %v", code, out)
	}
	if list, _ := s.g.kubeClient.RlinfV1alpha1().NodeJoins().List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("joins %d", len(list.Items))
	}
}

// A join that stopped at its tunnel is cleared: the tunnel on the machine, its declaration as a hub, the record.
func TestAJoinThatStoppedAtItsTunnelIsCleared(t *testing.T) {
	status := newJoinStatus("SHA256:box")
	status.Phase = rlarkv1alpha1.ClusterProvisionFailed
	status.Facts = rlarkv1alpha1.NodeJoinFacts{Route: rlarkv1alpha1.NodeJoinRouteTunnel, DeclaredHub: true, TunnelAddress: "10.254.240.6", PublicKey: joinKey}
	setStage(&status.Stages, &status.Stage, stageTunnel, rlarkv1alpha1.ClusterProvisionStageFailed, "", "no answer")
	j := &rlarkv1alpha1.NodeJoin{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "test-join"},
		Spec: rlarkv1alpha1.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box", Machine: rlarkv1alpha1.NodeJoinMachine{Address: "1.2.3.4", Port: 8023, User: "root"}}, Status: status}
	other := map[string]interface{}{"clusterID": "rlark-ksyun-gpu", "nodeName": "10-0-2-57", "address": "10.254.240.3"}
	s := newJoinScene(t, joinNetwork([]interface{}{other, map[string]interface{}{"clusterID": "cluster-a", "nodeName": "box", "address": "10.254.240.6", "publicKey": joinKey}}, nil), j)
	code, out := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/clear", `{"password":"`+joinPassword+`"}`)
	if code != http.StatusOK || len(out["steps"].([]any)) != 3 || len(s.box.scripts("rlark-station-gateway.service")) != 1 {
		t.Fatalf("clear = %d %v", code, out)
	}
	network, _ := s.g.provisionNetwork(context.Background())
	if nodes := network.Nodes(); len(nodes) != 1 || nodes[0].NodeName != "10-0-2-57" {
		t.Fatalf("nodes %v", nodes)
	}
	if _, err := s.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(context.Background(), "cluster-a.box", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the record: %v", err)
	}
}

// Clearing a join that succeeded takes its record only: the node stays in the cluster.
func TestClearingASucceededJoinKeepsItsNode(t *testing.T) {
	status := newJoinStatus("SHA256:box")
	status.Phase = rlarkv1alpha1.ClusterProvisionSucceeded
	for _, stage := range joinStages {
		setStage(&status.Stages, &status.Stage, stage, rlarkv1alpha1.ClusterProvisionStageDone, "", "")
	}
	j := &rlarkv1alpha1.NodeJoin{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "test-join"},
		Spec: rlarkv1alpha1.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box", Machine: rlarkv1alpha1.NodeJoinMachine{Address: "1.2.3.4", Port: 8023, User: "root"}}, Status: status}
	s := newJoinScene(t, joinNetwork(nil, nil), j)
	code, out := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/clear", `{}`)
	if code != http.StatusOK || len(out["steps"].([]any)) != 1 || out["steps"].([]any)[0] != "删掉这条 SSH 入网记录" {
		t.Fatalf("clear = %d %v", code, out)
	}
	operations, err := s.g.stationClient.Resource(nodeOperationGVR).Namespace("").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(operations.Items) != 0 {
		t.Fatalf("node operations %v %v", operations, err)
	}
	if _, err := s.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(context.Background(), "cluster-a.box", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the record: %v", err)
	}
}

func TestNodeNameOfHost(t *testing.T) {
	for host, name := range map[string]string{"NB260706069\n": "nb260706069", "cailiang-desktop": "cailiang-desktop", "Lab_PC 3": "lab-pc-3", "-x-": "x"} {
		if got := nodeNameOfHost(host); got != name {
			t.Fatalf("%q: %q", host, got)
		}
	}
}

func TestRetryRejectsUnknownJoinStage(t *testing.T) {
	s := newJoinScene(t, joinNetwork(nil, nil), &rlarkv1alpha1.NodeJoin{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box"},
		Status:     rlarkv1alpha1.NodeJoinStatus{Phase: rlarkv1alpha1.ClusterProvisionFailed, Stage: "unknown"},
	})
	if code, out := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/retry", `{"password":"fixture"}`); code != http.StatusConflict {
		t.Fatalf("retry = %d %v", code, out)
	}
}

func TestClearInterruptedJoinUsesOriginalUIDOrRefusesLegacyRecord(t *testing.T) {
	for _, uid := range []string{"original-native-uid", ""} {
		t.Run("uid="+uid, func(t *testing.T) {
			status := newJoinStatus("SHA256:box")
			status.Phase = rlarkv1alpha1.ClusterProvisionFailed
			setStage(&status.Stages, &status.Stage, stageInstall, rlarkv1alpha1.ClusterProvisionStageFailed, "", "interrupted")
			j := &rlarkv1alpha1.NodeJoin{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "join-1", Annotations: map[string]string{nodeJoinNodeUID: uid}},
				Spec:       rlarkv1alpha1.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box"},
				Status:     status,
			}
			replacement := readyNode(0)
			replacement.Status.KubernetesNodeUID = "replacement-native-uid"
			s := newJoinScene(t, joinNetwork(nil, nil), j, replacement)
			code, out := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/clear", `{}`)
			operations, err := s.g.stationClient.Resource(nodeOperationGVR).Namespace("agent-a").List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if uid == "" {
				if code != http.StatusConflict || len(operations.Items) != 0 {
					t.Fatalf("legacy clear = %d %v, operations %v", code, out, operations.Items)
				}
				return
			}
			if code != http.StatusOK || len(operations.Items) != 1 {
				t.Fatalf("clear = %d %v, operations %v", code, out, operations.Items)
			}
			got, _, _ := unstructured.NestedString(operations.Items[0].Object, "spec", "nodeUID")
			if got != uid {
				t.Fatalf("removal targeted %q, want %q", got, uid)
			}
		})
	}
}

func TestJoinUIDReceiptCannotBeReplacedOrAdoptedFromOlderAttempt(t *testing.T) {
	j := &rlarkv1alpha1.NodeJoin{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "join-1"}, Spec: rlarkv1alpha1.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box"}}
	s := newJoinScene(t, joinNetwork(nil, nil), j)
	ctx := context.Background()
	s.nativeNodeUID("different-current-uid")
	if err := s.g.recordJoinedNodeUID(ctx, j, "RLARK_SSH_JOIN join-1\nRLARK_JOIN_NODE_UID box original"); err == nil {
		t.Fatal("a receipt did not match the member API")
	}
	s.nativeNodeUID("original")
	if err := s.g.recordJoinedNodeUID(ctx, j, "RLARK_SSH_JOIN older-join\nRLARK_JOIN_NODE_UID box unknown\nRLARK_SSH_JOIN join-1\nRLARK_JOIN_NODE_UID box original"); err != nil {
		t.Fatal(err)
	}
	if err := s.g.recordJoinedNodeUID(ctx, j, "RLARK_SSH_JOIN join-1\nRLARK_JOIN_NODE_UID box replacement"); err == nil {
		t.Fatal("a second native UID must not overwrite the original")
	}
	current, err := s.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(ctx, j.Name, metav1.GetOptions{})
	if err != nil || current.Annotations[nodeJoinNodeUID] != "original" {
		t.Fatalf("join UID receipt = %v, %v", current, err)
	}
}

func TestAbandonUnverifiedJoinOnlyDeletesOwnedUnusedAdmission(t *testing.T) {
	for _, scenario := range []struct {
		name, owner, redeemed string
		remove                bool
	}{
		{name: "unused", owner: "join-1", remove: true},
		{name: "redeemed", owner: "join-1", redeemed: "2026-11-01T00:00:00Z"},
		{name: "replacement", owner: "join-2"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			status := newJoinStatus("SHA256:box")
			status.Phase = rlarkv1alpha1.ClusterProvisionFailed
			setStage(&status.Stages, &status.Stage, stageInstall, rlarkv1alpha1.ClusterProvisionStageFailed, "", "interrupted")
			j := &rlarkv1alpha1.NodeJoin{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "join-1"},
				Spec:       rlarkv1alpha1.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box"},
				Status:     status,
			}
			s := newJoinScene(t, joinNetwork(nil, nil), j, readyNode(0))
			ctx := t.Context()
			admissions := s.g.stationClient.Resource(nodeAdmissionGVR).Namespace("agent-a")
			admission := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission",
				"spec": map[string]any{"redeemedAt": scenario.redeemed},
			}}
			admission.SetName("box")
			admission.SetNamespace("agent-a")
			admission.SetUID("admission-uid")
			admission.SetResourceVersion("1")
			admission.SetAnnotations(map[string]string{stationidentity.SSHJoinUIDAnnotation: scenario.owner})
			if _, err := admissions.Create(ctx, admission, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if code, body := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/clear", `{"machine_gone":true}`); code != http.StatusOK {
				t.Fatalf("clear = %d %v", code, body)
			}
			if _, err := admissions.Get(ctx, "box", metav1.GetOptions{}); scenario.remove != apierrors.IsNotFound(err) {
				t.Fatalf("unexpected admission after clear: %v", err)
			}
			operations, err := s.g.stationClient.Resource(nodeOperationGVR).Namespace("agent-a").List(ctx, metav1.ListOptions{})
			if err != nil || len(operations.Items) != 0 {
				t.Fatalf("unknown identity triggered a native Node removal: %v %v", operations, err)
			}
		})
	}
}

func (s *joinScene) nativeNodeUID(uid string) {
	s.g.hooks.ProxyKubeRequest = func(_ context.Context, method, member, path string, _ []byte) (*http.Response, error) {
		if method != http.MethodGet || member != "cluster-a" || path != "api/v1/nodes/box" {
			return nil, fmt.Errorf("unexpected member request: %s %s %s", method, member, path)
		}
		body := fmt.Sprintf(`{"metadata":{"name":"box","uid":%q}}`, uid)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func TestClearMissingIssuedAdmissionPreservesDeclaredHubAndJoin(t *testing.T) {
	status := newJoinStatus("SHA256:box")
	status.Phase = rlarkv1alpha1.ClusterProvisionFailed
	status.Facts.DeclaredHub = true
	now := metav1.Now()
	status.Facts.AdmissionIssuedAt = &now
	setStage(&status.Stages, &status.Stage, stageInstall, rlarkv1alpha1.ClusterProvisionStageFailed, "", "interrupted")
	join := &rlarkv1alpha1.NodeJoin{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "join-1"},
		Spec:       rlarkv1alpha1.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box"},
		Status:     status,
	}
	scene := newJoinScene(t, joinNetwork(nil, nil), join)
	code, out := scene.call(http.MethodPost, "/station/node-joins/cluster-a.box/clear", `{"machine_gone":true}`)
	if code != http.StatusConflict {
		t.Fatalf("clear=%d %v", code, out)
	}
	if _, err := scene.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(t.Context(), join.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("lost original SSH join: %v", err)
	}
}

func TestHostFailureReasonIsIndependentOfDiagnosticText(t *testing.T) {
	if !hasHostReason("ssh warning\r\nreason=worker_credentials\r\nlocalized message\n", "worker_credentials") ||
		hasHostReason("the host still has Kubernetes credentials", "worker_credentials") ||
		hasHostReason("remote: reason=worker_credentials", "worker_credentials") {
		t.Fatal("host cleanup did not use an exact reason line")
	}
}

func TestClearResumesAfterAdmissionDeletionAndSiteNetworkFailure(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%t", replacement), func(t *testing.T) {
			const address = "10.254.240.6"
			status := newJoinStatus("SHA256:box")
			status.Phase = rlarkv1alpha1.ClusterProvisionFailed
			status.Facts.DeclaredHub, status.Facts.TunnelAddress, status.Facts.PublicKey = true, address, joinKey
			now := metav1.Now()
			status.Facts.AdmissionIssuedAt = &now
			setStage(&status.Stages, &status.Stage, stageInstall, rlarkv1alpha1.ClusterProvisionStageFailed, "", "interrupted")
			join := &rlarkv1alpha1.NodeJoin{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "join-1", ResourceVersion: "7"},
				Spec:       rlarkv1alpha1.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box"},
				Status:     status,
			}
			hub := map[string]any{"clusterID": "cluster-a", "nodeName": "box", "address": address, "publicKey": joinKey}
			s := newJoinScene(t, joinNetwork([]any{hub}, nil), join)
			ctx := t.Context()
			fakeStation := s.g.stationClient.(*dynamicfake.FakeDynamicClient)
			admissions := fakeStation.Resource(nodeAdmissionGVR).Namespace("agent-a")
			admission := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission",
				"spec": map[string]any{"expiresAt": now.Add(time.Hour).UTC().Format(time.RFC3339)},
			}}
			admission.SetName("box")
			admission.SetNamespace("agent-a")
			admission.SetUID("admission-uid")
			admission.SetResourceVersion("4")
			admission.SetAnnotations(map[string]string{stationidentity.SSHJoinUIDAnnotation: "join-1"})
			if _, err := admissions.Create(ctx, admission, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			fakeStation.PrependReactor("update", "nodeadmissions", func(action ktesting.Action) (bool, runtime.Object, error) {
				obj := action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured)
				obj.SetResourceVersion("5")
				return false, nil, nil
			})
			fakeStation.PrependReactor("delete", "nodeadmissions", func(action ktesting.Action) (bool, runtime.Object, error) {
				opts := action.(ktesting.DeleteAction).GetDeleteOptions()
				if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "admission-uid" ||
					opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.ResourceVersion != "5" {
					t.Fatalf("delete preconditions = %+v", opts.Preconditions)
				}
				record, err := s.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(ctx, join.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := readJoinAdmissionCleanup(record, "agent-a")
				if err != nil || receipt.AdmissionUID != "admission-uid" || receipt.AdmissionVersion != "5" {
					t.Fatalf("delete preceded the receipt: %+v, %v", receipt, err)
				}
				return false, nil, nil
			})
			outage := true
			fakeStation.PrependReactor("update", "sitenetworks", func(ktesting.Action) (bool, runtime.Object, error) {
				if outage {
					return true, nil, apierrors.NewServiceUnavailable("SiteNetwork temporarily unavailable")
				}
				return false, nil, nil
			})
			const clearURL = "/station/node-joins/cluster-a.box/clear"
			if code, out := s.call(http.MethodPost, clearURL, `{"machine_gone":true}`); code != http.StatusBadGateway {
				t.Fatalf("first clear = %d %v", code, out)
			}
			if _, err := admissions.Get(ctx, "box", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatalf("original Admission still present: %v", err)
			}
			if code, out := s.call(http.MethodPost, "/station/node-joins/cluster-a.box/retry", `{}`); code != http.StatusConflict {
				t.Fatalf("join retried during cleanup = %d %v", code, out)
			}
			outage = false
			if replacement {
				admission.SetUID("replacement-uid")
				if _, err := admissions.Create(ctx, admission, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			want := http.StatusOK
			if replacement {
				want = http.StatusConflict
			}
			if code, out := s.call(http.MethodPost, clearURL, `{"machine_gone":true}`); code != want {
				t.Fatalf("retry clear = %d %v, want %d", code, out, want)
			}
			if _, err := s.g.kubeClient.RlinfV1alpha1().NodeJoins().Get(ctx, join.Name, metav1.GetOptions{}); (replacement && err != nil) || (!replacement && !apierrors.IsNotFound(err)) {
				t.Fatalf("unexpected remaining join: %v", err)
			}
			network, err := s.g.provisionNetwork(ctx)
			if err != nil || (len(network.Entries("spec", "nodes")) == 1) != replacement {
				t.Fatalf("unexpected remaining Hub: %v, %v", network, err)
			}
		})
	}
}
