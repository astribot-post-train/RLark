// Package console provides the independent administrator management API.
// It never accepts Agent credentials or proxies arbitrary management resources.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const prefix = "/api/v1/container-runtime/"

// Management provides only inventory reads and optimistic desired-state patches.
type Management interface {
	client.Reader
	Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error
}

// ClusterCreator resumes a retained administrator provisioning operation.
type ClusterCreator interface {
	CreateCluster(context.Context, provision.ClusterRequest) (*ext.ContainerCluster, error)
}

// API has explicit dependencies and uses an uncached management client.
// Auth is required even for reads. UI is an optional static, public application.
type API struct {
	Client  Management
	Creator ClusterCreator
	Auth    Authenticator
	UI      http.Handler
	Now     func() time.Time
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Request-ID", uuid.NewString())
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !strings.HasPrefix(r.URL.Path, prefix) {
		if strings.HasPrefix(r.URL.Path, "/api/") || a.UI == nil {
			http.NotFound(w, r)
		} else {
			a.UI.ServeHTTP(w, r)
		}
		return
	}
	if a.Auth == nil {
		writeError(w, 503, "unavailable", "authentication unavailable")
		return
	}
	subject, err := a.Auth.Administrator(r)
	if err != nil {
		status := 401
		if errors.Is(err, errForbidden) {
			status = 403
		}
		writeError(w, status, "unauthorized", http.StatusText(status))
		return
	}
	if a.Client == nil {
		writeError(w, 503, "unavailable", "management service unavailable")
		return
	}
	// Cookies never authorize requests. Cross-origin writes are also rejected.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if origin := r.Header.Get("Origin"); origin != "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			if origin != scheme+"://"+r.Host {
				writeError(w, 403, "forbidden", "same-origin request required")
				return
			}
		}
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	switch {
	case len(parts) == 1 && parts[0] == "clusters" && r.Method == http.MethodGet:
		a.listClusters(w, r)
	case len(parts) == 1 && parts[0] == "clusters" && r.Method == http.MethodPost:
		a.createCluster(w, r, subject)
	case len(parts) == 3 && parts[0] == "clusters" && parts[1] != "" && parts[2] != "" && r.Method == http.MethodGet:
		a.clusterDetail(w, r, parts[1], parts[2])
	case len(parts) == 3 && parts[0] == "clusters" && parts[1] != "" && parts[2] != "" && r.Method == http.MethodPatch:
		a.patchCluster(w, r, parts[1], parts[2], subject)
	default:
		writeError(w, 404, "not_found", "route not found")
	}
}

