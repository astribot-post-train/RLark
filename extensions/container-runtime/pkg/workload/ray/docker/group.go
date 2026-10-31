package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strings"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// MaxMembers bounds scheduling work and protocol snapshots.
	MaxMembers = 16
	// Capability identifies the prepare/start group protocol.
	Capability = "docker-ray-group-v1"
	// RankAnnotation is controller-owned logical replica identity.
	RankAnnotation = ext.Group + "/group-rank"
)

// Member is a validated logical replica before API object creation.
type Member struct {
	Template string
	Name     string
	Index    int32
	Rank     int32
	Head     bool
	Spec     native.TaskSpec
}

// IsJob selects group semantics even for malformed mixed-runtime Jobs.
func IsJob(job *native.Job) bool {
	if job != nil {
		for _, t := range job.Spec.Tasks {
			if t.Runtime.Kind == "Docker" && t.Workload.Kind == taskapi.WorkloadTypeRay {
				return true
			}
		}
	}
	return false
}

// Expand validates the whole Job and consumes template-level replica counts.
// It never returns a partial set or mutates the input.
func Expand(job *native.Job) ([]Member, error) {
	if job == nil || job.Name == "" || len(job.Spec.Tasks) == 0 || job.Spec.Domain != "" || job.Spec.SSHPublicKey != "" {
		return nil, errors.New("ray group requires templates without virtual network or SSH injection")
	}
	var members []Member
	seenTemplates, seenNames := map[string]bool{}, map[string]bool{}
	headCount := 0
	for _, t := range job.Spec.Tasks {
		if len(validation.IsDNS1123Label(t.Name)) != 0 || seenTemplates[t.Name] || t.Suspended {
			return nil, errors.New("ray templates require unique DNS labels and cannot be suspended independently")
		}
		seenTemplates[t.Name] = true
		p := taskapi.DockerReplicatedCarrierParametersV1{}
		if len(t.Workload.Carrier.Parameters.Raw) != 0 {
			var err error
			p, err = taskapi.DecodeParametersStrict[taskapi.DockerReplicatedCarrierParametersV1](t.Workload.Carrier.Parameters)
			if err != nil {
				return nil, fmt.Errorf("decode Ray replicas: %w", err)
			}
		}
		count := int32(1)
		if p.Replicas != nil {
			count = *p.Replicas
		}
		if count < 1 || count > MaxMembers || len(members)+int(count) > MaxMembers || t.Head && count != 1 {
			return nil, errors.New("ray group supports 1..16 members and one head replica")
		}
		if t.Head {
			headCount++
		}
		spec := *t.TaskSpec.DeepCopy()
		spec.Workload.Carrier.Parameters = apiextensionsv1.JSON{}
		if err := ValidateUserTask(spec, t.Head); err != nil {
			return nil, fmt.Errorf("ray template %q: %w", t.Name, err)
		}
		spec.Tags = append(slices.Clone(job.Spec.Tags), spec.Tags...)
		for i := int32(0); i < count; i++ {
			name := utils.ChildName(job.Name, fmt.Sprintf("%s-%d", t.Name, i))
			if len(validation.IsDNS1123Subdomain(name)) != 0 || seenNames[name] {
				return nil, errors.New("ray child names are invalid or collide")
			}
			seenNames[name] = true
			members = append(members, Member{Template: t.Name, Name: name, Index: i, Head: t.Head, Spec: *spec.DeepCopy()})
		}
	}
	if headCount != 1 {
		return nil, errors.New("ray group requires exactly one head")
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Head != members[j].Head {
			return members[i].Head
		}
		if members[i].Template != members[j].Template {
			return members[i].Template < members[j].Template
		}
		return members[i].Index < members[j].Index
	})
	for i := range members {
		members[i].Rank = int32(i)
	}
	return members, nil
}

