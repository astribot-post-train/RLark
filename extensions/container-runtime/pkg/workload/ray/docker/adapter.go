// Package docker compiles the internal Ray execution contract for host Docker.
// It is not installed by the scoped Agent until group admission is available.
package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	taskapi "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	executionplan "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/plan"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
)

const (
	WorkloadType    workload.TypeID = taskapi.WorkloadTypeRay
	WorkloadVersion                 = taskapi.WorkloadVersionRayV1
)

// Assignment contains only trusted, resolved allocation input. Its presence
// does not itself authorize effects: the Provider still requires admission.
// Addresses are ordered by rank, with the head at index zero.
type Assignment struct {
	Task      rt.TaskIdentity
	Addresses []string
	Rank      int32
	Ports     PortPlan
	GPUUUIDs  []string
}

func (a Assignment) validate(task rt.TaskIdentity) error {
	if a.Task != task || task.ObjectUID == "" || task.Generation < 1 || task.Logical.Validate() != nil {
		return errors.New("ray assignment does not identify this exact Task generation")
	}
	if len(a.Addresses) < 1 || len(a.Addresses) > 16 || a.Rank < 0 || int(a.Rank) >= len(a.Addresses) {
		return errors.New("ray assignment has invalid membership or rank")
	}
	seen := map[string]bool{}
	for _, value := range a.Addresses {
		ip, err := netip.ParseAddr(value)
		if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.String() != value || seen[value] {
			return errors.New("ray members require unique canonical private IPv4 addresses")
		}
		seen[value] = true
	}
	if len(a.GPUUUIDs) == 0 {
		return errors.New("ray assignment requires whole GPU UUIDs")
	}
	if err := (dockerruntime.ContainerSpec{Image: "assigned", HostNetwork: true, GPUUUIDs: a.GPUUUIDs}).Validate(); err != nil {
		return err
	}
	return a.Ports.Validate()
}

// Adapter accepts a complete resolved assignment and an explicitly supported
// PodTemplate subset. An empty Adapter fails before creating runtime objects.
type Adapter struct{ Assignment Assignment }

var _ workload.Adapter = Adapter{}
var _ workload.InventoryAdapter = Adapter{}

func (Adapter) Info(context.Context) (workload.TypeInfo, error) {
	return workload.TypeInfo{ID: WorkloadType, Version: WorkloadVersion, SupportedRuntimeKinds: []rt.Kind{dockerruntime.RuntimeKind}, Configuration: workload.ConfigurationContract{Version: WorkloadVersion}, Capabilities: []rt.Capability{rt.CapabilityWorkloadInventory}}, nil
}

func (Adapter) Inventory(ctx context.Context, r workload.InventoryRequest) (workload.Inventory, error) {
	if err := ctx.Err(); err != nil {
		return workload.Inventory{}, err
	}
	if r.Runtime.Kind != dockerruntime.RuntimeKind || r.Carrier.Type != dockerruntime.CarrierType || r.Carrier.Version != dockerruntime.CarrierVersion {
		return workload.Inventory{}, fault.New(fault.InvalidArgument, "docker Ray v1 carrier required")
	}
	return workload.Inventory{Candidates: []rt.ObjectCandidate{{Kind: dockerruntime.ContainerKind, Namespace: r.Task.Logical.Namespace, Name: r.Task.Logical.Name, Ownership: rt.ResourceOwnership{Scope: rt.IdentityTaskIncarnation, OwnerKey: r.Task.ObjectUID}, Lifecycle: rt.LifecycleDeleteOnTaskCleanup}}}, nil
}

