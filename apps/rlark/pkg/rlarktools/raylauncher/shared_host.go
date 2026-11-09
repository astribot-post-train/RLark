package raylauncher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// rayStartPorts are the optional Ray start ports. A Raw node shares its
// network with everything else on the host and its relay only forwards an
// explicit port range, so its execution sets them; a Kubernetes Pod leaves
// them unset and keeps Ray's defaults.
var rayStartPorts = []struct{ env, flag string }{
	{"RLARK_RAY_NODE_MANAGER_PORT", "--node-manager-port"},
	{"RLARK_RAY_OBJECT_MANAGER_PORT", "--object-manager-port"},
	{"RLARK_RAY_MIN_WORKER_PORT", "--min-worker-port"},
	{"RLARK_RAY_MAX_WORKER_PORT", "--max-worker-port"},
}

// rayStartArgs returns the extra `ray start` flags configured through RLARK_RAY_*.
func rayStartArgs() ([]string, error) {
	var args []string
	values := map[string]int{}
	for _, port := range rayStartPorts {
		value := os.Getenv(port.env)
		if value == "" {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 || number > 65535 {
			return nil, fmt.Errorf("invalid %s %q: must be a port between 1 and 65535", port.env, value)
		}
		values[port.env] = number
		args = append(args, port.flag+"="+value)
	}
	minimum, hasMinimum := values["RLARK_RAY_MIN_WORKER_PORT"]
	maximum, hasMaximum := values["RLARK_RAY_MAX_WORKER_PORT"]
	if hasMinimum != hasMaximum {
		return nil, errors.New("RLARK_RAY_MIN_WORKER_PORT and RLARK_RAY_MAX_WORKER_PORT must be set together")
	}
	if hasMinimum && minimum > maximum {
		return nil, fmt.Errorf("RLARK_RAY_MIN_WORKER_PORT %d is above RLARK_RAY_MAX_WORKER_PORT %d", minimum, maximum)
	}
	if value := os.Getenv("RLARK_RAY_NUM_GPUS"); value != "" {
		if number, err := strconv.Atoi(value); err != nil || number < 0 {
			return nil, fmt.Errorf("invalid RLARK_RAY_NUM_GPUS %q: must be a non-negative integer", value)
		}
		args = append(args, "--num-gpus="+value)
	}
	return args, nil
}

// normalizeVisibleGPUs rewrites a CUDA_VISIBLE_DEVICES of GPU UUIDs to the
// matching nvidia-smi indices. A Raw allocation names its GPUs by UUID, which
// CUDA accepts but Ray rejects ("Invalid visible device IDs"). Indices, an empty
// value and an unset variable are left as they are.
func normalizeVisibleGPUs() error {
	visible, set := os.LookupEnv("CUDA_VISIBLE_DEVICES")
	if !set || strings.TrimSpace(visible) == "" {
		return nil
	}
	entries := strings.Split(visible, ",")
	uuids := 0
	for i, entry := range entries {
		entries[i] = strings.TrimSpace(entry)
		if strings.HasPrefix(entries[i], "GPU-") || strings.HasPrefix(entries[i], "MIG-") {
			uuids++
		} else if _, err := strconv.Atoi(entries[i]); err != nil {
			return fmt.Errorf("CUDA_VISIBLE_DEVICES entry %q is neither a GPU index nor a GPU UUID", entries[i])
		}
	}
	if uuids == 0 {
		return nil
	}
	if uuids != len(entries) {
		return fmt.Errorf("CUDA_VISIBLE_DEVICES %q mixes GPU indices and UUIDs", visible)
	}
	indices, err := gpuIndexer()
	if err != nil {
		return fmt.Errorf("map CUDA_VISIBLE_DEVICES GPU UUIDs to indices: %w", err)
	}
	mapped := make([]string, len(entries))
	for i, uuid := range entries {
		index, found := indices[uuid]
		if !found {
			return fmt.Errorf("allocated GPU %s is not on this host", uuid)
		}
		mapped[i] = index
	}
	value := strings.Join(mapped, ",")
	if err := os.Setenv("CUDA_VISIBLE_DEVICES", value); err != nil {
		return fmt.Errorf("set CUDA_VISIBLE_DEVICES: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "CUDA_VISIBLE_DEVICES: %s (from %s)\n", value, visible)
	return nil
}

func nvidiaSMIIndices() (map[string]string, error) {
	output, err := exec.Command("nvidia-smi", "--query-gpu=index,uuid", "--format=csv,noheader").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	indices := map[string]string{}
	for _, line := range strings.Split(string(bytes.TrimSpace(output)), "\n") {
		index, uuid, found := strings.Cut(line, ",")
		if found {
			indices[strings.TrimSpace(uuid)] = strings.TrimSpace(index)
		}
	}
	return indices, nil
}

// runSharedHead runs `ray start --head --block` as the launcher's own process
// group, waits until the head accepts connections and the declared nodes have
// joined, then runs RLARK_RUN_SCRIPT. Ray and the script stop when ctx ends or
// either exits; nothing outside this launcher's process groups is touched.
func runSharedHead(ctx context.Context, config Config, extra []string) error {
	port := getenv("RLARK_RAY_PORT", "6379")
	temp := getenv("RAY_TEMP_DIR", "/tmp/ray")
	nodeIP := headNodeIP(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := append([]string{"start", "--head", "--block", "--dashboard-host=127.0.0.1", "--disable-usage-stats", "--node-ip-address=" + nodeIP, "--port=" + port, "--temp-dir", temp}, extra...)
	rayDone := waitAsync(ownedStarter(ctx, config.RayBinary, args...))
	address := net.JoinHostPort(nodeIP, port)
	for !portReady(address) {
		select {
		case err := <-rayDone:
			return preserveExit("start Ray head", exitedEarly(err))
		case <-ctx.Done():
			<-rayDone
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	waitForCluster(ctx, config.PythonBinary, address, getenv("RLARK_TOTAL_NODES", "1"))
	script := os.Getenv("RLARK_RUN_SCRIPT")
	if script == "" {
		select {
		case err := <-rayDone:
			return preserveExit("Ray head", exitedEarly(err))
		case <-ctx.Done():
			<-rayDone
			return ctx.Err()
		}
	}
	scriptDone := waitAsync(ownedStarter(ctx, config.BashBinary, "-c", script))
	select {
	case err := <-rayDone:
		cancel()
		<-scriptDone
		return preserveExit("Ray head", exitedEarly(err))
	case err := <-scriptDone:
		if err != nil {
			cancel()
			<-rayDone
			return preserveExit("run script", err)
		}
		// As in a Pod, the head stays up after RLARK_RUN_SCRIPT succeeds.
		select {
		case err := <-rayDone:
			return preserveExit("Ray head", exitedEarly(err))
		case <-ctx.Done():
			<-rayDone
			return ctx.Err()
		}
	case <-ctx.Done():
		<-scriptDone
		<-rayDone
		return ctx.Err()
	}
}

// runSharedWorker joins the head as the launcher's own process group.
func runSharedWorker(ctx context.Context, config Config, extra []string) error {
	port := getenv("RLARK_RAY_PORT", "6379")
	temp := getenv("RAY_TEMP_DIR", "/tmp/ray")
	head := os.Getenv("RLARK_HEAD_ADDRESS")
	if head == "" {
		return fmt.Errorf("RLARK_HEAD_ADDRESS is required for Ray worker")
	}
	address := waitForHead(ctx, head, port, 120)
	args := append([]string{"start", "--address=" + address, "--temp-dir", temp, "--block"}, extra...)
	if err := ownedStarter(ctx, config.RayBinary, args...)(); err != nil && ctx.Err() == nil {
		return preserveExit("start Ray worker", err)
	}
	return ctx.Err()
}

func waitAsync(wait func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- wait() }()
	return done
}

// exitedEarly turns an unexpected clean exit of a blocking process into an error.
func exitedEarly(err error) error {
	if err == nil {
		return errors.New("exited before it was stopped")
	}
	return err
}