// ValidateUserTask uses the execution compiler with a synthetic, inert
// assignment to validate the complete supported user contract before placement.
func ValidateUserTask(t native.TaskSpec, head bool) error {
	if t.Runtime.Kind != "Docker" || t.Runtime.Name != "" || t.Placement.Environment == "" || t.Placement.Binding != nil || t.Placement.DockerBinding != nil || t.DownstreamName != "" || len(t.Environments) != 0 || t.Suspended || t.Workload.Template == nil || len(t.Workload.Template.Spec.Containers) != 1 || t.Workload.Kind != taskapi.WorkloadTypeRay || t.Workload.Version != WorkloadVersion || t.Workload.Carrier.Type != dockerruntime.CarrierType || t.Workload.Carrier.Version != dockerruntime.CarrierVersion || len(t.Workload.Carrier.Parameters.Raw) != 0 {
		return errors.New("unsupported Docker Ray task fields")
	}
	if _, err := labels.ValidatedSelectorFromSet(t.Placement.NodeSelector); err != nil {
		return err
	}
	task := &native.Task{ObjectMeta: metav1.ObjectMeta{Name: "validate", Namespace: "validate", UID: "validate", Generation: 1}, Spec: *t.DeepCopy()}
	snapshot, err := dockerruntime.DecodeTask(task)
	if err != nil {
		return err
	}
	_, _, cards, err := resources(t.Workload.Template.Spec.Containers[0].Resources)
	if err != nil {
		return err
	}
	if cards == 0 {
		cards = 1
	}
	if cards > 256 {
		return errors.New("ray device request exceeds preview policy")
	}
	gpus := make([]string, cards)
	for i := range gpus {
		gpus[i] = fmt.Sprintf("GPU-validation-%d", i)
	}
	rank := int32(1)
	if head {
		rank = 0
	}
	a := Adapter{Assignment: Assignment{Task: snapshot.Identity, Addresses: []string{"10.0.0.1", "10.0.0.2"}, Rank: rank, Ports: DefaultPortPlan(), GPUUUIDs: gpus}}
	_, err = a.Compile(context.Background(), workload.CompileRequest{Task: snapshot, Runtime: rt.Info{Kind: dockerruntime.RuntimeKind}})
	return err
}

// DefaultPortPlan is an operator-reserved explicit block, persisted once per
// execution. Dedicated hosts allow distinct Jobs to use the same block.
func DefaultPortPlan() PortPlan { return PortPlan{Base: 24000, WorkerCount: 256} }

// Tasks returns the canonical desired children for a validated member list.
func Tasks(job *native.Job, namespace string, members []Member) []*native.Task {
	result := make([]*native.Task, 0, len(members))
	for _, m := range members {
		t := &native.Task{ObjectMeta: metav1.ObjectMeta{Name: m.Name, Namespace: namespace, Annotations: map[string]string{utils.ParentUIDAnnotation: string(job.UID), utils.ChildTemplateAnnotation: m.Template, nativejob.RestartedAtAnnotation: job.Annotations[nativejob.RestartedAtAnnotation], RankAnnotation: fmt.Sprint(m.Rank)}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, native.GroupVersion.WithKind("Job"))}}, Spec: *m.Spec.DeepCopy()}
		tenant.Inherit(job, t)
		result = append(result, t)
	}
	return result
}

// MatchesTask proves the complete canonical child contract and parent instance.
func MatchesTask(job *native.Job, namespace string, task *native.Task) bool {
	members, err := Expand(job)
	if err != nil || task == nil {
		return false
	}
	for _, wanted := range Tasks(job, namespace, members) {
		if task.Name != wanted.Name {
			continue
		}
		owner := metav1.GetControllerOf(task)
		if owner == nil || !reflect.DeepEqual(*owner, wanted.OwnerReferences[0]) || tenant.IDOf(task) != tenant.IDOf(job) || task.Namespace != namespace {
			return false
		}
		for k, v := range wanted.Annotations {
			if task.Annotations[k] != v {
				return false
			}
		}
		return equalSpec(task.Spec, wanted.Spec)
	}
	return false
}

func equalSpec(a, b native.TaskSpec) bool {
	canonical := func(spec native.TaskSpec) []byte {
		data, err := json.Marshal(spec)
		if err != nil {
			return nil
		}
		var v any
		d := json.NewDecoder(strings.NewReader(string(data)))
		d.UseNumber()
		if d.Decode(&v) != nil {
			return nil
		}
		data, err = json.Marshal(v)
		if err != nil {
			return nil
		}
		return data
	}
	x, y := canonical(a), canonical(b)
	return x != nil && y != nil && string(x) == string(y)
}

// PlanIdentity identifies one binding/Job execution and exact Task incarnation set.
func PlanIdentity(p ext.RayGroupPlan) string {
	type member struct {
		Task       ext.NativeObjectReference
		Generation int64
	}
	ms := make([]member, len(p.Members))
	for i, m := range p.Members {
		ms[i] = member{m.Task, m.Generation}
	}
	return hash(struct {
		Binding, Job ext.NativeObjectReference
		Generation   int64
		Restart      string
		Members      []member
	}{p.Binding, p.Job, p.JobGeneration, p.Restart, ms})
}

