package stationidentity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var nodeAdmissionGVR = schema.GroupVersionResource{Group: stationObservationGroup, Version: stationObservationVersion, Resource: "nodeadmissions"}
var siteNetworkGVR = schema.GroupVersionResource{Group: stationObservationGroup, Version: stationObservationVersion, Resource: "sitenetworks"}
var addonGVR = schema.GroupVersionResource{Group: "rlinf.io", Version: "v1alpha1", Resource: "addons"}
var rlarkNodeGVR = schema.GroupVersionResource{Group: "rlinf.io", Version: "v1alpha1", Resource: "nodes"}

// Bootstrap serves a machine that is not a node yet. Its only credential is
// the one-time code of a NodeAdmission: the code buys the installer, the
// machine's tunnel configuration and a short-lived token to join its cluster.
type Bootstrap struct {
	Management dynamic.Interface
	// Member returns a client of the registered cluster that may create a
	// bootstrap token in kube-system and read kube-public/cluster-info.
	Member func(ctx context.Context, clusterID string) (kubernetes.Interface, error)
	// CA is the PEM certificate the machine verifies this API with.
	CA []byte
	// ImageFile is the installer image as a docker archive; ImageRef its name.
	ImageFile, ImageRef string
	// AddonsDir holds what the addons publish, one directory each. A site's
	// leafUplink.setup names a file in it.
	AddonsDir string
	Now       func() time.Time

	mu       sync.Mutex
	failures []time.Time
}

type redeemRequest struct {
	Code      string `json:"code"`
	PublicKey string `json:"public_key"`
}

type BootstrapJoin struct {
	APIServer  string `json:"api_server"`
	Token      string `json:"token"`
	CACertHash string `json:"ca_cert_hash"`
}

type BootstrapAnswer struct {
	State         string          `json:"state"`
	Reason        string          `json:"reason,omitempty"`
	ClusterID     string          `json:"cluster_id,omitempty"`
	NodeName      string          `json:"node_name,omitempty"`
	TunnelAddress string          `json:"tunnel_address,omitempty"`
	Config        json.RawMessage `json:"config,omitempty"`
	Join          *BootstrapJoin  `json:"join,omitempty"`
	Labels        string          `json:"labels,omitempty"`
	Taints        string          `json:"taints,omitempty"`
}

