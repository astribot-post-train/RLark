package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	nodeAdmissionGVR = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "nodeadmissions"}
	siteNetworkGVR   = schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: "sitenetworks"}
)

const (
	maxNodeAdmissionRequestBytes = 16 * 1024
	defaultJoinCodeTTL           = 2 * time.Hour
	minJoinCodeTTL               = 10 * time.Minute
	maxJoinCodeTTL               = 7 * 24 * time.Hour
	joinCodeLength               = 12
)

// stationBootstrap is Station API as the Gateway sees it when it gives out a
// join code: where a new machine reaches it, and the digests of what it
// serves, read over the Gateway's own verified connection. The machine's
// command pins those digests before it trusts anything it fetches.
type stationBootstrap interface {
	// Address is host:port of Station API as a machine that joins reaches it.
	Address() string
	// Digest is the SHA-256, in hex, of what Station API serves at path.
	Digest(ctx context.Context, path string) (string, error)
}

type stationBootstrapClient struct {
	address, origin string
	http            *http.Client
}

func newStationBootstrapClient(bootstrapURL, origin, caFile string) (*stationBootstrapClient, error) {
	machines, err := url.Parse(bootstrapURL)
	if err != nil || machines.Scheme != "https" || machines.Port() == "" || machines.User != nil || (machines.Path != "" && machines.Path != "/") || machines.RawQuery != "" {
		return nil, errors.New("the Station API bootstrap URL must be https://host:port")
	}
	address := machines.Host
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || caFile == "" {
		return nil, errors.New("the Station API HTTPS origin and CA are required to give out join codes")
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("read Station API CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid Station API CA")
	}
	return &stationBootstrapClient{address: address, origin: strings.TrimRight(origin, "/"),
		http: &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}},
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }}}, nil
}

func (s *stationBootstrapClient) Address() string { return s.address }

func (s *stationBootstrapClient) Digest(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.origin+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The path may hold a code: it stays out of the error.
		return "", fmt.Errorf("Station API answered %d", resp.StatusCode)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

type issueNodeAdmissionRequest struct {
	ClusterID string `json:"cluster_id"`
	NodeName  string `json:"node_name"`
	// APIServer is host:port of the cluster's API as the machine reaches it.
	APIServer string `json:"api_server"`
	// Site is the site the machine stands in when it is not its cluster's.
	Site string `json:"site,omitempty"`
	// Address pins the tunnel address; empty lets station-controller allocate one.
	Address string `json:"address,omitempty"`
	// Uplink is the gateway of a machine whose default route does not lead
	// out, where its site declares no leafUplink.
	Uplink     string            `json:"uplink,omitempty"`
	Taints     []string          `json:"taints,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	TTLSeconds int64             `json:"ttl_seconds,omitempty"`
}

type reissueNodeAdmissionRequest struct {
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// nodeAdmission is one machine's admission as an operator reads it. The code
// is never in it: the Gateway answers it once, when it gives it out.
type nodeAdmission struct {
	ClusterID string `json:"cluster_id"`
	NodeName  string `json:"node_name"`
	// State is Pending (the code is open), Redeemed (a machine took it and is
	// joining), Joined, or Expired (also a code withdrawn before it was used).
	State      string            `json:"state"`
	Reason     string            `json:"reason,omitempty"`
	APIServer  string            `json:"api_server"`
	Site       string            `json:"site,omitempty"`
	Address    string            `json:"address,omitempty"`
	Uplink     string            `json:"uplink,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Taints     []string          `json:"taints,omitempty"`
	ExpiresAt  string            `json:"expires_at"`
	IssuedBy   string            `json:"issued_by,omitempty"`
	IssuedAt   string            `json:"issued_at,omitempty"`
	RedeemedAt string            `json:"redeemed_at,omitempty"`
	// PublicKey is the WireGuard public key of the machine that redeemed the code.
	PublicKey string `json:"public_key,omitempty"`
	// TunnelAddress is the address station-controller gave the machine.
	TunnelAddress string `json:"tunnel_address,omitempty"`
}

// issuedNodeAdmission is the one answer that carries the code, and the line
// the operator runs as root on the machine.
type issuedNodeAdmission struct {
	Admission nodeAdmission `json:"admission"`
	Code      string        `json:"code"`
	Command   string        `json:"command"`
}

func nodeAdmissionView(object *unstructured.Unstructured, now time.Time) nodeAdmission {
	text := func(fields ...string) string {
		value, _, _ := unstructured.NestedString(object.Object, fields...)
		return value
	}
	view := nodeAdmission{
		ClusterID: text("spec", "clusterID"), NodeName: object.GetName(), APIServer: text("spec", "apiServer"), Site: text("spec", "site"),
		Address: text("spec", "address"), Uplink: text("spec", "uplink", "gateway"), ExpiresAt: text("spec", "expiresAt"),
		IssuedBy: text("spec", "issuedBy"), IssuedAt: text("spec", "issuedAt"), RedeemedAt: text("spec", "redeemedAt"), PublicKey: text("spec", "publicKey"),
		TunnelAddress: text("status", "address"), Reason: text("status", "reason"),
	}
	view.Labels, _, _ = unstructured.NestedStringMap(object.Object, "spec", "labels")
	view.Taints, _, _ = unstructured.NestedStringSlice(object.Object, "spec", "taints")
	// The state of the code is read from spec, which the Gateway and Station
	// API write; station-controller's answer only says whether a redeemed
	// code has led to a node yet.
	expires, _ := time.Parse(time.RFC3339, view.ExpiresAt)
	observed, _, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	switch {
	case view.PublicKey == "" && !now.Before(expires):
		view.State = "Expired"
	case view.PublicKey == "":
		view.State = "Pending"
	case observed == object.GetGeneration() && text("status", "phase") == "Joined":
		view.State = "Joined"
	default:
		view.State = "Redeemed"
	}
	return view
}

func decodeNodeAdmissionRequest(c *gin.Context, into any, emptyAllowed bool) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxNodeAdmissionRequestBytes))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(into)
	if err == io.EOF && emptyAllowed {
		return true
	}
	var trailing any
	if err != nil || decoder.Decode(&trailing) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the request must be one JSON object of the documented fields"})
		return false
	}
	return true
}

