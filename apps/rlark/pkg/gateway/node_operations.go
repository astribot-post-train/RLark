package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

var nodeOperationGVR = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "nodeoperations"}

const maxNodeOperationRequestBytes = 64 << 10
const nodeOperationCredentialFingerprintAnnotation = "station.rlark.io/credential-fingerprint"
const nodeOperationCredentialNamespace = "station-operation-credentials"

var nodeOperationFingerprint = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

type nodeOperationCreateRequest struct {
	ClusterID            string `json:"cluster_id"`
	RequestID            string `json:"request_id"`
	Action               string `json:"action"`
	NodeName             string `json:"node_name"`
	NodeUID              string `json:"node_uid"`
	ResetExistingKubeadm bool   `json:"reset_existing_kubeadm,omitempty"`
	ContainerdRuntime    string `json:"containerd_runtime,omitempty"`
	SSHAddress           string `json:"ssh_address"`
	SSHPort              int32  `json:"ssh_port"`
	SSHUser              string `json:"ssh_user"`
	HostKeyFingerprint   string `json:"host_key_fingerprint"`
	SSHPassword          string `json:"ssh_password,omitempty"`
	SSHPrivateKey        string `json:"ssh_private_key,omitempty"`
	SudoPassword         string `json:"sudo_password,omitempty"`
}

func nodeOperationName(clusterID, requestID string) string {
	digest := sha256.Sum256([]byte(clusterID + "\x00" + requestID))
	return "node-op-" + hex.EncodeToString(digest[:16])
}
func nodeOperationCredentialName(name string) string {
	digest := sha256.Sum256([]byte(name))
	return "node-auth-" + hex.EncodeToString(digest[:12])
}

func (g *Gateway) handleCreateNodeOperation(c *gin.Context) {
	if g.stationClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Station API unavailable"})
		return
	}
	if g.rawClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Kubernetes credential storage unavailable"})
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node operation requires an application/json request"})
		return
	}
	var request nodeOperationCreateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxNodeOperationRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node operation request"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request must contain one JSON object"})
		return
	}
	if err := validateNodeOperationCreate(request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	actor := c.GetString(authSubjectKey)
	if actor == "" || strings.TrimSpace(actor) != actor {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated administrator identity is required"})
		return
	}
	namespace, err := g.stationNamespace(c, request.ClusterID)
	if err != nil {
		return
	}
	name := nodeOperationName(request.ClusterID, request.RequestID)
	credentialName := nodeOperationCredentialName(name)
	fingerprint, err := nodeOperationCredentialFingerprint(g.jwtSigningKey, request)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "node operation credential verification is unavailable"})
		return
	}
	resource := g.stationClient.Resource(nodeOperationGVR).Namespace(namespace)
	sshSpec := map[string]any{"address": request.SSHAddress, "port": int64(request.SSHPort), "user": request.SSHUser, "hostKeyFingerprint": request.HostKeyFingerprint}
	spec := map[string]any{"requestID": request.RequestID, "action": request.Action, "clusterID": request.ClusterID, "nodeName": request.NodeName, "requestedBy": actor}
	if request.NodeUID != "" {
		spec["nodeUID"] = request.NodeUID
	}
	if request.ResetExistingKubeadm {
		spec["resetExistingKubeadm"] = true
	}
	if request.Action == "Join" {
		runtime := request.ContainerdRuntime
		if runtime == "" {
			runtime = "containerd"
		}
		spec["containerdRuntime"] = runtime
	}
	if request.Action != "EnableWireGuard" {
		spec["ssh"] = sshSpec
		spec["credentialSecret"] = credentialName
	}
	want := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeOperation", "metadata": map[string]any{"name": name, "namespace": namespace, "annotations": map[string]any{nodeOperationCredentialFingerprintAnnotation: fingerprint}}, "spec": spec}}
	if existing, getErr := resource.Get(c.Request.Context(), name, metav1.GetOptions{}); getErr == nil {
		if !nodeOperationSpecEqual(existing.Object["spec"], spec) || existing.GetAnnotations()[nodeOperationCredentialFingerprintAnnotation] != fingerprint {
			c.JSON(http.StatusConflict, gin.H{"error": "request ID is already used for a different operation"})
			return
		}
		if existing.Object["status"] != nil {
			c.JSON(http.StatusOK, safeNodeOperation(existing))
			return
		}
		if err := g.ensureNodeOperationCredential(c, existing, credentialName, request); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "operation credential could not be stored"})
			return
		}
		c.JSON(http.StatusAccepted, safeNodeOperation(existing))
		return
	} else if !apierrors.IsNotFound(getErr) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "node operation lookup failed"})
		return
	}
	created, err := resource.Create(c.Request.Context(), want, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, readErr := resource.Get(c.Request.Context(), name, metav1.GetOptions{})
		if readErr == nil && nodeOperationSpecEqual(existing.Object["spec"], spec) && existing.GetAnnotations()[nodeOperationCredentialFingerprintAnnotation] == fingerprint {
			if request.Action == "Join" || request.Action == "Remove" {
				if credentialErr := g.ensureNodeOperationCredential(c, existing, credentialName, request); credentialErr != nil {
					c.JSON(http.StatusBadGateway, gin.H{"error": "operation credential could not be stored"})
					return
				}
			}
			c.JSON(http.StatusAccepted, safeNodeOperation(existing))
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": "node operation already exists"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "node operation could not be created"})
		return
	}
	if request.Action != "EnableWireGuard" {
		if err := g.ensureNodeOperationCredential(c, created, credentialName, request); err != nil {
			// Retain the failed operation as evidence; its controller blocks while the credential is absent.
			c.JSON(http.StatusBadGateway, gin.H{"error": "operation was recorded but credential storage failed"})
			return
		}
	}
	c.Header("Location", "/api/v1/station/node-operations/"+name+"?cluster_id="+request.ClusterID)
	c.JSON(http.StatusAccepted, safeNodeOperation(created))
}

