// Package docker implements the Ray workload contract on a host Docker daemon.
package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
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
	WorkloadType      workload.TypeID = taskapi.WorkloadTypeRay
	WorkloadVersion                   = taskapi.WorkloadVersionRayV1
	mainContainerName                 = "main"
	minRayPort                        = int32(20000)
	rayPortRange                      = int32(20000)
)

// Adapter lowers the common PodTemplate subset into one host-networked Docker
// container. Docker does not expose Kubernetes init containers, Services, or
// environment injection, so those features are rejected before any daemon IO.
type Adapter struct{}

var _ workload.Adapter = (*Adapter)(nil)
var _ workload.InventoryAdapter = (*Adapter)(nil)

func (Adapter) Info(context.Context) (workload.TypeInfo, error) {
	return workload.TypeInfo{ID: WorkloadType, Version: WorkloadVersion, SupportedRuntimeKinds: []rt.Kind{dockerruntime.RuntimeKind}, Configuration: workload.ConfigurationContract{Version: WorkloadVersion}, Capabilities: []rt.Capability{rt.CapabilityWorkloadInventory}}, nil
}

func (Adapter) Inventory(ctx context.Context, request workload.InventoryRequest) (workload.Inventory, error) {
	if err := ctx.Err(); err != nil {
		return workload.Inventory{}, err
	}
	if request.Runtime.Kind != dockerruntime.RuntimeKind || request.Carrier.Type != dockerruntime.CarrierType || request.Carrier.Version != dockerruntime.CarrierVersion {
		return workload.Inventory{}, fault.New(fault.InvalidArgument, "Docker Ray carrier is required")
	}
	return workload.Inventory{Candidates: []rt.ObjectCandidate{{Kind: dockerruntime.ContainerKind, Namespace: request.Task.Logical.Namespace, Name: request.Task.Logical.Name, Ownership: rt.ResourceOwnership{Scope: rt.IdentityTaskIncarnation, OwnerKey: request.Task.ObjectUID}, Lifecycle: rt.LifecycleDeleteOnTaskCleanup}}}, nil
}