func (b *Bootstrap) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Bootstrap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/b/") && strings.HasSuffix(r.URL.Path, "/uplink"):
		b.uplink(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/b/"), "/uplink"))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/b/"):
		b.script(w, r, strings.TrimPrefix(r.URL.Path, "/b/"))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/bootstrap/ca":
		// Public: an operator's command fetches it without verifying the
		// connection, compares it with the digest the command holds, and
		// verifies every connection after that with it. The CA outlives the
		// certificates it signs, so a command stays good when this API's
		// certificate is renewed.
		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write(b.CA)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/bootstrap/image":
		// The archive is hundreds of megabytes; the server's write timeout is for API calls.
		_ = http.NewResponseController(w).SetWriteDeadline(b.now().Add(30 * time.Minute))
		w.Header().Set("Content-Type", "application/x-tar")
		http.ServeFile(w, r, b.ImageFile)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/bootstrap/redeem":
		b.redeem(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// script is what the operator's one command pipes into a shell: it fetches
// the installer over a connection verified with the CA it carries and runs it.
func (b *Bootstrap) script(w http.ResponseWriter, r *http.Request, code string) {
	if !validCode(code) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	fmt.Fprintf(w, `#!/bin/sh
# RLark node bootstrap. Loads the installer image and runs it once.
set -eu
[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
ca=$(mktemp)
cat > "$ca" <<'RLARK_CA'
%sRLARK_CA
echo "$(date +%%T) loading the installer"
curl -fsS --cacert "$ca" https://%s/v1/bootstrap/image | docker load
exec docker run --rm --privileged --net=host --pid=host --name rlark-node-bootstrap \
  -v "$ca":/ca.crt:ro -e RLARK_SERVER=https://%s -e RLARK_CODE=%s %s
`, b.CA, r.Host, r.Host, code, b.ImageRef)
}

// uplink serves the device a new leaf stands behind (a robot's router): until
// that device lets the leaf out, the leaf reaches nothing, this API included,
// so the device fetches what makes it do so, with the leaf's code. The file
// is the one the site declares (leafUplink.setup), published by an addon;
// this API does not know what is in it. The operator's command holds its
// digest and runs it only if it is that file.
func (b *Bootstrap) uplink(w http.ResponseWriter, r *http.Request, code string) {
	if b.throttled(false) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many wrong codes")
		return
	}
	setup, known, err := b.UplinkSetup(r.Context(), code)
	if err != nil {
		// The reason stays here: the caller is a machine nobody knows yet.
		log.Printf("bootstrap uplink: %v", err)
		writeError(w, http.StatusServiceUnavailable, "bootstrap storage unavailable")
		return
	}
	if setup == nil {
		if !known {
			b.throttled(true)
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	_, _ = w.Write(setup)
}

// UplinkSetup returns the uplink setup of the site of the machine the code
// admits, while the code can still be redeemed: once the machine has joined
// with it, the code opens nothing. known says whether the code is one.
func (b *Bootstrap) UplinkSetup(ctx context.Context, code string) (setup []byte, known bool, err error) {
	if !validCode(code) {
		return nil, false, nil
	}
	admission, err := b.admission(ctx, code)
	if err != nil || admission == nil {
		return nil, false, err
	}
	expires, _, _ := unstructured.NestedString(admission.Object, "spec", "expiresAt")
	redeemedBy, _, _ := unstructured.NestedString(admission.Object, "spec", "publicKey")
	if at, err := time.Parse(time.RFC3339, expires); err != nil || !b.now().Before(at) || redeemedBy != "" {
		return nil, true, nil
	}
	clusterID, _, _ := unstructured.NestedString(admission.Object, "spec", "clusterID")
	network, err := b.Management.Resource(siteNetworkGVR).Namespace("station-system").Get(ctx, "default", metav1.GetOptions{})
	if err != nil {
		return nil, true, fmt.Errorf("read the SiteNetwork: %w", err)
	}
	// The machine's site: the one the SiteNetwork's entry for the node
	// names, or its admission, or else its cluster's.
	in, _, _ := unstructured.NestedString(admission.Object, "spec", "site")
	nodes, _, _ := unstructured.NestedSlice(network.Object, "spec", "nodes")
	for _, node := range nodes {
		node, _ := node.(map[string]interface{})
		if node["clusterID"] == clusterID && node["nodeName"] == admission.GetName() && node["site"] != nil {
			in, _ = node["site"].(string)
		}
	}
	sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
	for _, site := range sites {
		site, _ := site.(map[string]interface{})
		clusters, _, _ := unstructured.NestedStringSlice(site, "clusters")
		addon, _, _ := unstructured.NestedString(site, "leafUplink", "setup", "addon")
		path, _, _ := unstructured.NestedString(site, "leafUplink", "setup", "path")
		if in != "" && site["name"] != in || in == "" && !slices.Contains(clusters, clusterID) || addon == "" || path == "" {
			continue
		}
		// Within the directory, whatever the declaration says.
		root, err := os.OpenRoot(b.AddonsDir)
		if err != nil {
			return nil, true, fmt.Errorf("open the addons directory: %w", err)
		}
		defer root.Close()
		setup, err := root.ReadFile(filepath.Join(addon, path))
		if err != nil {
			return nil, true, fmt.Errorf("read the uplink setup %s of addon %s: %w", path, addon, err)
		}
		return setup, true, nil
	}
	return nil, true, nil
}

// admission returns the NodeAdmission the code belongs to, nil if none.
func (b *Bootstrap) admission(ctx context.Context, code string) (*unstructured.Unstructured, error) {
	digest := sha256.Sum256([]byte(code))
	hash := hex.EncodeToString(digest[:])
	list, err := b.Management.Resource(nodeAdmissionGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list NodeAdmissions: %w", err)
	}
	var admission *unstructured.Unstructured
	for i := range list.Items {
		stored, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "codeHash")
		if subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) == 1 {
			admission = &list.Items[i]
		}
	}
	return admission, nil
}