func validateNodeOperationCreate(r nodeOperationCreateRequest) error {
	for _, value := range []string{r.ClusterID, r.RequestID, r.Action, r.NodeName, r.NodeUID, r.SSHAddress, r.SSHUser, r.HostKeyFingerprint} {
		if strings.TrimSpace(value) != value {
			return fmt.Errorf("node operation fields must not contain surrounding whitespace")
		}
	}
	if len(validation.IsDNS1123Subdomain(r.ClusterID)) != 0 || len(validation.IsDNS1123Subdomain(r.RequestID)) != 0 {
		return fmt.Errorf("cluster_id and request_id must be DNS subdomains")
	}
	if r.Action != "Join" && r.Action != "Remove" && r.Action != "EnableWireGuard" {
		return fmt.Errorf("unsupported node operation action")
	}
	if r.ContainerdRuntime != "" && r.ContainerdRuntime != "containerd" && r.ContainerdRuntime != "fleet" {
		return fmt.Errorf("containerd_runtime must be containerd or fleet")
	}
	if r.Action != "Join" && r.ContainerdRuntime != "" {
		return fmt.Errorf("containerd_runtime is only valid for Join")
	}
	if len(validation.IsDNS1123Subdomain(r.NodeName)) != 0 {
		return fmt.Errorf("node_name must be a DNS subdomain")
	}
	if r.Action == "Remove" && r.NodeUID == "" {
		return fmt.Errorf("node_uid is required for removal")
	}
	if r.Action == "Join" && r.NodeUID != "" {
		return fmt.Errorf("node_uid must be omitted when joining a new node")
	}
	if r.ResetExistingKubeadm && r.Action != "Join" {
		return fmt.Errorf("reset_existing_kubeadm is only valid for Join")
	}
	if r.Action == "EnableWireGuard" {
		return fmt.Errorf("independent WireGuard peer allocation is not available")
	}
	if r.SSHPort < 1 || r.SSHPort > 65535 || r.SSHUser == "" || len(r.SSHUser) > 128 || strings.ContainsAny(r.SSHUser, " \t\r\n\x00") || strings.TrimSpace(r.SSHAddress) != r.SSHAddress {
		return fmt.Errorf("SSH address, port and user are invalid")
	}
	if ip := net.ParseIP(r.SSHAddress); ip == nil && len(validation.IsDNS1123Subdomain(r.SSHAddress)) != 0 {
		return fmt.Errorf("ssh_address must be an IP address or DNS hostname")
	}
	if host, _, err := net.SplitHostPort(net.JoinHostPort(r.SSHAddress, strconv.Itoa(int(r.SSHPort)))); err != nil || host == "" {
		return fmt.Errorf("ssh_address must be an IP address or hostname without port")
	}
	if !nodeOperationFingerprint.MatchString(r.HostKeyFingerprint) {
		return fmt.Errorf("host_key_fingerprint must be a SHA256 SSH fingerprint")
	}
	if (r.SSHPassword == "") == (r.SSHPrivateKey == "") {
		return fmt.Errorf("provide exactly one SSH password or private key")
	}
	if len(r.SSHPassword) > 4096 || len(r.SSHPrivateKey) > 32<<10 || len(r.SudoPassword) > 4096 || strings.ContainsAny(r.SSHPassword+r.SudoPassword, "\x00") {
		return fmt.Errorf("credential exceeds supported format")
	}
	return nil
}

func nodeOperationSpecEqual(existing any, requested map[string]any) bool {
	a, _ := json.Marshal(existing)
	b, _ := json.Marshal(requested)
	return string(a) == string(b)
}

