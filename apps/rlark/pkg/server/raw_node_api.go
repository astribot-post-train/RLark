package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/gin-gonic/gin"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// This is a mediated Kubernetes-shaped subset, not an administrative proxy.
// Queries are read from the uncached management API and filtered before return.
// Watches, discovery, secrets and arbitrary resources/verbs are unsupported.
func (s *Server) handleRawNodeAPI(c *gin.Context) {
	i, node, ok := s.rawNodeIdentity(c)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(c.Param("path"), "/"), "/")
	if len(parts) < 4 || parts[0] != "apis" || parts[1] != api.GroupVersion.Group || parts[2] != api.GroupVersion.Version {
		rawDenied(c)
		return
	}
	parts = parts[3:]
	namespace := ""
	if len(parts) >= 2 && parts[0] == "namespaces" {
		namespace = parts[1]
		parts = parts[2:]
	}
	if len(parts) < 1 || len(parts) > 3 {
		rawDenied(c)
		return
	}
	resource := parts[0]
	name := ""
	sub := ""
	if len(parts) > 1 {
		name = parts[1]
	}
	if len(parts) > 2 {
		sub = parts[2]
	}
	if (resource == "nodes" || resource == "tasks") && namespace != i.Namespace {
		rawDenied(c)
		return
	}
	if (resource == "jobs" || resource == "clusters") && namespace != "" {
		rawDenied(c)
		return
	}
	if c.Request.URL.Query().Get("watch") != "" && c.Request.URL.Query().Get("watch") != "false" {
		rawDenied(c)
		return
	}
	if c.Request.Method == http.MethodGet && sub == "" {
		s.readRawResource(c, i, node, resource, name)
		return
	}
	if c.Request.Method == http.MethodPatch && resource == "tasks" && name != "" && (sub == "" || sub == "status") {
		s.patchRawTask(c, i, name, sub)
		return
	}
	rawDenied(c)
}

func rawDenied(c *gin.Context) {
	rawAPIError(c, apierrors.NewForbidden(schema.GroupResource{Group: api.GroupVersion.Group}, "", errors.New("Raw node operation is not authorized")))
}
func rawAPIError(c *gin.Context, err error) {
	if status, ok := err.(apierrors.APIStatus); ok {
		v := status.Status()
		v.APIVersion, v.Kind = "v1", "Status"
		c.JSON(int(v.Code), v)
		return
	}
	c.JSON(http.StatusServiceUnavailable, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: http.StatusServiceUnavailable, Reason: metav1.StatusReasonServiceUnavailable, Message: "Raw management API unavailable"})
}
func rawTaskAssigned(t *api.Task, i rawnode.Identity) bool {
	b := t.Spec.Placement.Binding
	owner := metav1.GetControllerOf(t)
	return t.Namespace == i.Namespace && t.UID != "" && tenant.IDOf(t) == i.TenantID && t.Spec.Runtime.Kind == "Raw" && b != nil && b.TaskUID == string(t.UID) && b.ClusterUID == i.ClusterUID && owner != nil && owner.Kind == "Job" && owner.APIVersion == api.GroupVersion.String() && string(owner.UID) == b.JobUID && t.Annotations[utils.ParentUIDAnnotation] == b.JobUID
}
func rawOwnTask(t *api.Task, i rawnode.Identity) bool {
	return rawTaskAssigned(t, i) && t.Spec.Placement.Binding.NodeName == i.Node && t.Spec.Placement.Binding.NodeUID == i.NodeUID && t.Spec.Placement.Binding.Incarnation == i.Incarnation
}
func (s *Server) readRawResource(c *gin.Context, i rawnode.Identity, node *api.Node, resource, name string) {
	ctx := c.Request.Context()
	switch resource {
	case "nodes":
		list, err := s.rlarkClient.RlinfV1alpha1().Nodes(i.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			rawAPIError(c, err)
			return
		}
		items := []api.Node{}
		for _, n := range list.Items {
			if tenant.IDOf(&n) == i.TenantID && n.Spec.AgentType == api.AgentTypeRaw {
				items = append(items, n)
			}
		}
		if name != "" {
			for _, n := range items {
				if n.Name == name {
					n.APIVersion = api.GroupVersion.String()
					n.Kind = "Node"
					c.JSON(http.StatusOK, n)
					return
				}
			}
			rawDenied(c)
			return
		}
		list.Items = items
		list.APIVersion = api.GroupVersion.String()
		list.Kind = "NodeList"
		c.JSON(http.StatusOK, list)
	case "clusters":
		list, err := s.rlarkClient.RlinfV1alpha1().Clusters().List(ctx, metav1.ListOptions{})
		if err != nil {
			rawAPIError(c, err)
			return
		}
		items := []api.Cluster{}
		for _, v := range list.Items {
			if v.Name == i.Cluster && string(v.UID) == i.ClusterUID {
				items = append(items, v)
			}
		}
		if name != "" {
			if len(items) == 1 && name == i.Cluster {
				v := items[0]
				v.APIVersion = api.GroupVersion.String()
				v.Kind = "Cluster"
				c.JSON(http.StatusOK, v)
				return
			}
			rawDenied(c)
			return
		}
		list.Items = items
		list.APIVersion = api.GroupVersion.String()
		list.Kind = "ClusterList"
		c.JSON(http.StatusOK, list)
	case "tasks":
		list, err := s.rlarkClient.RlinfV1alpha1().Tasks(i.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			rawAPIError(c, err)
			return
		}
		items := []api.Task{}
		for _, t := range list.Items {
			// Own Tasks remain readable for cleanup after reservation release. Peer
			// Tasks are visible only within the current committed allocation group.
			if rawOwnTask(&t, i) || (rawTaskAssigned(&t, i) && node.Status.Reservation != nil && t.Spec.Placement.Binding.JobUID == node.Status.Reservation.JobUID && t.Spec.Placement.Binding.ExecutionID == node.Status.Reservation.ExecutionID && t.Spec.Placement.Binding.GroupDigest == node.Status.Reservation.GroupDigest) {
				items = append(items, t)
			}
		}
		if name != "" {
			for _, v := range items {
				if v.Name == name {
					v.APIVersion = api.GroupVersion.String()
					v.Kind = "Task"
					c.JSON(http.StatusOK, v)
					return
				}
			}
			rawDenied(c)
			return
		}
		list.Items = items
		list.APIVersion = api.GroupVersion.String()
		list.Kind = "TaskList"
		c.JSON(http.StatusOK, list)
	case "jobs":
		if name == "" || node.Status.Reservation == nil {
			rawDenied(c)
			return
		}
		job, err := s.rlarkClient.RlinfV1alpha1().Jobs().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			rawAPIError(c, err)
			return
		}
		if string(job.UID) != node.Status.Reservation.JobUID || tenant.IDOf(job) != i.TenantID {
			rawDenied(c)
			return
		}
		job = job.DeepCopy()
		job.APIVersion = api.GroupVersion.String()
		job.Kind = "Job"
		c.JSON(http.StatusOK, job)
	default:
		rawDenied(c)
	}
}