func joinCodeTTL(seconds int64) (time.Duration, error) {
	if seconds == 0 {
		return defaultJoinCodeTTL, nil
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl < minJoinCodeTTL || ttl > maxJoinCodeTTL {
		return 0, fmt.Errorf("ttl_seconds must be between %d and %d", int64(minJoinCodeTTL.Seconds()), int64(maxJoinCodeTTL.Seconds()))
	}
	return ttl, nil
}

func validateIssueNodeAdmission(request *issueNodeAdmissionRequest) error {
	if request.ClusterID == "" {
		return errors.New("cluster_id is required")
	}
	if len(validation.IsDNS1123Subdomain(request.NodeName)) > 0 {
		return errors.New("node_name must be a DNS subdomain")
	}
	if host, port, err := net.SplitHostPort(request.APIServer); err != nil || host == "" || len(validation.IsValidPortNum(atoi(port))) > 0 {
		return errors.New("api_server must be host:port")
	}
	if request.Site != "" && len(validation.IsDNS1123Label(request.Site)) > 0 {
		return errors.New("site must be a DNS label")
	}
	for field, value := range map[string]string{"address": request.Address, "uplink": request.Uplink} {
		if ip := net.ParseIP(value); value != "" && (ip == nil || ip.To4() == nil) {
			return fmt.Errorf("%s must be an IPv4 address", field)
		}
	}
	if len(request.Labels) > 32 {
		return errors.New("at most 32 labels")
	}
	for key, value := range request.Labels {
		if len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			return fmt.Errorf("label %q is not a valid Kubernetes label", key)
		}
	}
	if len(request.Taints) > 8 {
		return errors.New("at most 8 taints")
	}
	for _, taint := range request.Taints {
		rest, effect, found := strings.Cut(taint, ":")
		key, value, _ := strings.Cut(rest, "=")
		if !found || (effect != "NoSchedule" && effect != "PreferNoSchedule" && effect != "NoExecute") ||
			len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			return fmt.Errorf("taint %q must be key[=value]:NoSchedule|PreferNoSchedule|NoExecute", taint)
		}
	}
	return nil
}

func atoi(text string) int {
	value, err := strconv.Atoi(text)
	if err != nil {
		return -1
	}
	return value
}