// PlanDigest covers all immutable placement material and excludes observations.
func PlanDigest(p ext.RayGroupPlan) string { return hash(p) }
func hash(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	} // Only typed JSON primitives reach this helper.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ValidatePlan rejects incomplete, overlapping and noncanonical placements.
func ValidatePlan(p ext.RayGroupPlan) error {
	if !validRef(p.Binding) || p.Binding.Namespace == "" || !validRef(p.Job) || !validRef(p.Cluster) || p.JobGeneration < 1 || p.TenantID == "" || p.NetworkScope == "" || len(p.Members) < 1 || len(p.Members) > MaxMembers {
		return errors.New("incomplete Ray execution identity")
	}
	if err := (PortPlan{Base: p.Ports.Base, WorkerCount: p.Ports.WorkerCount}).Validate(); err != nil {
		return err
	}
	tasks, nodes, records, addresses, engines, gpus := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, m := range p.Members {
		ip, err := netip.ParseAddr(m.PrivateAddr)
		if !validRef(m.Task) || !validRef(m.Node) || !validRef(m.NodeRecord) || m.Generation < 1 || m.Rank != int32(i) || m.Task.Namespace != p.Binding.Namespace || m.Node.Namespace != p.Binding.Namespace || m.NodeRecord.Namespace != p.Binding.Namespace || m.InstanceID == "" || m.CredentialID == "" || err != nil || !ip.Is4() || !ip.IsPrivate() || ip.String() != m.PrivateAddr || tasks[m.Task.UID] || nodes[m.Node.UID] || records[m.NodeRecord.UID] || engines[m.InstanceID] || addresses[m.PrivateAddr] || len(m.GPUUUIDs) == 0 {
			return errors.New("ray members have incomplete or overlapping identities")
		}
		tasks[m.Task.UID], nodes[m.Node.UID], records[m.NodeRecord.UID], addresses[m.PrivateAddr], engines[m.InstanceID] = true, true, true, true, true
		last := ""
		for _, gpu := range m.GPUUUIDs {
			if !strings.HasPrefix(gpu, "GPU-") || strings.ContainsAny(gpu, ", \t\r\n") || gpu <= last || gpus[gpu] {
				return errors.New("ray entitlements require sorted unique whole GPU UUIDs")
			}
			gpus[gpu], last = true, gpu
		}
	}
	return nil
}
func validRef(r ext.NativeObjectReference) bool { return r.Name != "" && r.UID != "" }

// ValidateGroup rehashes one member's complete immutable group material.
func ValidateGroup(g *ext.AllocationGroup) error {
	if g == nil || ValidatePlan(g.Plan) != nil || g.ID != PlanIdentity(g.Plan) || g.Digest != PlanDigest(g.Plan) || g.MemberIndex < 0 || int(g.MemberIndex) >= len(g.Plan.Members) {
		return errors.New("invalid Ray group identity or digest")
	}
	return nil
}

// AllocationMatches checks the outer reservation against its exact group member.
// Current heartbeat readiness is deliberately a separate authorization check.
func AllocationMatches(a ext.ContainerAllocationSpec) bool {
	g := a.Group
	if ValidateGroup(g) != nil || a.Upgrade != nil {
		return false
	}
	p, m := g.Plan, g.Plan.Members[g.MemberIndex]
	return a.Job == p.Job && a.JobGeneration == p.JobGeneration && a.JobBindingUID == p.Binding.UID && a.Cluster == p.Cluster && a.TenantID == p.TenantID && a.NetworkScope == p.NetworkScope && a.Task == m.Task && a.Generation == m.Generation && a.ExecutionID == m.Task.UID && a.Node == m.Node && a.NodeRecordUID == m.NodeRecord.UID && a.NodeInstanceID == m.InstanceID && a.PrivateAddress == m.PrivateAddr && slices.Equal(a.GPUUUIDs, m.GPUUUIDs)
}

// PreparedMatches verifies the owning Agent receipt for this exact allocation.
func PreparedMatches(a *ext.ContainerAllocation) bool {
	if a == nil || a.UID == "" || a.Status.ObservedGeneration != a.Generation || !AllocationMatches(a.Spec) || a.Status.Prepared == nil {
		return false
	}
	p, g := a.Status.Prepared, a.Spec.Group
	_, err := hex.DecodeString(p.ManifestDigest)
	return p.GroupID == g.ID && p.GroupDigest == g.Digest && p.AllocationUID == string(a.UID) && p.TaskUID == a.Spec.Task.UID && p.Generation == a.Spec.Generation && p.EngineID == a.Spec.NodeInstanceID && p.ContainerID != "" && len(p.ContainerID) <= 128 && len(p.ManifestDigest) == 64 && err == nil && strings.ToLower(p.ManifestDigest) == p.ManifestDigest
}
