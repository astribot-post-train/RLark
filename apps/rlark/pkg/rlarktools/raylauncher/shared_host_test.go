package raylauncher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHeadWithoutPodGetsRankZeroSoWorkerRanksAreConsistent(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("RLINF_NODE_RANK", "")
	if err := setNodeRank(RoleHead); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("RLINF_NODE_RANK"); got != "0" {
		t.Fatalf("RLINF_NODE_RANK=%q, want 0", got)
	}
}

func TestExplicitRankWinsAndInvalidRankFails(t *testing.T) {
	t.Setenv("POD_NAME", "env-worker-3")
	t.Setenv("RLARK_NODE_RANK_START", "1")
	t.Setenv("RLARK_NODE_RANK", "5")
	if err := setNodeRank(RoleWorker); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("RLINF_NODE_RANK"); got != "5" {
		t.Fatalf("RLINF_NODE_RANK=%q, want 5", got)
	}
	t.Setenv("RLARK_NODE_RANK", "-1")
	if err := setNodeRank(RoleWorker); err == nil {
		t.Fatal("negative rank accepted")
	}
}

func TestPodOrdinalRankIsUnchanged(t *testing.T) {
	t.Setenv("RLARK_NODE_RANK", "")
	t.Setenv("POD_NAME", "env-worker-2")
	t.Setenv("RLARK_NODE_RANK_START", "1")
	if err := setNodeRank(RoleWorker); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("RLINF_NODE_RANK"); got != "3" {
		t.Fatalf("RLINF_NODE_RANK=%q, want 3", got)
	}
}

func TestWorkerWithoutPodAndRankLeavesRankUnset(t *testing.T) {
	t.Setenv("RLARK_NODE_RANK", "")
	t.Setenv("POD_NAME", "")
	t.Setenv("RLINF_NODE_RANK", "")
	_ = os.Unsetenv("RLINF_NODE_RANK")
	if err := setNodeRank(RoleWorker); err != nil {
		t.Fatal(err)
	}
	if value, set := os.LookupEnv("RLINF_NODE_RANK"); set {
		t.Fatalf("RLINF_NODE_RANK=%q set for a worker without a rank", value)
	}
}

func stubGPUIndexer(t *testing.T, indices map[string]string, err error) {
	t.Helper()
	old := gpuIndexer
	gpuIndexer = func() (map[string]string, error) { return indices, err }
	t.Cleanup(func() { gpuIndexer = old })
}

func TestGPUUUIDsBecomeIndices(t *testing.T) {
	stubGPUIndexer(t, map[string]string{"GPU-aaa": "0", "GPU-bbb": "3"}, nil)
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-bbb,GPU-aaa")
	if err := normalizeVisibleGPUs(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CUDA_VISIBLE_DEVICES"); got != "3,0" {
		t.Fatalf("CUDA_VISIBLE_DEVICES=%q, want 3,0", got)
	}
}

func TestGPUIndicesAndEmptyAreUntouched(t *testing.T) {
	stubGPUIndexer(t, nil, errors.New("nvidia-smi must not run"))
	for _, value := range []string{"0,1", "", " "} {
		t.Setenv("CUDA_VISIBLE_DEVICES", value)
		if err := normalizeVisibleGPUs(); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if got := os.Getenv("CUDA_VISIBLE_DEVICES"); got != value {
			t.Fatalf("CUDA_VISIBLE_DEVICES=%q, want %q", got, value)
		}
	}
}

func TestGPUMappingFailuresAreReported(t *testing.T) {
	stubGPUIndexer(t, map[string]string{"GPU-aaa": "0"}, nil)
	cases := map[string]string{
		"GPU-zzz":   "not on this host",
		"GPU-aaa,1": "mixes",
		"gpu0":      "neither",
	}
	for value, want := range cases {
		t.Setenv("CUDA_VISIBLE_DEVICES", value)
		if err := normalizeVisibleGPUs(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: error=%v, want %q", value, err, want)
		}
	}
	stubGPUIndexer(t, nil, errors.New("no nvidia-smi"))
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-aaa")
	if err := normalizeVisibleGPUs(); err == nil || !strings.Contains(err.Error(), "no nvidia-smi") {
		t.Fatalf("error=%v, want the nvidia-smi failure", err)
	}
}

func TestRayStartPortsAreValidated(t *testing.T) {
	t.Setenv("RLARK_RAY_NODE_MANAGER_PORT", "41001")
	t.Setenv("RLARK_RAY_OBJECT_MANAGER_PORT", "41002")
	t.Setenv("RLARK_RAY_MIN_WORKER_PORT", "41100")
	t.Setenv("RLARK_RAY_MAX_WORKER_PORT", "41299")
	t.Setenv("RLARK_RAY_NUM_GPUS", "1")
	args, err := rayStartArgs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--node-manager-port=41001", "--object-manager-port=41002", "--min-worker-port=41100", "--max-worker-port=41299", "--num-gpus=1"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v, want %v", args, want)
	}
	t.Setenv("RLARK_RAY_MAX_WORKER_PORT", "41000")
	if _, err := rayStartArgs(); err == nil {
		t.Fatal("inverted worker port range accepted")
	}
	t.Setenv("RLARK_RAY_MAX_WORKER_PORT", "")
	if _, err := rayStartArgs(); err == nil {
		t.Fatal("half a worker port range accepted")
	}
	t.Setenv("RLARK_RAY_MIN_WORKER_PORT", "")
	t.Setenv("RLARK_RAY_NODE_MANAGER_PORT", "70000")
	if _, err := rayStartArgs(); err == nil {
		t.Fatal("out-of-range port accepted")
	}
}