func newJoinCode() (string, string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	// 252 is the largest multiple of 36 that fits a byte: rejecting above it
	// keeps every character equally likely.
	code := make([]byte, 0, joinCodeLength)
	buffer := make([]byte, 2*joinCodeLength)
	for len(code) < joinCodeLength {
		if _, err := rand.Read(buffer); err != nil {
			return "", "", err
		}
		for _, b := range buffer {
			if b < 252 && len(code) < joinCodeLength {
				code = append(code, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	digest := sha256.Sum256(code)
	return string(code), hex.EncodeToString(digest[:]), nil
}

// nodeAdmissionsAvailable answers 503 when the Gateway cannot give out codes.
func (g *Gateway) nodeAdmissionsAvailable(c *gin.Context, issuing bool) bool {
	if g.stationClient == nil || issuing && g.stationBootstrap == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "join codes are not configured on this Gateway (--station-api-bootstrap-url)"})
		return false
	}
	return true
}

// siteNetwork reads the one SiteNetwork; a management plane without one has no sites.
func (g *Gateway) siteNetwork(ctx context.Context) (*unstructured.Unstructured, error) {
	network, err := g.stationClient.Resource(siteNetworkGVR).Namespace("station-system").Get(ctx, "default", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &unstructured.Unstructured{Object: map[string]interface{}{}}, nil
	}
	return network, err
}

// leafUplinkOf returns the device a machine of this admission stands
// behind, when its site declares one with a setup: the SiteNetwork's entry
// for the node names the site, or else the admission, or else the site of
// its cluster.
func leafUplinkOf(network *unstructured.Unstructured, clusterID, nodeName, site string) (gateway, login string) {
	nodes, _, _ := unstructured.NestedSlice(network.Object, "spec", "nodes")
	for _, raw := range nodes {
		entry, _ := raw.(map[string]interface{})
		if named, _ := entry["site"].(string); entry["clusterID"] == clusterID && entry["nodeName"] == nodeName && named != "" {
			site = named
		}
	}
	sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
	for _, raw := range sites {
		entry, _ := raw.(map[string]interface{})
		clusters, _, _ := unstructured.NestedStringSlice(entry, "clusters")
		if site != "" && entry["name"] != site || site == "" && !containsString(clusters, clusterID) {
			continue
		}
		gateway, _, _ = unstructured.NestedString(entry, "leafUplink", "gateway")
		login, _, _ = unstructured.NestedString(entry, "leafUplink", "setup", "login")
		if gateway != "" && login != "" {
			return gateway, login
		}
	}
	return "", ""
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// joinCommand is the one line the operator runs on the machine. The machine
// has no CA of ours yet: the line fetches the CA unverified, checks it
// against the digest it carries, and verifies every connection after that
// with it. On a site whose leaves stand behind a device of their own, the
// line first makes the device let the machine out, running the device's
// setup only if it is, byte for byte, the file Station API serves now.
func (g *Gateway) joinCommand(ctx context.Context, network *unstructured.Unstructured, clusterID, nodeName, site, code, caDigest string) (string, error) {
	address := g.stationBootstrap.Address()
	trust := fmt.Sprintf(`c=$(mktemp) && curl --connect-timeout 15 -fsSk https://%s/v1/bootstrap/ca -o $c && echo "%s  $c" | sha256sum -c - && curl -fsS --cacert $c https://%s/b/%s`,
		address, caDigest, address, code)
	first := ""
	if gateway, login := leafUplinkOf(network, clusterID, nodeName, site); gateway != "" {
		digest, err := g.stationBootstrap.Digest(ctx, "/b/"+code+"/uplink")
		if err != nil {
			return "", fmt.Errorf("read the uplink setup of the site: %w", err)
		}
		first = fmt.Sprintf(`ssh -t %s@%s 'f=$(mktemp) %s/uplink -o $f && echo "%s  $f" | sha256sum -c - && sudo sh $f' && `, login, gateway, trust, digest)
	}
	return first + trust + " | sudo sh", nil
}

// handleListNodeAdmissions answers GET /api/v1/station/node-admissions?cluster_id=:
// every admission of a cluster, newest code first.
func (g *Gateway) handleListNodeAdmissions(c *gin.Context) {
	if !g.nodeAdmissionsAvailable(c, false) {
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
	list, err := g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeAdmission request failed"})
		return
	}
	now := time.Now()
	items := make([]nodeAdmission, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, nodeAdmissionView(&list.Items[i], now))
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IssuedAt != items[j].IssuedAt {
			return items[i].IssuedAt > items[j].IssuedAt
		}
		return items[i].NodeName < items[j].NodeName
	})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// handleIssueNodeAdmission answers POST /api/v1/station/node-admissions: it admits a
// machine as the named node and answers the code once. A node that has an
// admission keeps it, as what declares its tunnel address; a new code for it
// replaces what the admission declares and opens it again.
func (g *Gateway) handleIssueNodeAdmission(c *gin.Context) {
	if !g.nodeAdmissionsAvailable(c, true) {
		return
	}
	var request issueNodeAdmissionRequest
	if !decodeNodeAdmissionRequest(c, &request, false) {
		return
	}
	ttl, err := joinCodeTTL(request.TTLSeconds)
	if err == nil {
		err = validateIssueNodeAdmission(&request)
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	namespace, err := g.stationNamespace(c, request.ClusterID)
	if err != nil {
		return
	}
	network, err := g.siteNetwork(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "SiteNetwork request failed"})
		return
	}
	if request.Site != "" {
		declared := false
		sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
		for _, raw := range sites {
			entry, _ := raw.(map[string]interface{})
			declared = declared || entry["name"] == request.Site
		}
		if !declared {
			c.JSON(http.StatusBadRequest, gin.H{"error": "the SiteNetwork has no site " + request.Site})
			return
		}
	}
	// Nothing is admitted unless the machine's command can be made.
	caDigest, err := g.stationBootstrap.Digest(ctx, "/v1/bootstrap/ca")
	if err != nil {
		log.FromContext(ctx).Error(err, "read Station API's bootstrap CA")
		c.JSON(http.StatusBadGateway, gin.H{"error": "Station API does not serve its bootstrap CA"})
		return
	}
	code, codeHash, err := newJoinCode()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot make a code"})
		return
	}
	now := time.Now().UTC()
	spec := map[string]interface{}{
		"clusterID": request.ClusterID, "codeHash": codeHash, "apiServer": request.APIServer,
		"expiresAt": now.Add(ttl).Format(time.RFC3339), "issuedBy": c.GetString(authSubjectKey), "issuedAt": now.Format(time.RFC3339),
	}
	if request.Site != "" {
		spec["site"] = request.Site
	}
	if request.Address != "" {
		spec["address"] = request.Address
	}
	if request.Uplink != "" {
		spec["uplink"] = map[string]interface{}{"gateway": request.Uplink}
	}
	if len(request.Taints) > 0 {
		taints := make([]interface{}, len(request.Taints))
		for i, taint := range request.Taints {
			taints[i] = taint
		}
		spec["taints"] = taints
	}
	if len(request.Labels) > 0 {
		labels := map[string]interface{}{}
		for key, value := range request.Labels {
			labels[key] = value
		}
		spec["labels"] = labels
	}
	resource := g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace)
	existing, err := resource.Get(ctx, request.NodeName, metav1.GetOptions{})
	var written *unstructured.Unstructured
	status := http.StatusOK
	switch {
	case apierrors.IsNotFound(err):
		status = http.StatusCreated
		written, err = resource.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission",
			"metadata": map[string]interface{}{"name": request.NodeName, "namespace": namespace}, "spec": spec,
		}}, metav1.CreateOptions{})
	case err == nil:
		existing.Object["spec"] = spec
		written, err = resource.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if !g.respondNodeAdmissionWrite(c, err) {
		return
	}
	g.respondIssuedNodeAdmission(c, status, network, written, code, caDigest, "issue")
}