func (a Adapter) Compile(ctx context.Context, r workload.CompileRequest) (executionplan.Plan, error) {
	if err := ctx.Err(); err != nil {
		return executionplan.Plan{}, err
	}
	invalid := func(err error) (executionplan.Plan, error) {
		return executionplan.Plan{}, fault.Wrap(fault.InvalidArgument, "compile Docker Ray execution", err)
	}
	if r.Runtime.Kind != dockerruntime.RuntimeKind || r.Task.WorkloadSelector.Type != string(WorkloadType) || r.Task.WorkloadSelector.Version != WorkloadVersion || r.Task.CarrierSelector.Type != dockerruntime.CarrierType || r.Task.CarrierSelector.Version != dockerruntime.CarrierVersion || len(r.Task.Configuration.Carrier.Parameters) != 0 || !r.Task.Environment.Empty() || len(r.Task.LocalEnvironments) != 0 {
		return invalid(errors.New("docker Ray v1 requires a consumed carrier and no injected environments"))
	}
	params, err := taskapi.DecodeParametersStrict[taskapi.RayExecutionParametersV1](apiextensionsv1.JSON{Raw: r.Task.Configuration.Parameters})
	if err != nil {
		return invalid(err)
	}
	if err := a.Assignment.validate(r.Task.Identity); err != nil {
		return invalid(err)
	}
	head := a.Assignment.Rank == 0
	if params.Role != taskapi.RayRoleHead && params.Role != taskapi.RayRoleWorker || head != (params.Role == taskapi.RayRoleHead) {
		return invalid(errors.New("ray role disagrees with assigned rank"))
	}
	if params.HeadAddress != "" || params.HeadTaskName != "" || params.TotalNodes != 0 || params.NodeRankStart != 0 {
		return invalid(errors.New("ray address, head identity, node count and rank are allocation-owned"))
	}
	main, err := supportedContainer(r.Task.Configuration.Template)
	if err != nil {
		return invalid(err)
	}
	inputs := append(slices.Clone(main.Command), main.Args...)
	if params.Prepare != nil {
		inputs = append(inputs, params.Prepare.Inline)
	}
	if params.Run != nil {
		inputs = append(inputs, params.Run.Inline)
	}
	for _, value := range inputs {
		if strings.ContainsRune(value, 0) {
			return invalid(errors.New("NUL in Ray command or script"))
		}
	}
	if len(main.Args) > 0 && len(main.Command) == 0 {
		return invalid(errors.New("ray args require an explicit command"))
	}
	hasRun := params.Run != nil && strings.TrimSpace(params.Run.Inline) != ""
	if hasRun && len(main.Command) > 0 {
		return invalid(errors.New("ray run script and container command are mutually exclusive"))
	}
	if !head && (hasRun || len(main.Command) > 0) {
		return invalid(errors.New("ray worker is a resident service and cannot run training"))
	}
	if head && !hasRun && len(main.Command) == 0 {
		return invalid(errors.New("ray head requires a run script or explicit command"))
	}
	env, err := explicitEnv(main.Env)
	if err != nil {
		return invalid(err)
	}
	cpu, memory, gpu, err := resources(main.Resources)
	if err != nil {
		return invalid(err)
	}
	if gpu != 0 && gpu != int64(len(a.Assignment.GPUUUIDs)) {
		return invalid(errors.New("ray GPU request must match the complete allocated device set"))
	}
	address := a.Assignment.Addresses[a.Assignment.Rank]
	env = append(env, "RLARK_HEAD_ADDRESS="+a.Assignment.Addresses[0], "RLARK_NODE_ADDRESS="+address, "RLARK_RAY_PORT="+strconv.Itoa(int(a.Assignment.Ports.Base)), "RLARK_TOTAL_NODES="+strconv.Itoa(len(a.Assignment.Addresses)), "RLARK_NODE_RANK_START="+strconv.Itoa(int(a.Assignment.Rank)))
	args, err := a.Assignment.Ports.StartArgs(head)
	if err != nil {
		return invalid(err)
	}
	args = append(args, "--node-ip-address="+address, "--num-gpus="+strconv.Itoa(len(a.Assignment.GPUUUIDs)))
	if !head {
		args = append(args, fmt.Sprintf("--address=%s:%d", a.Assignment.Addresses[0], a.Assignment.Ports.Base))
	}
	if cpu > 0 {
		args = append(args, "--num-cpus="+strconv.FormatFloat(float64(cpu)/1000, 'f', -1, 64))
	}
	config := launchConfig{Head: head, Start: append([]string{"ray"}, args...), Address: fmt.Sprintf("%s:%d", a.Assignment.Addresses[0], a.Assignment.Ports.Base), NodeAddress: address, Members: slices.Clone(a.Assignment.Addresses), Command: append(slices.Clone(main.Command), main.Args...), StartupSeconds: 120}
	if params.Prepare != nil {
		config.Prepare = params.Prepare.Inline
	}
	if params.Run != nil {
		config.Run = params.Run.Inline
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return invalid(err)
	}
	spec := dockerruntime.ContainerSpec{Image: main.Image, Entrypoint: []string{"python3", "-u", "-c", launcher}, Command: []string{string(encoded)}, Env: env, WorkingDir: main.WorkingDir, CPU: cpu, MemoryBytes: memory, GPUUUIDs: slices.Clone(a.Assignment.GPUUUIDs), HostNetwork: true, RayPort: a.Assignment.Ports.Base, RayPorts: a.Assignment.Ports.TCP()}
	object, err := dockerruntime.DesiredContainer(r.Task.Identity, spec)
	if err != nil {
		return invalid(err)
	}
	sum := sha256.Sum256(object.Manifest.Object)
	return executionplan.Plan{AdapterID: string(WorkloadType), AdapterVersion: WorkloadVersion, CompiledHash: hex.EncodeToString(sum[:]), Objects: []rt.DesiredObject{object}, Discovery: executionplan.InstanceDiscovery{Roots: []string{"container"}}}, nil
}

