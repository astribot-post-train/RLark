package docker

import (
	"errors"
	"strconv"

	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
)

// PortPlan is internal, versioned execution material supplied by group
// admission. It never derives ports from a display name or a random bind.
// Ray 2.56.1 exposes nine fixed service ports through its start CLI.
type PortPlan struct {
	Base        int32 `json:"base"`
	WorkerCount int32 `json:"workerCount"`
}

const servicePorts = int32(9)

// Validate rejects invalid/oversized ranges before expansion or daemon IO.
func (p PortPlan) Validate() error {
	if p.Base < 1024 || p.WorkerCount < 1 || p.WorkerCount > 256 || p.Base > 65535-servicePorts-p.WorkerCount+1 {
		return errors.New("ray port block requires nine service ports and 1..256 worker ports within 1024..65535")
	}
	return nil
}

// TCP returns the complete port block for provider preflight and journalled
// immutable manifests. Call Validate first; invalid plans return no ports.
func (p PortPlan) TCP() []int32 {
	if p.Validate() != nil {
		return nil
	}
	ports := make([]int32, servicePorts+p.WorkerCount)
	for i := range ports {
		ports[i] = p.Base + int32(i)
	}
	return ports
}

// StartArgs pins every Ray CLI service and its worker range. Workers receive
// the same block as the head on a different exclusively allocated host.
func (p PortPlan) StartArgs(head bool) ([]string, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	flag := func(name string, offset int32) string { return name + "=" + strconv.Itoa(int(p.Base+offset)) }
	args := []string{"start", "--block", "--disable-usage-stats",
		flag("--object-manager-port", 1), flag("--node-manager-port", 2),
		flag("--dashboard-agent-listen-port", 5), flag("--dashboard-agent-grpc-port", 6),
		flag("--runtime-env-agent-port", 7), flag("--metrics-export-port", 8),
		flag("--min-worker-port", servicePorts), flag("--max-worker-port", servicePorts+p.WorkerCount-1),
	}
	if head {
		args = append(args, "--head", flag("--port", 0), flag("--ray-client-server-port", 3), "--include-dashboard=false", flag("--dashboard-port", 4))
	}
	if err := dockerruntime.ValidateHostPortPlan(p.TCP()); err != nil {
		return nil, err
	}
	return args, nil
}