func validCode(code string) bool {
	if len(code) < 8 || len(code) > 32 {
		return false
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// throttled keeps guessing a code slow: ten wrong codes a minute, for everyone.
func (b *Bootstrap) throttled(failed bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := b.now().Add(-time.Minute)
	for len(b.failures) > 0 && b.failures[0].Before(cutoff) {
		b.failures = b.failures[1:]
	}
	if failed {
		b.failures = append(b.failures, b.now())
	}
	return len(b.failures) > 10
}

func (b *Bootstrap) redeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if key, err := base64.StdEncoding.DecodeString(req.PublicKey); err != nil || len(key) != 32 || !validCode(req.Code) {
		writeError(w, http.StatusBadRequest, "code and a WireGuard public_key are required")
		return
	}
	if b.throttled(false) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many wrong codes")
		return
	}
	answer, status, err := b.Redeem(r.Context(), req.Code, req.PublicKey)
	if err != nil {
		log.Printf("bootstrap redeem: %v", err)
		writeError(w, http.StatusServiceUnavailable, "bootstrap storage unavailable")
		return
	}
	if status == http.StatusNotFound {
		b.throttled(true)
	}
	writeJSON(w, status, answer)
}

// Redeem binds the code to the machine's key, and once station-controller has
// computed the machine's tunnel returns it with a fresh join token.
func (b *Bootstrap) Redeem(ctx context.Context, code, publicKey string) (BootstrapAnswer, int, error) {
	admission, err := b.admission(ctx, code)
	if err != nil {
		return BootstrapAnswer{}, 0, err
	}
	if admission == nil {
		return BootstrapAnswer{State: "rejected", Reason: "unknown code"}, http.StatusNotFound, nil
	}
	text := func(fields ...string) string {
		value, _, _ := unstructured.NestedString(admission.Object, fields...)
		return value
	}
	switch redeemedBy := text("spec", "publicKey"); {
	case redeemedBy == "":
		expires, err := time.Parse(time.RFC3339, text("spec", "expiresAt"))
		if err != nil || !b.now().Before(expires) {
			return BootstrapAnswer{State: "rejected", Reason: "the code has expired"}, http.StatusGone, nil
		}
		_ = unstructured.SetNestedField(admission.Object, publicKey, "spec", "publicKey")
		_ = unstructured.SetNestedField(admission.Object, b.now().UTC().Format(time.RFC3339), "spec", "redeemedAt")
		if _, err := b.Management.Resource(nodeAdmissionGVR).Namespace(admission.GetNamespace()).Update(ctx, admission, metav1.UpdateOptions{}); err != nil && !apierrors.IsConflict(err) {
			return BootstrapAnswer{}, 0, fmt.Errorf("record the redeemed code: %w", err)
		}
		return BootstrapAnswer{State: "pending", Reason: "waiting for station-controller"}, http.StatusAccepted, nil
	case redeemedBy != publicKey:
		return BootstrapAnswer{State: "rejected", Reason: "the code was redeemed by another machine"}, http.StatusConflict, nil
	}
	// A status that has not seen this redemption yet is not an answer to it:
	// a code given to a node for joining again leaves the old "Joined" there
	// until station-controller has answered.
	observed, _, _ := unstructured.NestedInt64(admission.Object, "status", "observedGeneration")
	if observed != admission.GetGeneration() {
		return BootstrapAnswer{State: "pending", Reason: "waiting for station-controller"}, http.StatusAccepted, nil
	}
	if text("status", "phase") == "Joined" {
		return BootstrapAnswer{State: "rejected", Reason: "the node has already joined"}, http.StatusGone, nil
	}
	config := text("status", "config")
	if config == "" || text("status", "publicKey") != publicKey || !json.Valid([]byte(config)) {
		reason := text("status", "reason")
		if reason == "" {
			reason = "waiting for station-controller"
		}
		return BootstrapAnswer{State: "pending", Reason: reason}, http.StatusAccepted, nil
	}

	clusterID := text("spec", "clusterID")
	// A node of a site whose registry cache is still being filled would pull
	// through the uplink what the cache is there to spare it: it waits, and
	// is told what the cache's addon says. A cluster with no cache waits for none.
	// (A management plane that knows no Addons has no cache either.)
	addons, err := b.Management.Resource(addonGVR).Namespace(clusterID).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		addons, err = &unstructured.UnstructuredList{}, nil
	}
	if err != nil {
		return BootstrapAnswer{}, 0, fmt.Errorf("list the Addons of %s: %w", clusterID, err)
	}
	for _, addon := range addons.Items {
		kind, _, _ := unstructured.NestedString(addon.Object, "spec", "addonName")
		role, _, _ := unstructured.NestedString(addon.Object, "spec", "values", "role")
		phase, _, _ := unstructured.NestedString(addon.Object, "status", "phase")
		seen, _, _ := unstructured.NestedInt64(addon.Object, "status", "observedGeneration")
		if kind == "site-registry" && role == "cache" && (phase != "Ready" || seen != addon.GetGeneration()) {
			message, _, _ := unstructured.NestedString(addon.Object, "status", "message")
			if message == "" || phase == "Ready" {
				message = "the site's registry cache " + addon.GetName() + " has not reported on its last change yet"
			}
			return BootstrapAnswer{State: "pending", Reason: message}, http.StatusAccepted, nil
		}
	}
	member, err := b.Member(ctx, clusterID)
	if err != nil {
		return BootstrapAnswer{}, 0, fmt.Errorf("reach cluster %s: %w", clusterID, err)
	}
	join, err := newJoinToken(ctx, member, admission.GetName(), b.now())
	if err != nil {
		return BootstrapAnswer{}, 0, err
	}
	join.APIServer = text("spec", "apiServer")
	labels, _, _ := unstructured.NestedStringMap(admission.Object, "spec", "labels")
	// A machine that joins by a code is one RLark manages, like a machine
	// joined by a node operation: the cluster's agents report only such nodes.
	if labels == nil {
		labels = map[string]string{}
	}
	labels["rlark.io/enabled"], labels["rlark.io/cluster-id"] = "true", clusterID
	// A machine installed anew under a name the management plane knows has a
	// management Node already, with the labels declared for it before: the
	// agent would put those back over what this admission declares.
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if _, err := b.Management.Resource(rlarkNodeGVR).Namespace(clusterID).Patch(ctx, admission.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return BootstrapAnswer{}, 0, fmt.Errorf("declare the labels on management Node %s/%s: %w", clusterID, admission.GetName(), err)
	}
	pairs := make([]string, 0, len(labels))
	for key, value := range labels {
		pairs = append(pairs, key+"="+value)
	}
	sort.Strings(pairs)
	taints, _, _ := unstructured.NestedStringSlice(admission.Object, "spec", "taints")
	return BootstrapAnswer{
		State: "ready", ClusterID: clusterID, NodeName: admission.GetName(), TunnelAddress: text("status", "address"),
		Config: json.RawMessage(config), Join: &join, Labels: strings.Join(pairs, ","), Taints: strings.Join(taints, ","),
	}, http.StatusOK, nil
}