// handleReissueNodeAdmission answers POST /api/v1/station/node-admissions/:name/reissue:
// a new code for what the admission declares; the earlier code stops working.
func (g *Gateway) handleReissueNodeAdmission(c *gin.Context) {
	if !g.nodeAdmissionsAvailable(c, true) {
		return
	}
	var request reissueNodeAdmissionRequest
	if !decodeNodeAdmissionRequest(c, &request, true) {
		return
	}
	ttl, err := joinCodeTTL(request.TTLSeconds)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	admission, resource, ok := g.existingNodeAdmission(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	network, err := g.siteNetwork(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "SiteNetwork request failed"})
		return
	}
	caDigest, err := g.stationBootstrap.Digest(ctx, "/v1/bootstrap/ca")
	if err != nil {
		log.FromContext(ctx).Error(err, "read Station API's bootstrap CA")
		c.JSON(http.StatusBadGateway, gin.H{"error": "Station API does not serve its bootstrap CA"})
		return
	}
	code, codeHash, err := newJoinCode()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot make a code"})
		return
	}
	now := time.Now().UTC()
	spec, _, _ := unstructured.NestedMap(admission.Object, "spec")
	for _, field := range []string{"publicKey", "redeemedAt"} {
		delete(spec, field)
	}
	spec["codeHash"], spec["expiresAt"] = codeHash, now.Add(ttl).Format(time.RFC3339)
	spec["issuedBy"], spec["issuedAt"] = c.GetString(authSubjectKey), now.Format(time.RFC3339)
	admission.Object["spec"] = spec
	written, err := resource.Update(ctx, admission, metav1.UpdateOptions{})
	if !g.respondNodeAdmissionWrite(c, err) {
		return
	}
	g.respondIssuedNodeAdmission(c, http.StatusOK, network, written, code, caDigest, "reissue")
}