func nodeOperationCredentialFingerprint(key []byte, r nodeOperationCreateRequest) (string, error) {
	if len(key) < 32 {
		return "", fmt.Errorf("stable credential fingerprint key is unavailable")
	}
	mac := hmac.New(sha256.New, key)
	for _, field := range []struct{ name, value string }{{"ssh-password", r.SSHPassword}, {"ssh-private-key", r.SSHPrivateKey}, {"sudo-password", r.SudoPassword}} {
		_, _ = mac.Write([]byte{byte(len(field.name) >> 24), byte(len(field.name) >> 16), byte(len(field.name) >> 8), byte(len(field.name))})
		_, _ = mac.Write([]byte(field.name))
		_, _ = mac.Write([]byte{byte(len(field.value) >> 24), byte(len(field.value) >> 16), byte(len(field.value) >> 8), byte(len(field.value))})
		_, _ = mac.Write([]byte(field.value))
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func safeNodeOperation(op *unstructured.Unstructured) map[string]any {
	storedSpec, _, _ := unstructured.NestedMap(op.Object, "spec")
	spec := make(map[string]any, len(storedSpec))
	for key, value := range storedSpec {
		spec[key] = value
	}
	delete(spec, "credentialSecret")
	return map[string]any{"apiVersion": op.GetAPIVersion(), "kind": op.GetKind(), "metadata": map[string]any{"name": op.GetName(), "namespace": op.GetNamespace(), "uid": op.GetUID(), "creationTimestamp": op.GetCreationTimestamp()}, "spec": spec, "status": op.Object["status"]}
}

func (g *Gateway) ensureNodeOperationCredential(c *gin.Context, op *unstructured.Unstructured, name string, r nodeOperationCreateRequest) error {
	fingerprint, err := nodeOperationCredentialFingerprint(g.jwtSigningKey, r)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nodeOperationCredentialNamespace, Annotations: map[string]string{
		"station.rlark.io/credential-expires-at":     time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339),
		nodeOperationCredentialFingerprintAnnotation: fingerprint,
		"station.rlark.io/operation-name":            op.GetName(),
		"station.rlark.io/operation-namespace":       op.GetNamespace(),
		"station.rlark.io/operation-uid":             string(op.GetUID()),
	}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
	if r.SSHPassword != "" {
		secret.Data["ssh-password"] = []byte(r.SSHPassword)
	}
	if r.SSHPrivateKey != "" {
		secret.Data["ssh-private-key"] = []byte(r.SSHPrivateKey)
	}
	if r.SudoPassword != "" {
		secret.Data["sudo-password"] = []byte(r.SudoPassword)
	}
	_, err = g.rawClient.CoreV1().Secrets(nodeOperationCredentialNamespace).Create(c.Request.Context(), secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, readErr := g.rawClient.CoreV1().Secrets(nodeOperationCredentialNamespace).Get(c.Request.Context(), name, metav1.GetOptions{})
		if readErr != nil {
			return readErr
		}
		if !nodeOperationCredentialMatches(current, op) || current.Annotations[nodeOperationCredentialFingerprintAnnotation] != fingerprint {
			return fmt.Errorf("credential Secret owner mismatch")
		}
		return nil
	}
	return err
}

func nodeOperationCredentialMatches(secret *corev1.Secret, op *unstructured.Unstructured) bool {
	return secret != nil && op != nil && secret.Namespace == nodeOperationCredentialNamespace &&
		secret.Annotations["station.rlark.io/operation-name"] == op.GetName() &&
		secret.Annotations["station.rlark.io/operation-namespace"] == op.GetNamespace() &&
		secret.Annotations["station.rlark.io/operation-uid"] == string(op.GetUID())
}

func (g *Gateway) handleGetNodeOperation(c *gin.Context) {
	if g.stationClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Station API unavailable"})
		return
	}
	clusterID := c.Query("cluster_id")
	if clusterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id is required"})
		return
	}
	namespace, err := g.stationNamespace(c, clusterID)
	if err != nil {
		return
	}
	op, err := g.stationClient.Resource(nodeOperationGVR).Namespace(namespace).Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "node operation not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "node operation lookup failed"})
		return
	}
	id, found, nestedErr := unstructured.NestedString(op.Object, "spec", "clusterID")
	if nestedErr != nil || !found {
		c.JSON(http.StatusConflict, gin.H{"error": "operation cluster binding is invalid"})
		return
	}
	if id != clusterID {
		c.JSON(http.StatusConflict, gin.H{"error": "operation cluster binding mismatch"})
		return
	}
	c.JSON(http.StatusOK, safeNodeOperation(op))
}

func (g *Gateway) handleListNodeOperations(c *gin.Context) {
	if g.stationClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Station API unavailable"})
		return
	}
	clusterID := c.Query("cluster_id")
	if clusterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id is required"})
		return
	}
	namespace, err := g.stationNamespace(c, clusterID)
	if err != nil {
		return
	}
	limit := int64(100)
	if raw := c.Query("limit"); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 1 || parsed > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 500"})
			return
		}
		limit = parsed
	}
	items, err := g.stationClient.Resource(nodeOperationGVR).Namespace(namespace).List(c.Request.Context(), metav1.ListOptions{Limit: limit, Continue: c.Query("continue")})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "node operations lookup failed"})
		return
	}
	out := make([]map[string]any, 0, len(items.Items))
	for i := range items.Items {
		if id, found, _ := unstructured.NestedString(items.Items[i].Object, "spec", "clusterID"); !found || id != clusterID {
			continue
		}
		out = append(out, safeNodeOperation(&items.Items[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "continue": items.GetContinue()})
}