// newJoinToken creates a kubeadm bootstrap token that lives half an hour and
// pins the cluster's CA the way kubeadm join expects it.
func newJoinToken(ctx context.Context, member kubernetes.Interface, nodeName string, now time.Time) (BootstrapJoin, error) {
	info, err := member.CoreV1().ConfigMaps("kube-public").Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil {
		return BootstrapJoin{}, fmt.Errorf("read cluster-info: %w", err)
	}
	kubeconfig, err := clientcmd.Load([]byte(info.Data["kubeconfig"]))
	if err != nil || len(kubeconfig.Clusters) != 1 {
		return BootstrapJoin{}, errors.New("cluster-info holds no single cluster")
	}
	var caHash string
	for _, cluster := range kubeconfig.Clusters {
		block, _ := pem.Decode(cluster.CertificateAuthorityData)
		if block == nil {
			return BootstrapJoin{}, errors.New("cluster-info holds no CA certificate")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return BootstrapJoin{}, fmt.Errorf("parse the cluster CA: %w", err)
		}
		sum := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
		caHash = "sha256:" + hex.EncodeToString(sum[:])
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	random := make([]byte, 22)
	if _, err := rand.Read(random); err != nil {
		return BootstrapJoin{}, err
	}
	for i := range random {
		random[i] = alphabet[int(random[i])%len(alphabet)]
	}
	id, secret := string(random[:6]), string(random[6:])
	_, err = member.CoreV1().Secrets("kube-system").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-" + id, Namespace: "kube-system"},
		Type:       corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"token-id": id, "token-secret": secret, "expiration": now.Add(30 * time.Minute).UTC().Format(time.RFC3339),
			"usage-bootstrap-authentication": "true", "usage-bootstrap-signing": "true",
			"auth-extra-groups": "system:bootstrappers:kubeadm:default-node-token",
			"description":       "RLark node admission of " + nodeName,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return BootstrapJoin{}, fmt.Errorf("create the join token: %w", err)
	}
	return BootstrapJoin{Token: id + "." + secret, CACertHash: caHash}, nil
}