func (Adapter) Compile(ctx context.Context, request workload.CompileRequest) (executionplan.Plan, error) {
	if err := ctx.Err(); err != nil {
		return executionplan.Plan{}, err
	}
	if request.Runtime.Kind != dockerruntime.RuntimeKind || request.Task.CarrierSelector.Type != dockerruntime.CarrierType || request.Task.CarrierSelector.Version != dockerruntime.CarrierVersion || len(request.Task.Configuration.Carrier.Parameters) != 0 {
		return executionplan.Plan{}, fault.New(fault.InvalidArgument, "Docker Ray v1 container carrier is required")
	}
	if len(request.Task.Environment.Environments) != 0 || len(request.Task.LocalEnvironments) != 0 {
		return executionplan.Plan{}, fault.New(fault.InvalidArgument, "Docker Ray does not support environment injection")
	}
	params, err := taskapi.DecodeParametersStrict[taskapi.RayExecutionParametersV1](apiextensionsv1.JSON{Raw: request.Task.Configuration.Parameters})
	if err != nil {
		return executionplan.Plan{}, fault.Wrap(fault.InvalidArgument, "decode Ray parameters", err)
	}
	template := request.Task.Configuration.Template
	if len(template.Spec.InitContainers) != 0 || len(template.Spec.Containers) != 1 || template.Spec.Containers[0].Name != "" && template.Spec.Containers[0].Name != mainContainerName {
		return executionplan.Plan{}, fault.New(fault.InvalidArgument, "Docker Ray requires exactly one main container and no init containers")
	}
	main := template.Spec.Containers[0]
	if strings.TrimSpace(main.Image) == "" {
		return executionplan.Plan{}, fault.New(fault.InvalidArgument, "Docker Ray main container image is required")
	}
	if !template.Spec.HostNetwork {
		return executionplan.Plan{}, fault.New(fault.InvalidArgument, "Docker Ray requires host network")
	}
	if len(template.Spec.Volumes) != 0 || template.Spec.ImagePullSecrets != nil || template.Spec.ServiceAccountName != "" || template.Spec.HostAliases != nil {
		return executionplan.Plan{}, fault.New(fault.InvalidArgument, "Docker Ray does not support Kubernetes volumes, credentials, service accounts, or host aliases")
	}
	env, err := explicitEnv(main.Env)
	if err != nil {
		return executionplan.Plan{}, err
	}
	cpu, memory, gpu, err := resources(main.Resources)
	if err != nil {
		return executionplan.Plan{}, err
	}
	portKey := request.Task.Identity.Logical.Name
	if params.Role == taskapi.RayRoleWorker {
		if strings.TrimSpace(params.HeadTaskName) == "" {
			return executionplan.Plan{}, fault.New(fault.InvalidArgument, "Docker Ray worker requires headTaskName")
		}
		headValue := params.HeadAddress
		if strings.TrimSpace(headValue) == "" {
			// Backward compatibility for pre-controller-resolved declarations.
			headValue = params.HeadTaskName
		}
		head, err := parseHeadAddress(headValue)
		if err != nil {
			return executionplan.Plan{}, fault.Wrap(fault.InvalidArgument, "parse Docker Ray head address", err)
		}
		env = appendUnique(env, "RLARK_HEAD_ADDRESS="+head)
		portKey = params.HeadTaskName
	}
	port := stablePort(portKey)
	env = appendUnique(env, "RLARK_RAY_PORT="+strconv.Itoa(int(port)))
	if params.TotalNodes > 0 {
		env = appendUnique(env, "RLARK_TOTAL_NODES="+strconv.Itoa(int(params.TotalNodes)))
	}
	env = appendUnique(env, "RLARK_NODE_RANK_START="+strconv.Itoa(int(params.NodeRankStart)))
	command, args := dockerCommand(main, params, port)
	spec := dockerruntime.ContainerSpec{Image: main.Image, Entrypoint: command, Command: args, Env: env, WorkingDir: main.WorkingDir, GPUCount: gpu, CPU: cpu, MemoryBytes: memory, HostNetwork: true, RayPort: port}
	object, err := dockerruntime.DesiredContainer(request.Task.Identity, spec)
	if err != nil {
		return executionplan.Plan{}, err
	}
	return executionplan.Plan{AdapterID: string(WorkloadType), AdapterVersion: WorkloadVersion, CompiledHash: hash(object.Manifest.Object), Objects: []rt.DesiredObject{object}, Discovery: executionplan.InstanceDiscovery{Roots: []string{"container"}}}, nil
}

func (Adapter) Observe(ctx context.Context, request workload.ObserveRequest) (workload.Observation, error) {
	if err := ctx.Err(); err != nil {
		return workload.Observation{}, err
	}
	if len(request.Instances.Instances) == 0 {
		return workload.Observation{Phase: workload.Phase("Pending"), Reason: "ContainerAbsent"}, nil
	}
	if len(request.Instances.Instances) != 1 {
		return workload.Observation{}, fault.New(fault.Conflict, "Docker Ray container inventory is ambiguous")
	}
	instance := request.Instances.Instances[0]
	return workload.Observation{Phase: workload.Phase(instance.Phase), Reason: instance.Reason, Message: instance.Message}, nil
}

func explicitEnv(values []corev1.EnvVar) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		if value.ValueFrom != nil || value.Name == "" || strings.ContainsRune(value.Name, '=') {
			return nil, fault.New(fault.InvalidArgument, "Docker environment must use explicit NAME=value entries")
		}
		if _, ok := seen[value.Name]; ok {
			return nil, fault.New(fault.InvalidArgument, "Docker environment contains duplicate names")
		}
		seen[value.Name] = struct{}{}
		result = append(result, value.Name+"="+value.Value)
	}
	return result, nil
}

