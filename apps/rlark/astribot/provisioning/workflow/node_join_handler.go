package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rlinf/rlark/api/kubeclients/clientset/versioned"
	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
)

// nodeJoinClusterLabel holds a NodeJoin's cluster, by which a cluster's joins are listed.
const nodeJoinClusterLabel = "rlark.io/cluster"

// requestError is a request refused: its status, what it says, and for the
// console a reason it acts on and what the machine printed.
type requestError struct {
	status int
	reason string
	err    error
	output string
}

func (e *requestError) Error() string { return e.err.Error() }

func (e *requestError) respond(c *gin.Context) {
	body := gin.H{"error": e.err.Error()}
	if e.reason != "" {
		body["reason"] = e.reason
	}
	if e.output != "" {
		body["output"] = e.output
	}
	c.JSON(e.status, body)
}

type createNodeJoinRequest struct {
	ClusterID        string            `json:"cluster_id"`
	MachineName      string            `json:"machine_name,omitempty"`
	HostKey          string            `json:"host_key,omitempty"`
	HostKeyAlgorithm string            `json:"host_key_algorithm,omitempty"`
	MachineType      string            `json:"machine_type,omitempty"`
	MachineModel     string            `json:"machine_model,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	Address          string            `json:"address"`
	// Port is the machine's SSH port, 22 when it is not given.
	Port         int32  `json:"port,omitempty"`
	User         string `json:"user"`
	Password     string `json:"password"`
	SudoPassword string `json:"sudo_password,omitempty"`
	// NodeName is the node's name; empty takes the machine's host name.
	NodeName string `json:"node_name,omitempty"`
	// TunnelEndpoint is the ip:port other sites dial for the machine's tunnel, when it joins through one; empty, the
	// address at port 51820, which must then be an IPv4 address.
	TunnelEndpoint string `json:"tunnel_endpoint,omitempty"`
}

// nodeJoinRequest retries a join from the stage it stopped at, or clears it. User, when given, is the account to log
// in with from now on; MachineGone (clear) says the machine is not there to be put back.
type nodeJoinRequest struct {
	Password     string `json:"password"`
	SudoPassword string `json:"sudo_password,omitempty"`
	User         string `json:"user"`
	MachineGone  bool   `json:"machine_gone"`
}

// HandleCreateNodeJoin answers POST /api/v1/station/node-joins: it logs in to the machine once, at once, so that a
// password the machine refuses is said on the form and the node's name is known; then it records the join and runs
// its stages in the background, from the login it made.
func (g *Service) HandleCreateNodeJoin(c *gin.Context) {
	var request createNodeJoinRequest
	if !g.http.DecodeOneJSON(c, maxNodeAdmissionRequestBytes, &request, false) {
		return
	}
	portProvided := request.Port != 0
	password := []byte(request.Password)
	sudoPassword := []byte(request.SudoPassword)
	request.Password = ""
	request.SudoPassword = ""
	if request.Port == 0 {
		request.Port = 22
	}
	request.TunnelEndpoint = strings.TrimSpace(request.TunnelEndpoint)
	if request.ClusterID == "" || request.Address == "" || net.ParseIP(request.Address) == nil && len(validation.IsDNS1123Subdomain(request.Address)) > 0 ||
		request.Port < 1 || request.Port > 65535 || request.User == "" || len(request.User) > 64 || strings.ContainsAny(request.User, " \t\r\n\x00'\"\\$`") ||
		len(password) == 0 || len(password) > 4096 || len(sudoPassword) > 4096 || request.TunnelEndpoint != "" && !provisioning.ValidTunnelEndpoint(request.TunnelEndpoint) {
		clear(password)
		clear(sudoPassword)
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id, address (an IP address or host name), port, user and password are required; tunnel_endpoint is an IPv4 ip:port"})
		return
	}
	namespace, err := g.http.StationNamespace(c, request.ClusterID)
	if err != nil {
		clear(password)
		clear(sudoPassword)
		return
	}
	var candidate *unstructured.Unstructured
	if request.MachineName != "" {
		candidate, err = g.stationClient.Resource(machineGVR).Namespace(machineNamespace).Get(c.Request.Context(), request.MachineName, metav1.GetOptions{})
		if err != nil {
			clear(password)
			clear(sudoPassword)
			c.JSON(http.StatusBadRequest, gin.H{"error": "scanned Machine is unavailable"})
			return
		}
		candidateAddress, _, _ := unstructured.NestedString(candidate.Object, "status", "entry", "address")
		candidatePort, _, _ := unstructured.NestedInt64(candidate.Object, "status", "entry", "port")
		candidateKey, _, _ := unstructured.NestedString(candidate.Object, "status", "entry", "hostKeyFingerprint")
		candidateAlgorithm, _, _ := unstructured.NestedString(candidate.Object, "status", "entry", "hostKeyAlgorithm")
		candidatePhase, _, _ := unstructured.NestedString(candidate.Object, "status", "phase")
		candidateCluster, _, _ := unstructured.NestedString(candidate.Object, "status", "clusterID")
		candidateNode, _, _ := unstructured.NestedString(candidate.Object, "status", "nodeName")
		if candidatePort == 0 {
			candidatePort = 22
		}
		if candidatePhase == "Joined" || candidateCluster != "" || candidateNode != "" {
			clear(password)
			clear(sudoPassword)
			c.JSON(http.StatusConflict, gin.H{"error": "scanned Machine is already a joined node"})
			return
		}
		if (request.MachineType != "" && request.MachineType != "Server") || candidateAddress == "" || candidateKey == "" || candidateAlgorithm == "" ||
			(request.Address != "" && request.Address != candidateAddress) || (portProvided && int64(request.Port) != candidatePort) ||
			(request.HostKey != "" && request.HostKey != candidateKey) || (request.HostKeyAlgorithm != "" && request.HostKeyAlgorithm != candidateAlgorithm) {
			clear(password)
			clear(sudoPassword)
			c.JSON(http.StatusConflict, gin.H{"error": "scanned candidate identity changed; refresh discovery and review the host key"})
			return
		}
		request.Address, request.Port, request.HostKey, request.HostKeyAlgorithm, request.MachineType = candidateAddress, int32(candidatePort), candidateKey, candidateAlgorithm, "Server"
		if _, present, _ := unstructured.NestedMap(candidate.Object, "spec", "admit"); present {
			clear(password)
			clear(sudoPassword)
			c.JSON(http.StatusConflict, gin.H{"error": "scanned Machine already has an admission"})
			return
		}
	} else if request.MachineType != "" && request.MachineType != "Server" {
		clear(password)
		clear(sudoPassword)
		c.JSON(http.StatusBadRequest, gin.H{"error": "NodeJoin only accepts Server machines"})
		return
	}
	if request.MachineType == "" {
		request.MachineType = "Server"
	}
	for key, value := range request.Labels {
		if len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			clear(password)
			clear(sudoPassword)
			c.JSON(http.StatusBadRequest, gin.H{"error": "labels must be valid Kubernetes labels"})
			return
		}
	}
	ctx := c.Request.Context()
	login, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var machine provisioning.Machine
	var hostKey string
	if candidate != nil {
		annotations, annotationErr := nodeJoinAnnotations(request, request.HostKey)
		if annotationErr != nil {
			clear(password)
			clear(sudoPassword)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid NodeJoin metadata"})
			return
		}
		probe := &rlarkv1alpha1.NodeJoin{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}, Spec: rlarkv1alpha1.NodeJoinSpec{Machine: rlarkv1alpha1.NodeJoinMachine{Address: request.Address, Port: request.Port, User: request.User}}}
		machine, hostKey, err = g.dialNodeJoinMachine(login, probe, password, sudoPassword)
	} else {
		machine, hostKey, err = g.provisionDial(login, rlarkv1alpha1.ClusterProvisionMachine{Address: request.Address, Port: request.Port, User: request.User}, password, "")
	}
	if err != nil {
		clear(password)
		clear(sudoPassword)
		var failure *nodessh.Error
		if errors.As(err, &failure) {
			loginRefused(failure).respond(c)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	started := false
	defer func() {
		if !started {
			_ = machine.Close()
			clear(password)
			clear(sudoPassword)
		}
	}()
	if request.NodeName == "" {
		name, err := machine.Run(login, "hostname", nil, nil)
		if request.NodeName = nodeNameOfHost(name); err != nil || request.NodeName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("the machine's host name %q is no node name; give one", strings.TrimSpace(name)), "reason": "NodeName"})
			return
		}
	}
	name := NodeJoinName(request.ClusterID, request.NodeName)
	if len(validation.IsDNS1123Subdomain(request.NodeName)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_name must be a DNS subdomain", "reason": "NodeName"})
		return
	}
	client := g.kubeClient.RlinfV1alpha1().NodeJoins()
	joins, err := client.List(ctx, metav1.ListOptions{})
	if err != nil {
		g.http.RespondKubeError(c, "list node joins: "+err.Error(), err)
		return
	}
	for _, other := range joins.Items {
		switch {
		case other.Name == name && other.UID != "" && other.Status.Phase == rlarkv1alpha1.ClusterProvisionSucceeded:
			if err := client.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &other.UID, ResourceVersion: &other.ResourceVersion}}); err != nil && !apierrors.IsNotFound(err) {
				g.http.RespondKubeError(c, "delete the earlier join: "+err.Error(), err)
				return
			}
		case other.Name == name:
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("an earlier join of %s is left (%s, at stage %s): retry it, or clear it and start over", request.NodeName, other.Status.Phase, other.Status.Stage)})
			return
		case other.Status.Phase == rlarkv1alpha1.ClusterProvisionRunning && other.Spec.Machine.Address == request.Address && other.Spec.Machine.Port == request.Port:
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("the join %s runs on %s:%d now", other.Name, request.Address, request.Port)})
			return
		}
	}
	var machineClaim string
	if candidate != nil {
		resource := g.stationClient.Resource(machineGVR).Namespace(machineNamespace)
		latest, err := resource.Get(ctx, candidate.GetName(), metav1.GetOptions{})
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "scanned Machine changed before its admission was recorded; refresh and retry"})
			return
		}
		latestAddress, _, _ := unstructured.NestedString(latest.Object, "status", "entry", "address")
		latestPort, _, _ := unstructured.NestedInt64(latest.Object, "status", "entry", "port")
		latestKey, _, _ := unstructured.NestedString(latest.Object, "status", "entry", "hostKeyFingerprint")
		latestAlgorithm, _, _ := unstructured.NestedString(latest.Object, "status", "entry", "hostKeyAlgorithm")
		latestSite, _, _ := unstructured.NestedString(latest.Object, "spec", "site")
		initialSite, _, _ := unstructured.NestedString(candidate.Object, "spec", "site")
		if latestPort == 0 {
			latestPort = 22
		}
		_, alreadyAdmitted, _ := unstructured.NestedMap(latest.Object, "spec", "admit")
		if latest.GetUID() != candidate.GetUID() || latestAddress != request.Address || latestPort != int64(request.Port) || latestKey != request.HostKey || latestAlgorithm != request.HostKeyAlgorithm || latestSite != initialSite || alreadyAdmitted {
			c.JSON(http.StatusConflict, gin.H{"error": "scanned candidate identity changed; refresh discovery and review before joining"})
			return
		}
		machineClaim = uuid.NewString()
		patch, _ := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": latest.GetResourceVersion(), "annotations": map[string]string{nodeJoinMachineClaim: machineClaim}},
			"spec":     map[string]interface{}{"admit": machineAdmissionForServer(request, g.http.Actor(c), time.Now().UTC().Format(time.RFC3339))},
		})
		if _, err := resource.Patch(ctx, latest.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "scanned Machine changed before its admission was recorded; refresh and retry"})
			return
		}
	}
	annotations, annotationErr := nodeJoinAnnotations(request, hostKey)
	if annotationErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid NodeJoin metadata"})
		return
	}
	labels := map[string]string{nodeJoinClusterLabel: request.ClusterID}
	if candidate != nil {
		labels[nodeJoinMachineAnnotation] = candidate.GetName()
		annotations[nodeJoinMachineClaim] = machineClaim
		annotations[nodeJoinMachineUID] = string(candidate.GetUID())
	}
	j, err := client.Create(ctx, &rlarkv1alpha1.NodeJoin{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
		Spec: rlarkv1alpha1.NodeJoinSpec{
			ClusterID: request.ClusterID, NodeName: request.NodeName, RequestedBy: g.http.Actor(c),
			Machine: rlarkv1alpha1.NodeJoinMachine{Address: request.Address, Port: request.Port, User: request.User, TunnelEndpoint: request.TunnelEndpoint},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		if candidate != nil {
			_ = g.clearMachineAdmission(ctx, candidate.GetName(), request.ClusterID, g.http.Actor(c), candidate.GetUID(), machineClaim)
		}
		g.http.RespondKubeError(c, "create node join: "+err.Error(), err)
		return
	}
	j.Status = newJoinStatus(hostKey)
	written, err := client.UpdateStatus(ctx, j, metav1.UpdateOptions{})
	if err != nil {
		if candidate != nil {
			_ = g.clearMachineAdmission(ctx, candidate.GetName(), request.ClusterID, g.http.Actor(c), candidate.GetUID(), machineClaim)
		}
		if j.UID != "" {
			_ = client.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &j.UID, ResourceVersion: &j.ResourceVersion}})
		}
		g.http.RespondKubeError(c, "write the node join's status: "+err.Error(), err)
		return
	}
	j = written
	started = g.startNodeJoin(ctx, &nodeJoinRun{g: g, name: name, namespace: namespace, password: password, sudoPassword: sudoPassword, machine: machine}, joinStages[0])
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"data": j})
}