// fakeOwned records the processes started through ownedStarter. A process ends
// when its context ends, or immediately with the configured result.
type fakeOwned struct {
	started [][]string
	exit    map[string]error // by executable; absent means "runs until stopped"
	stopped []string
}

func (f *fakeOwned) start(ctx context.Context, executable string, args ...string) func() error {
	f.started = append(f.started, append([]string{executable}, args...))
	result, exits := f.exit[executable]
	return func() error {
		if exits {
			return result
		}
		<-ctx.Done()
		f.stopped = append(f.stopped, executable)
		return ctx.Err()
	}
}

func stubSharedHost(t *testing.T, owned *fakeOwned) *[][]string {
	t.Helper()
	stubExecutableCheck(t)
	oldOwned, oldReady, oldRunner := ownedStarter, portReady, commandRunner
	ownedStarter, portReady = owned.start, func(string) bool { return true }
	var calls [][]string
	commandRunner = func(_ context.Context, executable string, args ...string) error {
		calls = append(calls, append([]string{executable}, args...))
		return nil
	}
	t.Cleanup(func() { ownedStarter, portReady, commandRunner = oldOwned, oldReady, oldRunner })
	t.Setenv("RLARK_NODE_IP", "10.1.2.45")
	t.Setenv("POD_NAME", "")
	t.Setenv("CUDA_VISIBLE_DEVICES", "")
	return &calls
}

func TestSharedHeadNeverRunsAHostWideRayStop(t *testing.T) {
	owned := &fakeOwned{exit: map[string]error{"/bin/bash": nil}}
	calls := stubSharedHost(t, owned)
	t.Setenv("RLARK_RUN_SCRIPT", "fleet-driver")
	t.Setenv("RLARK_RAY_NODE_MANAGER_PORT", "41001")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Role: RoleHead, RayBinary: "ray", BashBinary: "/bin/bash", PythonBinary: "python", Prepared: true, SharedHost: true})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
	if len(owned.started) != 2 || owned.started[0][0] != "ray" || owned.started[1][0] != "/bin/bash" {
		t.Fatalf("started=%v, want Ray head then the run script", owned.started)
	}
	head := strings.Join(owned.started[0], " ")
	for _, want := range []string{"start --head --block", "--node-ip-address=10.1.2.45", "--node-manager-port=41001"} {
		if !strings.Contains(head, want) {
			t.Fatalf("head argv %q lacks %q", head, want)
		}
	}
	for _, call := range *calls {
		if len(call) > 1 && call[0] == "ray" && call[1] == "stop" {
			t.Fatalf("shared-host head ran %v", call)
		}
	}
	if os.Getenv("RLINF_NODE_RANK") != "0" {
		t.Fatalf("RLINF_NODE_RANK=%q, want 0", os.Getenv("RLINF_NODE_RANK"))
	}
}

func TestSharedHeadFailsWhenTheRunScriptFailsAndStopsRay(t *testing.T) {
	owned := &fakeOwned{exit: map[string]error{"/bin/bash": run(context.Background(), "/bin/sh", "-c", "exit 4")}}
	stubSharedHost(t, owned)
	t.Setenv("RLARK_RUN_SCRIPT", "false")
	err := Run(context.Background(), Config{Role: RoleHead, RayBinary: "ray", BashBinary: "/bin/bash", PythonBinary: "python", Prepared: true, SharedHost: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Operation != "run script" || exitErr.Code != 4 {
		t.Fatalf("error=%#v, want run script exit 4", err)
	}
	if !reflect.DeepEqual(owned.stopped, []string{"ray"}) {
		t.Fatalf("stopped=%v, want the Ray head stopped", owned.stopped)
	}
}

func TestSharedHeadFailsWhenRayExitsBeforeItIsReady(t *testing.T) {
	owned := &fakeOwned{exit: map[string]error{"ray": nil}}
	stubSharedHost(t, owned)
	portReady = func(string) bool { return false }
	err := Run(context.Background(), Config{Role: RoleHead, RayBinary: "ray", BashBinary: "/bin/bash", PythonBinary: "python", Prepared: true, SharedHost: true})
	if err == nil || !strings.Contains(err.Error(), "start Ray head") || !strings.Contains(err.Error(), "exited before") {
		t.Fatalf("error=%v, want an early Ray head exit", err)
	}
}

func TestSharedHostFlagSurvivesThePrepareScript(t *testing.T) {
	t.Setenv("RLARK_PREPARE_SCRIPT", "true")
	old := commandRunner
	defer func() { commandRunner = old }()
	var argv []string
	commandRunner = func(_ context.Context, _ string, args ...string) error {
		argv = args
		return nil
	}
	if err := Run(context.Background(), Config{Role: RoleHead, RayBinary: "ray", BashBinary: "/bin/bash", PythonBinary: "python", SelfBinary: "/rlark-tools/rlark-tools", SharedHost: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--shared-host") {
		t.Fatalf("prepared re-exec argv=%v lacks --shared-host", argv)
	}
}

func TestOwnedProcessGroupStopsWithItsChildren(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "child-alive")
	// A parent that starts a long-lived child in the same group, like `ray start --block`.
	script := "sleep 30 & echo $! > " + marker + "; wait"
	ctx, cancel := context.WithCancel(context.Background())
	wait := startOwned(ctx, "/bin/sh", "-c", script)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(marker)
	child := strings.TrimSpace(string(data))
	cancel()
	_ = wait()
	if _, err := os.Stat("/proc/" + child); err == nil {
		status, _ := os.ReadFile("/proc/" + child + "/status")
		if !strings.Contains(string(status), "State:\tZ") {
			t.Fatalf("child %s outlived its stopped process group", child)
		}
	}
}
