// Package raylauncher implements the built-in Ray head and worker lifecycle
// without using shell scripts for RLark-owned orchestration.
package raylauncher

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	RoleHead   = "head"
	RoleWorker = "worker"
)

type Config struct {
	Role         string
	RayBinary    string
	BashBinary   string
	PythonBinary string
	Prepared     bool
	WaitNetwork  bool
	SelfBinary   string
	// SharedHost is set where the launcher does not own the whole host (a Raw
	// node): Ray runs as the launcher's own child process group and is stopped
	// through it, never by a host-wide `ray stop`.
	SharedHost bool
}

// ExitError preserves the exit status of a user compatibility script or Ray
// process across the Cobra command boundary.
type ExitError struct {
	Operation string
	Code      int
	Err       error
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s exited with code %d: %v", e.Operation, e.Code, e.Err)
}
func (e *ExitError) Unwrap() error { return e.Err }

var commandRunner = run
var headStarter = startHead
var executableChecker = checkExecutable
var hostResolver = net.LookupHost

// ownedStarter starts a process group that the launcher stops itself; the
// returned function waits for it. Cancelling ctx sends SIGTERM to the group.
var ownedStarter = startOwned

// gpuIndexer maps GPU UUIDs to nvidia-smi indices.
var gpuIndexer = nvidiaSMIIndices