func (s *Server) patchRawTask(c *gin.Context, i rawnode.Identity, name, sub string) {
	tasks := s.rlarkClient.RlinfV1alpha1().Tasks(i.Namespace)
	current, err := tasks.Get(c.Request.Context(), name, metav1.GetOptions{})
	if err != nil {
		rawAPIError(c, err)
		return
	}
	if !rawOwnTask(current, i) {
		rawDenied(c)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err != nil {
		rawDenied(c)
		return
	}
	if !rawPatchVersion(c.ContentType(), data, current.ResourceVersion) {
		rawAPIError(c, apierrors.NewConflict(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "tasks"}, name, errors.New("explicit current resourceVersion required")))
		return
	}
	original, err := json.Marshal(current)
	if err != nil {
		rawAPIError(c, err)
		return
	}
	var updated []byte
	switch c.ContentType() {
	case string(types.MergePatchType):
		updated, err = jsonpatch.MergePatch(original, data)
	case string(types.JSONPatchType):
		var p jsonpatch.Patch
		p, err = jsonpatch.DecodePatch(data)
		if err == nil {
			updated, err = p.Apply(original)
		}
	default:
		rawDenied(c)
		return
	}
	if err != nil {
		rawAPIError(c, apierrors.NewConflict(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "tasks"}, name, errors.New("Task patch precondition failed")))
		return
	}
	var desired api.Task
	decoder := json.NewDecoder(bytes.NewReader(updated))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&desired); err != nil {
		rawDenied(c)
		return
	}
	// Every write carries resourceVersion; a stale status cannot acknowledge a
	// newer desired generation, rebind a Task, or remove somebody else's finalizer.
	if desired.ResourceVersion == "" || desired.ResourceVersion != current.ResourceVersion || desired.UID != current.UID {
		rawDenied(c)
		return
	}
	if sub == "status" {
		allowed := current.DeepCopy()
		allowed.Status.Phase = desired.Status.Phase
		allowed.Status.Message = desired.Status.Message
		allowed.Status.ObservedGeneration = desired.Status.ObservedGeneration
		if !validRawConditions(current, &desired) {
			rawDenied(c)
			return
		}
		allowed.Status.Conditions = desired.Status.Conditions
		allowed.Status.ObservedNodes = desired.Status.ObservedNodes
		if desired.Status.Phase == api.TaskPhaseStopped {
			allowed.Status.Events = nil
			allowed.Status.PullProgress = nil
		}
		if !equality.Semantic.DeepEqual(allowed, &desired) || desired.Status.ObservedGeneration != current.Generation || !validRawTaskStatus(&desired, i) {
			rawDenied(c)
			return
		}
		result, e := tasks.UpdateStatus(c.Request.Context(), &desired, metav1.UpdateOptions{})
		if e != nil {
			rawAPIError(c, e)
			return
		}
		result.APIVersion = api.GroupVersion.String()
		result.Kind = "Task"
		c.JSON(http.StatusOK, result)
		return
	}
	allowed := current.DeepCopy()
	allowed.Annotations = desired.Annotations
	allowed.Finalizers = desired.Finalizers
	if !equality.Semantic.DeepEqual(allowed, &desired) || !validRawClaimMutation(current, &desired, i) {
		rawDenied(c)
		return
	}
	result, e := tasks.Update(c.Request.Context(), &desired, metav1.UpdateOptions{})
	if e != nil {
		rawAPIError(c, e)
		return
	}
	result.APIVersion = api.GroupVersion.String()
	result.Kind = "Task"
	c.JSON(http.StatusOK, result)
}
func validRawTaskStatus(t *api.Task, i rawnode.Identity) bool {
	switch t.Status.Phase {
	case "", api.TaskPhasePending, api.TaskPhaseRunning, api.TaskPhaseSucceeded, api.TaskPhaseFailed, api.TaskPhaseStopped:
	default:
		return false
	}
	for _, n := range t.Status.ObservedNodes {
		if n != i.Node {
			return false
		}
	}
	for _, v := range t.Status.Conditions {
		if v.Type == "ExecutionKnown" && (v.ObservedGeneration != t.Generation || (v.Status != metav1.ConditionTrue && v.Status != metav1.ConditionFalse && v.Status != metav1.ConditionUnknown) || v.Reason == "" || v.LastTransitionTime.IsZero()) {
			return false
		}
	}
	return true
}