// startNodeJoin runs the join in the background, from stage on; false when it already runs.
func (g *Service) startNodeJoin(ctx context.Context, run *nodeJoinRun, stage string) bool {
	if !g.nodeJoins.start(run.name) {
		return false
	}
	go run.run(context.WithoutCancel(ctx), stage)
	return true
}

// HandleListNodeJoins answers GET /api/v1/station/node-joins?cluster_id=: the cluster's joins, running ones first,
// then the newest.
func (g *Service) HandleListNodeJoins(c *gin.Context) {
	clusterID := c.Query("cluster_id")
	if clusterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id is required"})
		return
	}
	items, err := NodeJoinsOf(c.Request.Context(), g.kubeClient, clusterID)
	if err != nil {
		g.http.RespondKubeError(c, "list node joins: "+err.Error(), err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// NodeJoinsOf lists a cluster's joins, running ones first and then the newest, and deletes those that succeeded more
// than joinKeptFor ago.
func NodeJoinsOf(ctx context.Context, kubeClient versioned.Interface, clusterID string) ([]rlarkv1alpha1.NodeJoin, error) {
	client := kubeClient.RlinfV1alpha1().NodeJoins()
	list, err := client.List(ctx, metav1.ListOptions{LabelSelector: nodeJoinClusterLabel + "=" + clusterID})
	if err != nil {
		return nil, err
	}
	items := make([]rlarkv1alpha1.NodeJoin, 0, len(list.Items))
	for _, j := range list.Items {
		// A join that succeeded is listed for joinKeptFor after its last stage.
		if j.UID != "" && j.Status.Phase == rlarkv1alpha1.ClusterProvisionSucceeded && joinExpired(j.Status.Stages) {
			options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &j.UID, ResourceVersion: &j.ResourceVersion}}
			if err := client.Delete(ctx, j.Name, options); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
			continue
		}
		items = append(items, j)
	}
	slices.SortStableFunc(items, func(a, b rlarkv1alpha1.NodeJoin) int {
		if running := (a.Status.Phase == rlarkv1alpha1.ClusterProvisionRunning); running != (b.Status.Phase == rlarkv1alpha1.ClusterProvisionRunning) {
			if running {
				return -1
			}
			return 1
		}
		return b.CreationTimestamp.Compare(a.CreationTimestamp.Time)
	})
	return items, nil
}