// handleRevokeNodeAdmission answers POST /api/v1/station/node-admissions/:name/revoke:
// the code stops working now: its expiry is set to now, which Station API and
// station-controller already treat as the end of the code. A code a machine
// has redeemed is that machine's; it is not withdrawn here.
func (g *Gateway) handleRevokeNodeAdmission(c *gin.Context) {
	if !g.nodeAdmissionsAvailable(c, false) {
		return
	}
	admission, resource, ok := g.existingNodeAdmission(c)
	if !ok {
		return
	}
	now := time.Now().UTC()
	switch view := nodeAdmissionView(admission, now); view.State {
	case "Pending":
	case "Expired":
		c.JSON(http.StatusOK, gin.H{"admission": view})
		return
	default:
		c.JSON(http.StatusConflict, gin.H{"error": "a machine has redeemed the code; remove the node instead"})
		return
	}
	_ = unstructured.SetNestedField(admission.Object, now.Format(time.RFC3339), "spec", "expiresAt")
	written, err := resource.Update(c.Request.Context(), admission, metav1.UpdateOptions{})
	if !g.respondNodeAdmissionWrite(c, err) {
		return
	}
	view := nodeAdmissionView(written, now)
	log.FromContext(c.Request.Context()).WithName("audit").Info("node admission", "action", "revoke", "actor", c.GetString(authSubjectKey),
		"cluster", view.ClusterID, "node", view.NodeName)
	c.JSON(http.StatusOK, gin.H{"admission": view})
}

func (g *Gateway) existingNodeAdmission(c *gin.Context) (*unstructured.Unstructured, dynamic.ResourceInterface, bool) {
	clusterID := c.Query("cluster_id")
	if clusterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cluster_id is required"})
		return nil, nil, false
	}
	namespace, err := g.stationNamespace(c, clusterID)
	if err != nil {
		return nil, nil, false
	}
	resource := g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace)
	admission, err := resource.Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "node admission not found"})
		return nil, nil, false
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeAdmission request failed"})
		return nil, nil, false
	}
	return admission, resource, true
}

func (g *Gateway) respondNodeAdmissionWrite(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return true
	case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
		c.JSON(http.StatusConflict, gin.H{"error": "the admission changed meanwhile; try again"})
	case apierrors.IsInvalid(err):
		c.JSON(http.StatusBadRequest, gin.H{"error": "the management plane rejected the admission"})
	default:
		log.FromContext(c.Request.Context()).Error(err, "write NodeAdmission")
		c.JSON(http.StatusBadGateway, gin.H{"error": "NodeAdmission request failed"})
	}
	return false
}

func (g *Gateway) respondIssuedNodeAdmission(c *gin.Context, status int, network, written *unstructured.Unstructured, code, caDigest, action string) {
	ctx := c.Request.Context()
	view := nodeAdmissionView(written, time.Now())
	command, err := g.joinCommand(ctx, network, view.ClusterID, view.NodeName, view.Site, code, caDigest)
	if err != nil {
		// The admission is written and its code is lost with this answer:
		// a new code replaces it.
		log.FromContext(ctx).Error(err, "make the join command", "cluster", view.ClusterID, "node", view.NodeName)
		c.JSON(http.StatusBadGateway, gin.H{"error": "the admission is written but its command cannot be made; give out a new code"})
		return
	}
	log.FromContext(ctx).WithName("audit").Info("node admission", "action", action, "actor", view.IssuedBy,
		"cluster", view.ClusterID, "node", view.NodeName, "expiresAt", view.ExpiresAt)
	c.Header("Cache-Control", "no-store")
	c.JSON(status, issuedNodeAdmission{Admission: view, Code: code, Command: command})
}