// rawPatchVersion requires a caller-observed version, never one filled in by
// merging the request into a fresh server read. A retry must reread the Task.
func rawPatchVersion(contentType string, data []byte, version string) bool {
	if version == "" {
		return false
	}
	switch contentType {
	case string(types.MergePatchType):
		var p struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		return json.Unmarshal(data, &p) == nil && p.Metadata.ResourceVersion == version
	case string(types.JSONPatchType):
		var p []struct {
			Op    string          `json:"op"`
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(data, &p) != nil || len(p) == 0 || p[0].Op != "test" || p[0].Path != "/metadata/resourceVersion" {
			return false
		}
		var v string
		return json.Unmarshal(p[0].Value, &v) == nil && v == version
	}
	return false
}

func validRawConditions(old, next *api.Task) bool {
	// Only ExecutionKnown belongs to this Agent. Preserve controller conditions.
	withoutOwned := func(in []metav1.Condition) []metav1.Condition {
		return slices.DeleteFunc(slices.Clone(in), func(v metav1.Condition) bool { return v.Type == "ExecutionKnown" })
	}
	if !slices.Equal(withoutOwned(old.Status.Conditions), withoutOwned(next.Status.Conditions)) {
		return false
	}
	count := 0
	for _, v := range next.Status.Conditions {
		if v.Type == "ExecutionKnown" {
			count++
		}
	}
	return count <= 1
}

func validRawClaimMutation(old, next *api.Task, i rawnode.Identity) bool {
	claimant := i.Namespace + "/Raw/" + i.NodeUID
	if v := old.Annotations[host.TaskClaimantAnnotation]; v != "" && v != claimant {
		return false
	}
	keys := map[string]string{
		host.TaskClaimantAnnotation:               claimant,
		host.TaskClaimedWorkloadTypeAnnotation:    string(old.Spec.Workload.Kind),
		host.TaskClaimedWorkloadVersionAnnotation: old.Spec.Workload.Version,
		host.TaskClaimedCarrierTypeAnnotation:     old.Spec.Workload.Carrier.Type,
		host.TaskClaimedCarrierVersionAnnotation:  old.Spec.Workload.Carrier.Version,
	}
	// Frozen selectors can only be set once, or removed on deletion after cleanup.
	releasing := !old.DeletionTimestamp.IsZero() && slices.Contains(old.Finalizers, host.TaskFinalizer) && !slices.Contains(next.Finalizers, host.TaskFinalizer)
	expected := old.DeepCopy()
	if expected.Annotations == nil {
		expected.Annotations = map[string]string{}
	}
	for key, value := range keys {
		if releasing {
			delete(expected.Annotations, key)
			continue
		}
		if frozen := old.Annotations[key]; frozen != "" {
			value = frozen
		}
		if next.Annotations[key] != value {
			return false
		}
		expected.Annotations[key] = value
	}
	// An empty annotations map is serialized identically to nil.
	if len(expected.Annotations) == 0 {
		expected.Annotations = nil
	}
	annotations := next.Annotations
	if len(annotations) == 0 {
		annotations = nil
	}
	if !reflect.DeepEqual(expected.Annotations, annotations) {
		return false
	}
	before := slices.DeleteFunc(slices.Clone(old.Finalizers), func(v string) bool { return v == host.TaskFinalizer })
	after := slices.DeleteFunc(slices.Clone(next.Finalizers), func(v string) bool { return v == host.TaskFinalizer })
	if !slices.Equal(before, after) || len(next.Finalizers)-len(after) > 1 {
		return false
	}
	if slices.Contains(old.Finalizers, host.TaskFinalizer) && !slices.Contains(next.Finalizers, host.TaskFinalizer) && !releasing {
		return false
	}
	// A claim is durable before effects; never create a claimant-less finalizer.
	return releasing || slices.Contains(next.Finalizers, host.TaskFinalizer)
}
