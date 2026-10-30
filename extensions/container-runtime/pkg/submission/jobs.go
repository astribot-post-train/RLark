// Package submission implements administrator Raw Job submission independently
// of workload scheduling. Native Jobs and exact-UID bindings are its receipts.
package submission

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	"github.com/google/uuid"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	operationAnnotation   = ext.Group + "/submission-operation"
	fingerprintAnnotation = ext.Group + "/submission-request-sha256"
	clusterAnnotation     = ext.Group + "/submission-cluster-uid"
	activatedAnnotation   = ext.Group + "/submission-activated"
)

var (
	// ErrConflict fences stale identity, ambiguous ownership and changed input.
	ErrConflict = errors.New("container Job submission conflict")
	// ErrInvalid rejects requests before any object is written.
	ErrInvalid = errors.New("invalid container Job submission")
)

// ClusterTarget is an exact companion instance; native scope and tenant are
// derived server-side rather than accepted as submission input.
type ClusterTarget struct{ Namespace, Name, UID string }

// Request describes the sole supported single-process workload. Environment
// selects a registered profile; Process.Env is explicit, never inherited.
type Request struct {
	OperationID string          `json:"operationID"`
	ClusterUID  string          `json:"clusterUID"`
	Environment string          `json:"environment"`
	Process     raw.ProcessSpec `json:"process"`
}