type clusterSummary struct {
	Namespace          string                   `json:"namespace"`
	Name               string                   `json:"name"`
	UID                string                   `json:"uid"`
	ResourceVersion    string                   `json:"resourceVersion"`
	TenantID           string                   `json:"tenantID"`
	Runtime            ext.Runtime              `json:"runtime"`
	TargetAgentRelease string                   `json:"targetAgentRelease"`
	BindingValid       bool                     `json:"bindingValid"`
	Deleting           bool                     `json:"deleting"`
	Conditions         []metav1.Condition       `json:"conditions"`
	NodeCount          int                      `json:"nodeCount"`
	ReadyNodes         int                      `json:"readyNodes"`
	BusyNodes          int                      `json:"busyNodes"`
	GPUCount           int                      `json:"gpuCount"`
	AllocationCount    int                      `json:"allocationCount"`
	UpgradeCount       int                      `json:"upgradeCount"`
	LastAgentUpgrade   *ext.AgentRolloutReceipt `json:"lastAgentUpgrade,omitempty"`
}
type nodeSummary struct {
	Name               string             `json:"name"`
	UID                string             `json:"uid"`
	InstanceID         string             `json:"instanceID"`
	PrivateAddress     string             `json:"privateAddress"`
	Environment        string             `json:"environment"`
	NetworkScope       string             `json:"networkScope"`
	AgentVersion       string             `json:"agentVersion"`
	Ready              bool               `json:"ready"`
	Schedulable        bool               `json:"schedulable"`
	Busy               bool               `json:"busy"`
	Held               bool               `json:"held"`
	BindingValid       bool               `json:"bindingValid"`
	Capacity           map[string]string  `json:"capacity"`
	AuthorizedGPUUUIDs []string           `json:"authorizedGPUUUIDs"`
	Capabilities       []string           `json:"capabilities"`
	LastHeartbeatTime  *metav1.Time       `json:"lastHeartbeatTime"`
	Conditions         []metav1.Condition `json:"conditions"`
}
type releaseSummary struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Version      string `json:"version"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	SHA256       string `json:"sha256"`
	Available    bool   `json:"available"`
}
type allocationSummary struct {
	Name             string `json:"name"`
	UID              string `json:"uid"`
	NodeName         string `json:"nodeName"`
	JobName          string `json:"jobName"`
	TaskName         string `json:"taskName"`
	Kind             string `json:"kind"`
	Phase            string `json:"phase"`
	Deleting         bool   `json:"deleting"`
	CleanupConfirmed bool   `json:"cleanupConfirmed"`
}
type upgradeSummary struct {
	Name        string             `json:"name"`
	UID         string             `json:"uid"`
	NodeName    string             `json:"nodeName"`
	ReleaseName string             `json:"releaseName"`
	Phase       string             `json:"phase"`
	Conditions  []metav1.Condition `json:"conditions"`
}
type clusterDetailResponse struct {
	Cluster     clusterSummary      `json:"cluster"`
	Nodes       []nodeSummary       `json:"nodes"`
	Releases    []releaseSummary    `json:"releases"`
	Allocations []allocationSummary `json:"allocations"`
	Upgrades    []upgradeSummary    `json:"upgrades"`
}

func current(items []metav1.Condition, typ string, generation int64) bool {
	for _, c := range items {
		if c.Type == typ {
			return c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
		}
	}
	return false
}

// binding verifies authoritative membership and rejects ambiguous ownership.
// Invalid inventory remains visible but cannot authorize a mutation.
func (a *API) binding(ctx context.Context, cc *ext.ContainerCluster) (*native.Cluster, bool, error) {
	var n native.Cluster
	if err := a.Client.Get(ctx, client.ObjectKey{Name: cc.Spec.Cluster.Name}, &n); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	valid := cc.UID != "" && cc.Spec.Cluster.Namespace == "" && ownership.MatchesNative(cc.Spec.Cluster, &n) == nil && tenant.IDOf(cc) != "" && tenant.IDOf(cc) == tenant.IDOf(&n) && cc.Namespace != "" && cc.Namespace == n.Spec.AgentNamespace && cc.Spec.AgentNamespace == n.Spec.AgentNamespace && (cc.Spec.Runtime == "Raw" || cc.Spec.Runtime == "Docker")
	var natives native.ClusterList
	if err := a.Client.List(ctx, &natives); err != nil {
		return nil, false, err
	}
	for _, other := range natives.Items {
		if other.UID != n.UID && other.Spec.AgentNamespace == n.Spec.AgentNamespace {
			valid = false
		}
	}
	var companions ext.ContainerClusterList
	if err := a.Client.List(ctx, &companions); err != nil {
		return nil, false, err
	}
	for _, other := range companions.Items {
		if other.UID != cc.UID && (other.Spec.Cluster == cc.Spec.Cluster || other.Namespace == cc.Namespace) {
			valid = false
		}
	}
	return &n, valid, nil
}
func (a *API) detail(ctx context.Context, cc *ext.ContainerCluster) (*clusterDetailResponse, error) {
	nc, valid, err := a.binding(ctx, cc)
	if err != nil {
		return nil, err
	}
	result := &clusterDetailResponse{Cluster: clusterSummary{Namespace: cc.Namespace, Name: cc.Name, UID: string(cc.UID), ResourceVersion: cc.ResourceVersion, TenantID: tenant.IDOf(cc), Runtime: cc.Spec.Runtime, TargetAgentRelease: cc.Spec.TargetAgentRelease, BindingValid: valid, Deleting: !cc.DeletionTimestamp.IsZero(), Conditions: cc.Status.Conditions, LastAgentUpgrade: cc.Status.LastAgentUpgrade}, Nodes: []nodeSummary{}, Releases: []releaseSummary{}, Allocations: []allocationSummary{}, Upgrades: []upgradeSummary{}}
	var allocations ext.ContainerAllocationList
	if err := a.Client.List(ctx, &allocations, client.InNamespace(cc.Namespace)); err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, v := range allocations.Items {
		held[v.Spec.NodeRecordUID] = true
		held["native/"+v.Spec.Node.UID] = true
		if v.Spec.Cluster != cc.Spec.Cluster || v.Spec.TenantID != tenant.IDOf(cc) || tenant.IDOf(&v) != tenant.IDOf(cc) {
			continue
		}
		held[v.Spec.NodeRecordUID] = true
		kind := "workload"
		if v.Spec.Upgrade != nil {
			kind = "upgrade"
		}
		result.Allocations = append(result.Allocations, allocationSummary{Name: v.Name, UID: string(v.UID), NodeName: v.Spec.Node.Name, JobName: v.Spec.Job.Name, TaskName: v.Spec.Task.Name, Kind: kind, Phase: v.Status.Phase, Deleting: !v.DeletionTimestamp.IsZero(), CleanupConfirmed: v.Status.CleanupConfirmed && v.Status.ObservedGeneration == v.Generation})
	}
	var nodes ext.ContainerNodeList
	if err := a.Client.List(ctx, &nodes, client.InNamespace(cc.Namespace)); err != nil {
		return nil, err
	}
	for _, n := range nodes.Items {
		if n.Spec.Cluster != cc.Spec.Cluster || tenant.IDOf(&n) != tenant.IDOf(cc) {
			continue
		}
		var nn native.Node
		err := a.Client.Get(ctx, client.ObjectKey{Namespace: n.Spec.Node.Namespace, Name: n.Spec.Node.Name}, &nn)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		runtimeMatches := (cc.Spec.Runtime == "Raw" && nn.Spec.AgentType == native.AgentTypeRaw) || (cc.Spec.Runtime == "Docker" && nn.Spec.AgentType == native.AgentTypeDocker)
		nv := valid && runtimeMatches && err == nil && n.UID != "" && ownership.MatchesNative(n.Spec.Node, &nn) == nil && nn.Namespace == cc.Namespace && tenant.IDOf(&nn) == tenant.IDOf(cc) && n.Spec.Runtime == cc.Spec.Runtime
		heartbeat := n.Status.LastHeartbeatTime
		fresh := heartbeat != nil && !heartbeat.After(a.now()) && a.now().Sub(heartbeat.Time) <= 45*time.Second
		ready := nv && n.DeletionTimestamp.IsZero() && nn.DeletionTimestamp.IsZero() && nc.DeletionTimestamp.IsZero() && cc.DeletionTimestamp.IsZero() && fresh && n.Status.ObservedGeneration == n.Generation && current(n.Status.Conditions, "Ready", n.Generation) && n.Status.AgentCredentialID != "" && n.Status.AgentCredentialID == n.Annotations[ext.AgentCredentialAnnotation]
		schedulable := ready && n.Status.Schedulable && !n.Status.Busy && !held[string(n.UID)] && !held["native/"+n.Spec.Node.UID] && !nn.Spec.Unschedulable && !nc.Spec.Cordoned && !nc.Spec.Draining && nn.Status.Reservation == nil && len(nn.Status.DockerReservations) == 0
		result.Nodes = append(result.Nodes, nodeSummary{Name: n.Name, UID: string(n.UID), InstanceID: n.Spec.InstanceID, PrivateAddress: n.Spec.PrivateAddress, Environment: n.Spec.Environment, NetworkScope: n.Spec.NetworkScope, AgentVersion: n.Status.AgentVersion, Ready: ready, Schedulable: schedulable, Busy: n.Status.Busy, Held: held[string(n.UID)] || held["native/"+n.Spec.Node.UID], BindingValid: nv, Capacity: n.Status.Capacity, AuthorizedGPUUUIDs: n.Spec.AuthorizedGPUUUIDs, Capabilities: n.Status.Capabilities, LastHeartbeatTime: heartbeat, Conditions: n.Status.Conditions})
		result.Cluster.NodeCount++
		result.Cluster.GPUCount += len(n.Spec.AuthorizedGPUUUIDs)
		if ready {
			result.Cluster.ReadyNodes++
		}
		if n.Status.Busy {
			result.Cluster.BusyNodes++
		}
	}
	var releases ext.AgentReleaseList
	if err := a.Client.List(ctx, &releases, client.InNamespace(cc.Namespace)); err != nil {
		return nil, err
	}
	for _, v := range releases.Items {
		if tenant.IDOf(&v) == tenant.IDOf(cc) {
			result.Releases = append(result.Releases, releaseSummary{Name: v.Name, UID: string(v.UID), Version: v.Spec.Version, OS: v.Spec.OS, Architecture: v.Spec.Architecture, SHA256: v.Spec.SHA256, Available: available(&v)})
		}
	}
	var upgrades ext.AgentUpgradeList
	if err := a.Client.List(ctx, &upgrades, client.InNamespace(cc.Namespace)); err != nil {
		return nil, err
	}
	for _, v := range upgrades.Items {
		if v.Spec.Cluster == cc.Spec.Cluster && v.Spec.ClusterRecord.UID == string(cc.UID) && v.Spec.TenantID == tenant.IDOf(cc) && tenant.IDOf(&v) == tenant.IDOf(cc) {
			result.Upgrades = append(result.Upgrades, upgradeSummary{Name: v.Name, UID: string(v.UID), NodeName: v.Spec.NodeRecord.Name, ReleaseName: v.Spec.Release.Name, Phase: v.Status.Phase, Conditions: v.Status.Conditions})
		}
	}
	if cc.Spec.TargetAgentRelease != "" {
		targetVersion := ""
		for _, release := range result.Releases {
			if release.Name == cc.Spec.TargetAgentRelease && release.Available {
				targetVersion = release.Version
			}
		}
		for i := range result.Nodes {
			if targetVersion == "" || result.Nodes[i].AgentVersion != targetVersion {
				result.Nodes[i].Schedulable = false
			}
		}
	}
	result.Cluster.AllocationCount = len(result.Allocations)
	result.Cluster.UpgradeCount = len(result.Upgrades)
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].Name < result.Nodes[j].Name })
	sort.Slice(result.Releases, func(i, j int) bool { return result.Releases[i].Name < result.Releases[j].Name })
	return result, nil
}
func available(v *ext.AgentRelease) bool {
	return v.UID != "" && v.DeletionTimestamp.IsZero() && v.Status.Available && v.Status.ObservedGeneration == v.Generation && current(v.Status.Conditions, "Available", v.Generation) && artifact.ValidateSpec(v.Spec) == nil
}
func (a *API) listClusters(w http.ResponseWriter, r *http.Request) {
	var list ext.ContainerClusterList
	if err := a.Client.List(r.Context(), &list); err != nil {
		writeKubeError(w, err)
		return
	}
	result := make([]clusterSummary, 0, len(list.Items))
	for i := range list.Items {
		detail, err := a.detail(r.Context(), &list.Items[i])
		if err != nil {
			writeKubeError(w, err)
			return
		}
		result = append(result, detail.Cluster)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Namespace+"/"+result[i].Name < result[j].Namespace+"/"+result[j].Name
	})
	writeJSON(w, 200, struct {
		Items []clusterSummary `json:"items"`
	}{result})
}
func (a *API) clusterDetail(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var cc ext.ContainerCluster
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &cc); err != nil {
		writeKubeError(w, err)
		return
	}
	result, err := a.detail(r.Context(), &cc)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) createCluster(w http.ResponseWriter, r *http.Request, subject string) {
	var req provision.ClusterRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if a.Creator == nil {
		writeError(w, 503, "unavailable", "cluster provisioning unavailable")
		return
	}
	cc, err := a.Creator.CreateCluster(r.Context(), req)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	log.FromContext(r.Context()).Info("container cluster creation observed", "subject", subject, "operationID", req.OperationID, "namespace", cc.Namespace, "name", cc.Name, "uid", cc.UID)
	// Return durable identity without an additional read that could fail after create.
	writeJSON(w, 201, struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		UID       string `json:"uid"`
	}{cc.Namespace, cc.Name, string(cc.UID)})
}

type patchClusterRequest struct {
	UID                string  `json:"uid"`
	ResourceVersion    string  `json:"resourceVersion"`
	TargetAgentRelease *string `json:"targetAgentRelease"`
	ReleaseUID         string  `json:"releaseUID"`
}

func (a *API) patchCluster(w http.ResponseWriter, r *http.Request, namespace, name, subject string) {
	var req patchClusterRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.UID == "" || req.ResourceVersion == "" || req.TargetAgentRelease == nil || (*req.TargetAgentRelease != "" && req.ReleaseUID == "") || (*req.TargetAgentRelease == "" && req.ReleaseUID != "") {
		writeError(w, 400, "invalid_request", "exact cluster and release identity required")
		return
	}
	var cc ext.ContainerCluster
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &cc); err != nil {
		writeKubeError(w, err)
		return
	}
	nc, valid, err := a.binding(r.Context(), &cc)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	if !valid || !cc.DeletionTimestamp.IsZero() || nc == nil || !nc.DeletionTimestamp.IsZero() || string(cc.UID) != req.UID || cc.ResourceVersion != req.ResourceVersion {
		writeError(w, 409, "conflict", "cluster changed or binding is invalid; refresh before retrying")
		return
	}
	if *req.TargetAgentRelease != "" {
		var release ext.AgentRelease
		if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: *req.TargetAgentRelease}, &release); err != nil {
			writeKubeError(w, err)
			return
		}
		if string(release.UID) != req.ReleaseUID || tenant.IDOf(&release) != tenant.IDOf(&cc) || !available(&release) {
			writeError(w, 409, "conflict", "release changed or is unavailable; refresh before retrying")
			return
		}
	}
	before := cc.DeepCopy()
	cc.Spec.TargetAgentRelease = *req.TargetAgentRelease
	if err := a.Client.Patch(r.Context(), &cc, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		writeKubeError(w, err)
		return
	}
	log.FromContext(r.Context()).Info("container cluster target updated", "subject", subject, "namespace", cc.Namespace, "name", cc.Name, "uid", cc.UID, "target", cc.Spec.TargetAgentRelease)
	writeJSON(w, 200, struct {
		ResourceVersion string `json:"resourceVersion"`
	}{cc.ResourceVersion})
}
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, 400, "invalid_request", "invalid JSON request")
		return false
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, 400, "invalid_request", "exactly one JSON value required")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "response unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		return
	} // Client disconnect: a second response cannot be sent.
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message})
}
func writeKubeError(w http.ResponseWriter, err error) {
	switch {
	case apierrors.IsNotFound(err):
		writeError(w, 404, "not_found", "resource not found")
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err), errors.Is(err, provision.ErrConflict):
		writeError(w, 409, "conflict", "resource changed or ownership conflicts; refresh before retrying")
	default:
		writeError(w, 503, "unavailable", "management service unavailable")
	}
}