func supportedContainer(t corev1.PodTemplateSpec) (corev1.Container, error) {
	if len(t.Spec.Containers) != 1 {
		return corev1.Container{}, errors.New("docker Ray requires exactly one main container")
	}
	c := t.Spec.Containers[0]
	if c.Name != "" && c.Name != "main" {
		return c, errors.New("docker Ray container must be named main")
	}
	allowed := corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Name: c.Name, Image: c.Image, Command: c.Command, Args: c.Args, Env: c.Env, WorkingDir: c.WorkingDir, Resources: c.Resources}}}}
	if !reflect.DeepEqual(t, allowed) {
		return c, errors.New("unsupported Docker Ray PodTemplate or container field")
	}
	if strings.TrimSpace(c.Image) == "" {
		return c, errors.New("docker Ray image is required")
	}
	return c, nil
}

func explicitEnv(values []corev1.EnvVar) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, v := range values {
		if v.ValueFrom != nil || v.Name == "" || strings.ContainsAny(v.Name, "=\x00") || seen[v.Name] || v.Name == "CUDA_VISIBLE_DEVICES" || v.Name == "NVIDIA_VISIBLE_DEVICES" || strings.HasPrefix(v.Name, "RLARK_") {
			return nil, errors.New("ray environment requires unique explicit non-reserved names")
		}
		seen[v.Name] = true
		out = append(out, v.Name+"="+v.Value)
	}
	return out, nil
}

func resources(r corev1.ResourceRequirements) (int64, int64, int64, error) {
	if len(r.Claims) != 0 {
		return 0, 0, 0, errors.New("docker Ray resource claims are unsupported")
	}
	for _, set := range []corev1.ResourceList{r.Requests, r.Limits} {
		for key, q := range set {
			if key != corev1.ResourceCPU && key != corev1.ResourceMemory && key != "nvidia.com/gpu" || q.Sign() < 0 {
				return 0, 0, 0, errors.New("unsupported or negative Docker Ray resource")
			}
			if other, ok := r.Limits[key]; ok {
				if _, exists := r.Requests[key]; exists && other.Cmp(r.Requests[key]) != 0 {
					return 0, 0, 0, errors.New("docker Ray requests and limits must match")
				}
			}
		}
	}
	get := func(n corev1.ResourceName) resource.Quantity {
		if q, ok := r.Limits[n]; ok {
			return q
		}
		return r.Requests[n]
	}
	cpu, memory, gpu := get(corev1.ResourceCPU), get(corev1.ResourceMemory), get("nvidia.com/gpu")
	maxCPU := resource.NewMilliQuantity(math.MaxInt64/1_000_000, resource.DecimalSI)
	if cpu.Cmp(*maxCPU) > 0 {
		return 0, 0, 0, errors.New("docker Ray CPU overflows NanoCPUs")
	}
	if cpu.MilliValue()%1000 != 0 {
		return 0, 0, 0, errors.New("docker Ray CLI requires a whole CPU count")
	}
	mem, ok := memory.AsInt64()
	if !ok && !memory.IsZero() {
		return 0, 0, 0, errors.New("docker Ray memory must fit whole int64 bytes")
	}
	cards, ok := gpu.AsInt64()
	if !ok && !gpu.IsZero() {
		return 0, 0, 0, errors.New("docker Ray GPUs must be whole devices")
	}
	return cpu.MilliValue(), mem, cards, nil
}

func (Adapter) Observe(ctx context.Context, r workload.ObserveRequest) (workload.Observation, error) {
	if err := ctx.Err(); err != nil {
		return workload.Observation{}, err
	}
	if len(r.Instances.Instances) == 0 {
		return workload.Observation{Phase: "Pending", Reason: "ContainerAbsent"}, nil
	}
	if len(r.Instances.Instances) != 1 {
		return workload.Observation{}, fault.New(fault.Conflict, "docker Ray inventory is ambiguous")
	}
	instance := r.Instances.Instances[0]
	return workload.Observation{Phase: workload.Phase(instance.Phase), Reason: instance.Reason, Message: instance.Message}, nil
}