func resources(resources corev1.ResourceRequirements) (int64, int64, int64, error) {
	get := func(name corev1.ResourceName) resource.Quantity {
		if value, ok := resources.Requests[name]; ok {
			return value
		}
		return resources.Limits[name]
	}
	cpu := get(corev1.ResourceCPU)
	memory := get(corev1.ResourceMemory)
	gpu := get("nvidia.com/gpu")
	if !cpu.IsZero() && cpu.Sign() < 0 || !memory.IsZero() && memory.Sign() < 0 || !gpu.IsZero() && gpu.Sign() < 0 {
		return 0, 0, 0, fault.New(fault.InvalidArgument, "Docker resources must not be negative")
	}
	if gpu.MilliValue()%1000 != 0 {
		return 0, 0, 0, fault.New(fault.InvalidArgument, "Docker GPU requests must be whole cards")
	}
	return cpu.MilliValue(), memory.Value(), gpu.Value(), nil
}

func dockerCommand(main corev1.Container, params taskapi.RayExecutionParametersV1, port int32) ([]string, []string) {
	role := string(params.Role)
	if role == "" {
		role = string(taskapi.RayRoleWorker)
	}
	start := "ray start --block --port=${RLARK_RAY_PORT}"
	if params.Role == taskapi.RayRoleHead {
		// Ray 2.x rejects port 0 for the client server even when the client
		// service is not used. Keep it on a deterministic companion port so
		// host-networked Docker jobs do not rely on an arbitrary bind.
		start = fmt.Sprintf("ray start --head --port=${RLARK_RAY_PORT} --include-dashboard=false --dashboard-port=0 --ray-client-server-port=%d", port+1)
	} else {
		start = "ray start --address=${RLARK_HEAD_ADDRESS}:${RLARK_RAY_PORT} --block"
	}
	cpu, _, gpu, _ := resources(main.Resources)
	if cpu > 0 {
		start += " --num-cpus=" + strconv.FormatFloat(float64(cpu)/1000, 'f', -1, 64)
	}
	start += " --num-gpus=" + strconv.FormatInt(gpu, 10)
	if params.Prepare != nil && strings.TrimSpace(params.Prepare.Inline) != "" {
		start = params.Prepare.Inline + "\n" + start
	}
	if params.Run != nil && strings.TrimSpace(params.Run.Inline) != "" {
		start += "\n" + params.Run.Inline
	}
	if params.Role == taskapi.RayRoleHead && params.Run == nil && len(main.Command) == 0 {
		start += "\ntail -f /dev/null"
	}
	if params.Run == nil && len(main.Command) > 0 {
		start += "\nexec " + shellJoin(append(main.Command, main.Args...))
	}
	if params.Role == taskapi.RayRoleWorker {
		start = "export RLARK_HEAD_ADDRESS=${RLARK_HEAD_ADDRESS}\n" + start
	}
	_ = port
	return []string{"/bin/sh"}, []string{"-lc", "set -eu\n# RLark Docker Ray launcher (" + role + ")\n" + start}
}

func shellJoin(values []string) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Quote(value)
	}
	return strings.Join(parts, " ")
}

func appendUnique(values []string, value string) []string {
	name := value[:strings.IndexByte(value, '=')]
	for _, existing := range values {
		if strings.HasPrefix(existing, name+"=") {
			return values
		}
	}
	return append(values, value)
}

func stablePort(key string) int32 {
	sum := sha256.Sum256([]byte(key))
	value := int32(sum[0])<<8 | int32(sum[1])
	return minRayPort + value%rayPortRange
}

func dockerContainerName(task rt.TaskIdentity) string {
	uid := task.ObjectUID
	if len(uid) > 48 {
		uid = uid[:48]
	}
	return "rlark-task-" + uid
}

func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func parseHeadAddress(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return host, nil
	}
	if net.ParseIP(value) == nil {
		return "", fmt.Errorf("invalid Docker Ray head address %q", value)
	}
	return value, nil
}