func joinExpired(stages []rlarkv1alpha1.ClusterProvisionStage) bool {
	if len(stages) == 0 {
		return false
	}
	finished := stages[len(stages)-1].FinishedAt
	return finished != nil && !finished.IsZero() && time.Since(finished.Time) > joinKeptFor
}

func (g *Service) HandleGetNodeJoin(c *gin.Context) {
	j, err := g.kubeClient.RlinfV1alpha1().NodeJoins().Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if err != nil {
		g.http.RespondKubeError(c, "get node join: "+err.Error(), err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": j})
}

// HandleRetryNodeJoin answers POST /api/v1/station/node-joins/:name/retry: a join that stopped goes on from its stage.
func (g *Service) HandleRetryNodeJoin(c *gin.Context) {
	var request nodeJoinRequest
	if !g.http.DecodeOneJSON(c, maxNodeAdmissionRequestBytes, &request, false) {
		return
	}
	password := []byte(request.Password)
	sudoPassword := []byte(request.SudoPassword)
	transferred := false
	defer func() {
		if !transferred {
			clear(password)
			clear(sudoPassword)
		}
	}()
	request.Password = ""
	request.SudoPassword = ""
	ctx := c.Request.Context()
	client := g.kubeClient.RlinfV1alpha1().NodeJoins()
	j, err := client.Get(ctx, c.Param("name"), metav1.GetOptions{})
	if err != nil {
		g.http.RespondKubeError(c, "get node join: "+err.Error(), err)
		return
	}
	if j.Status.Phase != rlarkv1alpha1.ClusterProvisionFailed {
		c.JSON(http.StatusConflict, gin.H{"error": "the join of " + j.Spec.NodeName + " is " + string(j.Status.Phase) + ": only a stopped one is retried"})
		return
	}
	if !slices.Contains(joinStages, j.Status.Stage) {
		c.JSON(http.StatusConflict, gin.H{"error": "the recorded node join stage is invalid"})
		return
	}
	if joinNeedsMachine(j.Status.Stage) && len(password) == 0 {
		clear(password)
		clear(sudoPassword)
		c.JSON(http.StatusBadRequest, gin.H{"error": "stage " + j.Status.Stage + " logs in to the machine: its SSH password is required"})
		return
	}
	namespace, err := g.http.StationNamespace(c, j.Spec.ClusterID)
	if err != nil {
		clear(password)
		clear(sudoPassword)
		return
	}
	if user := strings.TrimSpace(request.User); user != "" && user != j.Spec.Machine.User {
		j.Spec.Machine.User = user
		if j, err = client.Update(ctx, j, metav1.UpdateOptions{}); err != nil {
			clear(password)
			clear(sudoPassword)
			g.http.RespondKubeError(c, "change the machine's account: "+err.Error(), err)
			return
		}
	}
	run := &nodeJoinRun{g: g, name: j.Name, namespace: namespace}
	run.password, run.sudoPassword = password, sudoPassword
	if !g.startNodeJoin(ctx, run, j.Status.Stage) {
		c.JSON(http.StatusConflict, gin.H{"error": "the join of " + j.Spec.NodeName + " is running"})
		return
	}
	transferred = true
	c.JSON(http.StatusAccepted, gin.H{"data": j})
}

// clearTunnelScript takes away the tunnel a join started on the machine; its key and the installer stay, so that a
// join that starts over finds the same machine.
const joinTunnelHostMarker = "/var/lib/rlark/node-join-tunnel.uid"

const clearTunnelScript = `set -eu
marker=` + joinTunnelHostMarker + "\n" + verifyHostMarker + `systemctl disable --now rlark-station-gateway >/dev/null 2>&1 || true
rm -f /etc/systemd/system/rlark-station-gateway.service /etc/rlark/station/bootstrap.json /var/lib/station/wg/last-applied.json
ip link del st-wg0 2>/dev/null || true
systemctl daemon-reload
echo cleared=yes
` + finishHostCleanup

// HandleClearNodeJoin clears owned tunnel state before installation, or
// submits a UID-bound removal for an interrupted installation. Successful
// joins can be removed from history without removing their nodes.
func (g *Service) HandleClearNodeJoin(c *gin.Context) {
	var request nodeJoinRequest
	if !g.http.DecodeOneJSON(c, maxNodeAdmissionRequestBytes, &request, true) {
		return
	}
	password := []byte(request.Password)
	sudoPassword := []byte(request.SudoPassword)
	request.Password = ""
	request.SudoPassword = ""
	defer clear(password)
	defer clear(sudoPassword)
	ctx := c.Request.Context()
	client := g.kubeClient.RlinfV1alpha1().NodeJoins()
	j, err := client.Get(ctx, c.Param("name"), metav1.GetOptions{})
	if err != nil {
		g.http.RespondKubeError(c, "get node join: "+err.Error(), err)
		return
	}
	namespace, err := g.http.StationNamespace(c, j.Spec.ClusterID)
	if err != nil {
		return
	}
	reached := func(stage string) bool {
		return slices.ContainsFunc(j.Status.Stages, func(s rlarkv1alpha1.ClusterProvisionStage) bool {
			return s.Name == stage && s.State != rlarkv1alpha1.ClusterProvisionStagePending && s.State != rlarkv1alpha1.ClusterProvisionStageSkipped
		})
	}
	if j.Status.Phase == rlarkv1alpha1.ClusterProvisionRunning || !g.nodeJoins.start(j.Name) {
		c.JSON(http.StatusConflict, gin.H{"error": "the join of " + j.Spec.NodeName + " is running"})
		return
	}
	defer g.nodeJoins.end(j.Name)
	if user := strings.TrimSpace(request.User); user != "" {
		j.Spec.Machine.User = user
	}
	installReached := reached(stageInstall) || slices.Index(joinStages, j.Status.Stage) >= slices.Index(joinStages, stageInstall)
	if j.UID == "" || j.Status.Phase != rlarkv1alpha1.ClusterProvisionSucceeded && installReached && j.Annotations[nodeJoinNodeUID] == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "the SSH join has no verified original UID for cleanup"})
		return
	}
	// A join that succeeded leaves its node in the cluster: only its record goes (a node is removed on its own page).
	var steps []string
	switch {
	case j.Status.Phase == rlarkv1alpha1.ClusterProvisionSucceeded:
	case installReached:
		steps, err = g.removeJoinedNode(ctx, namespace, j, g.http.Actor(c))
	default:
		steps, err = g.clearUnredeemedJoin(ctx, j, password, sudoPassword)
	}
	if err == nil {
		if err = g.clearNodeJoinMachineAdmission(ctx, j); err == nil && j.Annotations[nodeJoinMachineClaim] != "" {
			steps = append(steps, "cleared the scanned Machine admission")
		}
	}
	if err == nil {
		if err = client.Delete(ctx, j.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &j.UID, ResourceVersion: &j.ResourceVersion}}); err == nil || apierrors.IsNotFound(err) {
			err = nil
			steps = append(steps, "删掉这条 SSH 入网记录")
		}
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "steps": steps})
		return
	}
	c.JSON(http.StatusOK, gin.H{"steps": steps})
}