// Validate rejects unsupported identity, process and user GPU overrides.
func (r Request) Validate() error {
	id, err := uuid.Parse(r.OperationID)
	if err != nil || id == uuid.Nil || id.String() != r.OperationID || r.ClusterUID == "" || r.Environment == "" || strings.TrimSpace(r.Environment) != r.Environment || len(r.Environment) > 128 {
		return ErrInvalid
	}
	if err := r.Process.Validate(); err != nil {
		return fmt.Errorf("%w: invalid process paths or environment", ErrInvalid)
	}
	for _, env := range r.Process.Env {
		key, _, _ := strings.Cut(env, "=")
		if key == "CUDA_VISIBLE_DEVICES" || key == "NVIDIA_VISIBLE_DEVICES" {
			return ErrInvalid
		}
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 64<<10 {
		return ErrInvalid
	}
	return nil
}

// Summary never exposes process argv, environment, or credentials.
type Summary struct {
	Name            string          `json:"name"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	BindingUID      string          `json:"bindingUID"`
	OperationID     string          `json:"operationID"`
	Phase           native.JobPhase `json:"phase"`
	Observed        bool            `json:"observed"`
	Suspended       bool            `json:"suspended"`
	Activated       bool            `json:"activated"`
	BindingValid    bool            `json:"bindingValid"`
}

// Service requires an uncached client. It never creates allocations, executes
// processes, changes observed status, or deletes a resource.
type Service struct{ Client client.Client }

func (s Service) cluster(ctx context.Context, t ClusterTarget) (*ext.ContainerCluster, error) {
	if s.Client == nil || t.Namespace == "" || t.Name == "" || t.UID == "" {
		return nil, ErrConflict
	}
	var cc ext.ContainerCluster
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, &cc); err != nil {
		return nil, err
	}
	if string(cc.UID) != t.UID || cc.Spec.Runtime != "Raw" || !cc.DeletionTimestamp.IsZero() || tenant.IDOf(&cc) == "" {
		return nil, ErrConflict
	}
	var nc native.Cluster
	if err := s.Client.Get(ctx, client.ObjectKey{Name: cc.Spec.Cluster.Name}, &nc); err != nil {
		return nil, err
	}
	if ownership.MatchesNative(cc.Spec.Cluster, &nc) != nil || !nc.DeletionTimestamp.IsZero() || nc.Spec.AgentNamespace != cc.Namespace || cc.Spec.AgentNamespace != cc.Namespace || tenant.IDOf(&nc) != tenant.IDOf(&cc) {
		return nil, ErrConflict
	}
	var natives native.ClusterList
	if err := s.Client.List(ctx, &natives); err != nil {
		return nil, err
	}
	for _, n := range natives.Items {
		if n.UID != nc.UID && n.Spec.AgentNamespace == cc.Namespace {
			return nil, ErrConflict
		}
	}
	var companions ext.ContainerClusterList
	if err := s.Client.List(ctx, &companions); err != nil {
		return nil, err
	}
	for _, c := range companions.Items {
		if c.UID != cc.UID && (c.Namespace == cc.Namespace || c.Spec.Cluster == cc.Spec.Cluster) {
			return nil, ErrConflict
		}
	}
	return &cc, nil
}

func (s Service) environment(ctx context.Context, cc *ext.ContainerCluster, profile string) error {
	var nodes ext.ContainerNodeList
	if err := s.Client.List(ctx, &nodes, client.InNamespace(cc.Namespace)); err != nil {
		return err
	}
	for _, n := range nodes.Items {
		if n.UID == "" || n.Spec.Runtime != "Raw" || n.Spec.Cluster != cc.Spec.Cluster || n.Spec.Environment != profile || n.Spec.Node.Namespace != cc.Namespace || len(n.Spec.AuthorizedGPUUUIDs) == 0 || tenant.IDOf(&n) != tenant.IDOf(cc) || !n.DeletionTimestamp.IsZero() {
			continue
		}
		var nn native.Node
		if err := s.Client.Get(ctx, client.ObjectKey{Namespace: n.Spec.Node.Namespace, Name: n.Spec.Node.Name}, &nn); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if ownership.MatchesNative(n.Spec.Node, &nn) == nil && tenant.IDOf(&nn) == tenant.IDOf(cc) && nn.Spec.AgentType == native.AgentTypeRaw && nn.DeletionTimestamp.IsZero() {
			return nil
		}
	}
	return fmt.Errorf("%w: no registered Raw environment", ErrInvalid)
}

func desired(cc *ext.ContainerCluster, req Request) (*native.Job, string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(data))
	params, err := json.Marshal(req.Process)
	if err != nil {
		return nil, "", err
	}
	name := "raw-" + req.OperationID
	job := &native.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{tenant.IDLabel: tenant.IDOf(cc)}, Annotations: map[string]string{nativejob.ControllerOwnerAnnotation: ext.Group, operationAnnotation: req.OperationID, fingerprintAnnotation: fingerprint, clusterAnnotation: string(cc.UID)}}, Spec: native.JobSpec{Suspended: true, Tasks: []native.JobTaskTemplate{{Name: "worker", TaskSpec: native.TaskSpec{Role: "worker", Runtime: native.TaskRuntimeSpec{Kind: "Raw"}, Placement: native.TaskPlacementSpec{Environment: req.Environment}, Workload: native.TaskWorkloadSpec{Kind: native.WorkloadKind(raw.ProcessWorkload), Version: "v1", Carrier: native.TaskWorkloadCarrier{Type: string(raw.ProcessWorkload), Version: "v1"}, Parameters: apiextensions.JSON{Raw: params}}}}}}}
	return job, fingerprint, nil
}

func matchIntent(job, wanted *native.Job) bool {
	if job.UID == "" || !job.DeletionTimestamp.IsZero() || tenant.IDOf(job) != tenant.IDOf(wanted) || job.Annotations[nativejob.ControllerOwnerAnnotation] != ext.Group {
		return false
	}
	for _, key := range []string{operationAnnotation, fingerprintAnnotation, clusterAnnotation} {
		if job.Annotations[key] != wanted.Annotations[key] {
			return false
		}
	}
	// Suspension is independently owned durable intent; an identical submission
	// may observe it but must never undo a subsequent administrator stop.
	a, b := job.Spec, wanted.Spec
	a.Suspended, b.Suspended = false, false
	if len(a.Tasks) != 1 || len(b.Tasks) != 1 {
		return false
	}
	if !taskcontract.EqualSpec(a.Tasks[0].TaskSpec, b.Tasks[0].TaskSpec) {
		return false
	}
	a.Tasks, b.Tasks = nil, nil
	return reflect.DeepEqual(a, b) && job.Spec.Tasks[0].Name == wanted.Spec.Tasks[0].Name && job.Spec.Tasks[0].Head == wanted.Spec.Tasks[0].Head
}

func reference(o client.Object) ext.NativeObjectReference {
	return ext.NativeObjectReference{Name: o.GetName(), Namespace: o.GetNamespace(), UID: string(o.GetUID())}
}

// Submit creates a suspended Job, pins the binding, then records activation in
// the same CAS patch that unsuspends. Lost responses recover exact identities;
// later suspension/completion is observed without replaying activation.
func (s Service) Submit(ctx context.Context, t ClusterTarget, req Request) (Summary, error) {
	if err := req.Validate(); err != nil {
		return Summary{}, err
	}
	if req.ClusterUID != t.UID {
		return Summary{}, ErrConflict
	}
	cc, err := s.cluster(ctx, t)
	if err != nil {
		return Summary{}, err
	}
	wanted, fp, err := desired(cc, req)
	if err != nil {
		return Summary{}, err
	}
	// An orphaned retained binding fences recreation even before a Job read.
	var bindings ext.ContainerJobList
	if err := s.Client.List(ctx, &bindings); err != nil {
		return Summary{}, err
	}
	var retained *ext.ContainerJob
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if b.Spec.Job.Name != wanted.Name || b.Spec.Job.Namespace != "" {
			continue
		}
		if retained != nil || b.Namespace != cc.Namespace || b.Name != wanted.Name || b.Spec.Cluster != cc.Spec.Cluster || tenant.IDOf(b) != tenant.IDOf(cc) || b.Spec.TenantID != tenant.IDOf(cc) || b.Spec.Runtime != "Raw" || b.Annotations[operationAnnotation] != req.OperationID || b.Annotations[fingerprintAnnotation] != fp || b.Annotations[clusterAnnotation] != string(cc.UID) || !b.DeletionTimestamp.IsZero() {
			return Summary{}, ErrConflict
		}
		retained = b
	}
	var job native.Job
	err = s.Client.Get(ctx, client.ObjectKeyFromObject(wanted), &job)
	if apierrors.IsNotFound(err) {
		if retained != nil {
			return Summary{}, ErrConflict
		}
		if err := s.environment(ctx, cc, req.Environment); err != nil {
			return Summary{}, err
		}
		if createErr := s.Client.Create(ctx, wanted); createErr != nil {
			if readErr := s.Client.Get(ctx, client.ObjectKeyFromObject(wanted), &job); readErr != nil {
				return Summary{}, fmt.Errorf("create suspended Job: %w", createErr)
			}
		} else {
			job = *wanted
		}
	} else if err != nil {
		return Summary{}, err
	}
	if !matchIntent(&job, wanted) || retained != nil && retained.Spec.Job != reference(&job) {
		return Summary{}, ErrConflict
	}
	binding := &ext.ContainerJob{ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: cc.Namespace, Labels: map[string]string{tenant.IDLabel: tenant.IDOf(cc)}, Annotations: map[string]string{operationAnnotation: req.OperationID, fingerprintAnnotation: fp, clusterAnnotation: string(cc.UID)}}, Spec: ext.ContainerJobSpec{Cluster: cc.Spec.Cluster, Job: reference(&job), TenantID: tenant.IDOf(cc), Runtime: "Raw"}}
	if retained == nil {
		if err := s.Client.Create(ctx, binding); err != nil {
			var current ext.ContainerJob
			if readErr := s.Client.Get(ctx, client.ObjectKeyFromObject(binding), &current); readErr != nil {
				return Summary{}, fmt.Errorf("create Job binding: %w", err)
			}
			if !reflect.DeepEqual(current.Spec, binding.Spec) || current.UID == "" || !current.DeletionTimestamp.IsZero() || tenant.IDOf(&current) != tenant.IDOf(cc) {
				return Summary{}, ErrConflict
			}
			for _, key := range []string{operationAnnotation, fingerprintAnnotation, clusterAnnotation} {
				if current.Annotations[key] != binding.Annotations[key] {
					return Summary{}, ErrConflict
				}
			}
			binding = &current
		}
	} else {
		binding = retained
	}
	// Recheck both retained instances after possible creation/racing suspension.
	if _, err := s.cluster(ctx, t); err != nil {
		return Summary{}, err
	}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(wanted), &job); err != nil {
		return Summary{}, err
	}
	if !matchIntent(&job, wanted) || binding.Spec.Job != reference(&job) {
		return Summary{}, ErrConflict
	}
	verified, err := s.bound(ctx, cc, binding)
	if err != nil || verified.UID != job.UID {
		if err != nil {
			return Summary{}, err
		}
		return Summary{}, ErrConflict
	}
	if job.Annotations[activatedAnnotation] == "" {
		if !job.Spec.Suspended || job.Status.Phase == native.JobPhaseRunning || job.Status.Phase == native.JobPhaseSucceeded || job.Status.Phase == native.JobPhaseFailed {
			return Summary{}, ErrConflict
		}
		patch, err := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(job.UID)}, {"op": "test", "path": "/metadata/resourceVersion", "value": job.ResourceVersion}, {"op": "add", "path": "/metadata/annotations/containers.rlark.io~1submission-activated", "value": "true"}, {"op": "add", "path": "/spec/suspended", "value": false}})
		if err != nil {
			return Summary{}, err
		}
		if err := s.patchIntent(ctx, &job, patch); err != nil {
			return Summary{}, err
		}
	}
	return summarize(&job, binding), nil
}

func summarize(j *native.Job, b *ext.ContainerJob) Summary {
	return Summary{Name: j.Name, UID: string(j.UID), ResourceVersion: j.ResourceVersion, BindingUID: string(b.UID), OperationID: j.Annotations[operationAnnotation], Phase: j.Status.Phase, Observed: j.Status.ObservedGeneration == j.Generation, Suspended: j.Spec.Suspended, Activated: j.Annotations[activatedAnnotation] != "", BindingValid: true}
}

func (s Service) bound(ctx context.Context, cc *ext.ContainerCluster, b *ext.ContainerJob) (*native.Job, error) {
	var current ext.ContainerJob
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(b), &current); err != nil {
		return nil, err
	}
	if current.UID != b.UID || !reflect.DeepEqual(current.Spec, b.Spec) || tenant.IDOf(&current) != tenant.IDOf(b) || !current.DeletionTimestamp.IsZero() {
		return nil, ErrConflict
	}
	if b.UID == "" || b.Namespace != cc.Namespace || b.Spec.Cluster != cc.Spec.Cluster || b.Spec.Job.Namespace != "" || b.Spec.Job.UID == "" || b.Spec.TenantID != tenant.IDOf(cc) || tenant.IDOf(b) != tenant.IDOf(cc) || b.Spec.Runtime != "Raw" || !b.DeletionTimestamp.IsZero() {
		return nil, ErrConflict
	}
	var all ext.ContainerJobList
	if err := s.Client.List(ctx, &all); err != nil {
		return nil, err
	}
	for _, other := range all.Items {
		if other.UID != b.UID && other.Spec.Job.Name == b.Spec.Job.Name && other.Spec.Job.Namespace == "" {
			return nil, ErrConflict
		}
	}
	var j native.Job
	if err := s.Client.Get(ctx, client.ObjectKey{Name: b.Spec.Job.Name}, &j); err != nil {
		return nil, err
	}
	if reference(&j) != b.Spec.Job || tenant.IDOf(&j) != tenant.IDOf(cc) || j.Annotations[nativejob.ControllerOwnerAnnotation] != ext.Group || !j.DeletionTimestamp.IsZero() {
		return nil, ErrConflict
	}
	return &j, nil
}

// List returns only verified bound identities; unresolved bindings are visible
// as invalid without leaking a replacement Job's state or payload.
func (s Service) List(ctx context.Context, t ClusterTarget) ([]Summary, error) {
	cc, err := s.cluster(ctx, t)
	if err != nil {
		return nil, err
	}
	var bindings ext.ContainerJobList
	if err := s.Client.List(ctx, &bindings, client.InNamespace(cc.Namespace)); err != nil {
		return nil, err
	}
	out := []Summary{}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if b.Spec.Cluster != cc.Spec.Cluster || tenant.IDOf(b) != tenant.IDOf(cc) || b.Spec.TenantID != tenant.IDOf(cc) {
			continue
		}
		j, err := s.bound(ctx, cc, b)
		if err != nil {
			if errors.Is(err, ErrConflict) || apierrors.IsNotFound(err) {
				out = append(out, Summary{Name: b.Spec.Job.Name, BindingUID: string(b.UID)})
				continue
			}
			return nil, err
		}
		out = append(out, summarize(j, b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Suspend is durable intent, not an edge-triggered stop command. It also seals
// a pending submission so a delayed identical Submit cannot activate it.
func (s Service) Suspend(ctx context.Context, t ClusterTarget, bindingName, uid, version string) (Summary, error) {
	if uid == "" || version == "" {
		return Summary{}, ErrInvalid
	}
	cc, err := s.cluster(ctx, t)
	if err != nil {
		return Summary{}, err
	}
	var bindings ext.ContainerJobList
	if err := s.Client.List(ctx, &bindings, client.InNamespace(cc.Namespace)); err != nil {
		return Summary{}, err
	}
	var b *ext.ContainerJob
	for i := range bindings.Items {
		candidate := &bindings.Items[i]
		if candidate.Spec.Job.Name == bindingName && candidate.Spec.Job.Namespace == "" {
			if b != nil {
				return Summary{}, ErrConflict
			}
			b = candidate
		}
	}
	if b == nil {
		return Summary{}, ErrConflict
	}
	j, err := s.bound(ctx, cc, b)
	if err != nil {
		return Summary{}, err
	}
	if string(j.UID) != uid || j.ResourceVersion != version {
		return Summary{}, ErrConflict
	}
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": uid}, {"op": "test", "path": "/metadata/resourceVersion", "value": version}, {"op": "add", "path": "/spec/suspended", "value": true}}
	if j.Annotations[operationAnnotation] != "" {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations/containers.rlark.io~1submission-activated", "value": "stopped"})
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return Summary{}, err
	}
	if err := s.patchIntent(ctx, j, data); err != nil {
		return Summary{}, err
	}
	return summarize(j, b), nil
}

func (s Service) patchIntent(ctx context.Context, j *native.Job, patch []byte) error {
	uid, version := j.UID, j.ResourceVersion
	if err := s.Client.Patch(ctx, j, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		// JSON Patch precondition failures vary between management APIs. Prove
		// replacement/version drift before classifying a failed CAS as conflict.
		var current native.Job
		if readErr := s.Client.Get(ctx, client.ObjectKeyFromObject(j), &current); readErr == nil && (current.UID != uid || current.ResourceVersion != version) {
			return errors.Join(ErrConflict, err)
		}
		return err
	}
	return nil
}