// portReady reports whether the Ray GCS accepts connections.
var portReady = func(address string) bool {
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

//go:embed files/prepare_and_exec.sh
var prepareAndExecScript string

//go:embed files/cluster_check.py
var clusterCheckProgram string

func (c Config) Validate() error {
	if c.Role != RoleHead && c.Role != RoleWorker {
		return fmt.Errorf("ray role must be head or worker")
	}
	if c.RayBinary == "" {
		return fmt.Errorf("ray binary is required")
	}
	if c.BashBinary == "" {
		return fmt.Errorf("bash binary is required")
	}
	if c.PythonBinary == "" {
		return fmt.Errorf("python binary is required")
	}
	if !c.Prepared && c.SelfBinary == "" {
		return fmt.Errorf("self binary is required")
	}
	return nil
}

func Run(ctx context.Context, config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if err := setNodeRank(config.Role); err != nil {
		return err
	}
	if !config.Prepared {
		if config.WaitNetwork {
			if err := waitForNetwork(ctx, config.Role); err != nil {
				return err
			}
		}
		if script := os.Getenv("RLARK_PREPARE_SCRIPT"); script != "" {
			args := []string{"ray-launcher", "--prepared", "--role=" + config.Role, "--ray-binary=" + config.RayBinary, "--bash-binary=" + config.BashBinary, "--python-binary=" + config.PythonBinary}
			if config.WaitNetwork {
				args = append(args, "--wait-network")
			}
			if config.SharedHost {
				args = append(args, "--shared-host")
			}
			if err := runPrepared(ctx, config, script, args); err != nil {
				return preserveExit("prepare script", err)
			}
			return nil
		}
	}
	setRayDefaults()
	if err := normalizeVisibleGPUs(); err != nil {
		return err
	}
	if err := executableChecker(config.RayBinary); err != nil {
		return err
	}
	extra, err := rayStartArgs()
	if err != nil {
		return err
	}
	if config.SharedHost {
		if config.Role == RoleHead {
			return runSharedHead(ctx, config, extra)
		}
		return runSharedWorker(ctx, config, extra)
	}
	if config.Role == RoleHead {
		stopStaleRay(config.RayBinary)
		return runHead(ctx, config, extra)
	}
	return runWorker(ctx, config, extra)
}

func waitForNetwork(ctx context.Context, role string) error {
	host := hostname() + ".rlark-domain"
	if role == RoleWorker {
		host = os.Getenv("RLARK_HEAD_ADDRESS")
		if host == "" {
			return fmt.Errorf("RLARK_HEAD_ADDRESS is required for Ray worker")
		}
	}
	for {
		addresses, err := hostResolver(host)
		if err == nil && len(addresses) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for RLark network host %q: %w", host, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func checkExecutable(binary string) error {
	if strings.ContainsRune(binary, os.PathSeparator) {
		info, err := os.Stat(binary)
		if err != nil {
			return fmt.Errorf("ray CLI %q is unavailable: %w", binary, err)
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("ray CLI %q is not executable", binary)
		}
		return nil
	}
	if _, err := exec.LookPath(binary); err != nil {
		return fmt.Errorf("ray CLI %q is unavailable: %w", binary, err)
	}
	return nil
}

func runPrepared(ctx context.Context, config Config, script string, args []string) error {
	// The script remains the explicit compatibility boundary. Passing it as a
	// positional parameter avoids interpolating it into RLark-owned shell text.
	argv := []string{"-c", prepareAndExecScript, "rlark-prepare", script, config.SelfBinary}
	argv = append(argv, args...)
	return commandRunner(ctx, config.BashBinary, argv...)
}

func headNodeIP(ctx context.Context) string {
	nodeIP := os.Getenv("RLARK_NODE_IP")
	if nodeIP == "" {
		nodeIP = firstHostIP()
	}
	if os.Getenv("RLARK_DOMAIN") != "" {
		if resolved := resolveWithRetry(ctx, hostname()+".rlark-domain", 120); resolved != "" {
			nodeIP = resolved
		}
	}
	return nodeIP
}

func runHead(ctx context.Context, config Config, extra []string) error {
	port := getenv("RLARK_RAY_PORT", "6379")
	temp := getenv("RAY_TEMP_DIR", "/tmp/ray")
	nodeIP := headNodeIP(ctx)
	if err := headStarter(ctx, config.RayBinary, nodeIP, port, temp, extra); err != nil {
		return preserveExit("start Ray head", err)
	}
	defer stopRay(config.RayBinary)
	waitForCluster(ctx, config.PythonBinary, net.JoinHostPort(nodeIP, port), getenv("RLARK_TOTAL_NODES", "1"))
	if err := commandRunner(ctx, config.RayBinary, "status"); err != nil {
		return preserveExit("Ray status", err)
	}
	if script := os.Getenv("RLARK_RUN_SCRIPT"); script != "" {
		if err := commandRunner(ctx, config.BashBinary, "-c", script); err != nil {
			return preserveExit("run script", err)
		}
	}
	<-ctx.Done() // 执行完 RLARK_RUN_SCRIPT 开始阻塞，保证 pod 不退出
	return ctx.Err()
}

func startHead(ctx context.Context, binary, nodeIP, port, temp string, extra []string) error {
	args := append([]string{"start", "--head", "--dashboard-host=0.0.0.0", "--disable-usage-stats", "--node-ip-address=" + nodeIP, "--port=" + port, "--temp-dir", temp}, extra...)
	return run(ctx, binary, args...)
}

func waitForCluster(ctx context.Context, python, address, nodes string) {
	_ = commandRunner(ctx, python, "-u", "-c", clusterCheckProgram, address, nodes)
}

func runWorker(ctx context.Context, config Config, extra []string) error {
	port := getenv("RLARK_RAY_PORT", "6379")
	temp := getenv("RAY_TEMP_DIR", "/tmp/ray")
	head := os.Getenv("RLARK_HEAD_ADDRESS")
	if head == "" {
		return fmt.Errorf("RLARK_HEAD_ADDRESS is required for Ray worker")
	}
	address := waitForHead(ctx, head, port, 120)
	defer stopRay(config.RayBinary)
	args := append([]string{"start", "--address=" + address, "--temp-dir", temp, "--block"}, extra...)
	if err := commandRunner(ctx, config.RayBinary, args...); err != nil {
		return preserveExit("start Ray worker", err)
	}
	return nil
}

func run(ctx context.Context, executable string, args ...string) error {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
func stopRay(binary string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = commandRunner(ctx, binary, "stop", "--force")
}
func stopStaleRay(binary string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = commandRunner(ctx, binary, "stop")
}
func waitForHead(ctx context.Context, host, port string, attempts int) string {
	for i := 0; i < attempts; i++ {
		ips, _ := hostResolver(host)
		for _, ip := range ips {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, port), time.Second)
			if err == nil {
				_ = conn.Close()
				return net.JoinHostPort(ip, port)
			}
		}
		select {
		case <-ctx.Done():
			return net.JoinHostPort(host, port)
		case <-time.After(time.Second):
		}
	}
	return net.JoinHostPort(host, port)
}
func resolveWithRetry(ctx context.Context, host string, attempts int) string {
	for i := 0; i < attempts; i++ {
		ips, _ := hostResolver(host)
		if len(ips) > 0 {
			return ips[0]
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(time.Second):
		}
	}
	return ""
}
func firstHostIP() string {
	addresses, _ := net.InterfaceAddrs()
	for _, address := range addresses {
		var ip net.IP
		switch value := address.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if ip != nil && !ip.IsLoopback() && ip.To4() != nil {
			return ip.String()
		}
	}
	return "127.0.0.1"
}
func hostname() string { value, _ := os.Hostname(); return value }
func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func setRayDefaults() {
	defaults := map[string]string{"RAY_health_check_period_ms": "5000", "RAY_health_check_timeout_ms": "15000", "RAY_health_check_failure_threshold": "30", "RAY_num_heartbeats_timeout": "60"}
	for name, value := range defaults {
		if os.Getenv(name) == "" {
			_ = os.Setenv(name, value)
		}
	}
}

// setNodeRank sets RLINF_NODE_RANK. RLinf requires every node or no node to set
// it, so a worker that has a rank forces the head to have one too.
//
// Precedence: an explicit RLARK_NODE_RANK (a Raw process has no Pod ordinal);
// then RLARK_NODE_RANK_START plus the Pod ordinal (Kubernetes replicas); then,
// for a head without a Pod name, RLARK_NODE_RANK_START (0 by default).
func setNodeRank(role string) error {
	if explicit := os.Getenv("RLARK_NODE_RANK"); explicit != "" {
		rank, err := strconv.Atoi(explicit)
		if err != nil || rank < 0 {
			return fmt.Errorf("invalid RLARK_NODE_RANK %q: must be a non-negative integer", explicit)
		}
		return exportRank(rank)
	}
	startValue := os.Getenv("RLARK_NODE_RANK_START")
	start, err := strconv.Atoi(startValue)
	if err != nil && startValue != "" {
		return fmt.Errorf("invalid RLARK_NODE_RANK_START %q: %w", startValue, err)
	}
	pod := os.Getenv("POD_NAME")
	if index := strings.LastIndexByte(pod, '-'); index >= 0 {
		ordinal, _ := strconv.Atoi(pod[index+1:])
		return exportRank(start + ordinal)
	}
	if pod == "" && role == RoleHead {
		return exportRank(start)
	}
	return nil
}

func exportRank(rank int) error {
	value := strconv.Itoa(rank)
	if err := os.Setenv("RLINF_NODE_RANK", value); err != nil {
		return fmt.Errorf("set RLINF_NODE_RANK: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "RLINF_NODE_RANK: %s\n", value)
	return nil
}

func preserveExit(operation string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitError{Operation: operation, Code: exitErr.ExitCode(), Err: err}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