// clearUnredeemedJoin takes away what a join left before its code's line ran: the tunnel on the machine, when the
// password is given, and its declaration of the machine as a hub.
func (g *Service) clearUnredeemedJoin(ctx context.Context, j *rlarkv1alpha1.NodeJoin, password, sudoPassword []byte) ([]string, error) {
	var steps []string
	machineTouched := slices.ContainsFunc(j.Status.Stages, func(s rlarkv1alpha1.ClusterProvisionStage) bool {
		return s.Name == stageTunnel && (s.State == rlarkv1alpha1.ClusterProvisionStageDone || s.State == rlarkv1alpha1.ClusterProvisionStageFailed || s.State == rlarkv1alpha1.ClusterProvisionStageRunning)
	})
	switch {
	case machineTouched && len(password) > 0:
		machine, _, err := g.dialNodeJoinMachine(ctx, j, password, sudoPassword)
		if err != nil {
			return steps, fmt.Errorf("登录 %s@%s：%w", j.Spec.Machine.User, j.Spec.Machine.Address, err)
		}
		_, err = machine.Run(ctx, "owner="+nodessh.Quote(string(j.UID))+"\n"+clearTunnelScript, nil, nil)
		_ = machine.Close()
		if err != nil {
			return steps, fmt.Errorf("撤掉机器上的隧道：%w", err)
		}
		steps = append(steps, "机器 "+j.Spec.Machine.Address+"：撤掉这次建的隧道；隧道密钥和安装包留在机器上")
	case machineTouched:
		steps = append(steps, "机器 "+j.Spec.Machine.Address+" 没动（没给口令或已不在）：它上面的隧道服务还在")
	}
	if !j.Status.Facts.DeclaredHub {
		return steps, nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		network, err := g.provisionNetwork(ctx)
		if err != nil {
			return err
		}
		nodes, _, _ := unstructured.NestedSlice(network.Object.Object, "spec", "nodes")
		nodes = slices.DeleteFunc(nodes, func(raw interface{}) bool {
			entry, _ := raw.(map[string]interface{})
			return j.Status.Facts.PublicKey != "" && j.Status.Facts.TunnelAddress != "" && entry["clusterID"] == j.Spec.ClusterID &&
				entry["nodeName"] == j.Spec.NodeName && entry["publicKey"] == j.Status.Facts.PublicKey && entry["address"] == j.Status.Facts.TunnelAddress
		})
		if err := unstructured.SetNestedSlice(network.Object.Object, nodes, "spec", "nodes"); err != nil {
			return err
		}
		_, err = g.stationClient.Resource(siteNetworkGVR).Namespace("station-system").Update(ctx, network.Object, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return steps, fmt.Errorf("从 SiteNetwork 里撤掉这台机器的 Hub 声明：%w", err)
	}
	return append(steps, fmt.Sprintf("SiteNetwork：撤掉 %s 的 Hub 声明（隧道地址 %s 放出）", j.Spec.NodeName, j.Status.Facts.TunnelAddress)), nil
}

// removeJoinedNode submits a repeatable removal of the native Node recorded by the installer.
func (g *Service) removeJoinedNode(ctx context.Context, namespace string, j *rlarkv1alpha1.NodeJoin, actor string) ([]string, error) {
	uid := j.Annotations[nodeJoinNodeUID]
	if j.UID == "" || uid == "" {
		return nil, fmt.Errorf("the SSH join has no verified original Node UID")
	}
	requestID := "clear-" + string(j.UID)
	name := stationidentity.NodeOperationName(j.Spec.ClusterID, requestID)
	spec := map[string]any{"requestID": requestID, "action": "Remove", "clusterID": j.Spec.ClusterID, "nodeName": j.Spec.NodeName, "nodeUID": uid, "requestedBy": actor}
	operation := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeOperation",
		"metadata": map[string]any{"name": name, "namespace": namespace}, "spec": spec}}
	operation.SetAnnotations(map[string]string{stationidentity.SSHJoinUIDAnnotation: string(j.UID)})
	resource := g.stationClient.Resource(nodeOperationGVR).Namespace(namespace)
	if _, err := resource.Create(ctx, operation, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		current, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.GetDeletionTimestamp() != nil || current.GetAnnotations()[stationidentity.SSHJoinUIDAnnotation] != string(j.UID) {
			return nil, fmt.Errorf("the existing removal does not belong to this SSH join")
		}
		for _, field := range []string{"requestID", "action", "clusterID", "nodeName", "nodeUID"} {
			value, _, _ := unstructured.NestedString(current.Object, "spec", field)
			if value != spec[field] {
				return nil, fmt.Errorf("the existing removal has a different %s", field)
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("提交删除节点 %s：%w", j.Spec.NodeName, err)
	}
	return []string{fmt.Sprintf("提交删除节点 %s（UID %s，操作 %s）", j.Spec.NodeName, uid, name)}, nil
}
