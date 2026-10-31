//go:build linux

package agent

import (
	"context"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw/supervision"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeLimits(t *testing.T, dir, cpu, memory string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"cpu.max": cpu, "memory.max": memory} {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCgroupCapacityHonorsAncestorAndAffinity(t *testing.T) {
	mount := t.TempDir()
	current := filepath.Join(mount, "parent", "child")
	writeLimits(t, mount, "max 100000", "max")
	writeLimits(t, filepath.Dir(current), "150000 100000", "100")
	writeLimits(t, current, "2000000 100000", "253403070464")
	for _, tc := range []struct {
		affinity int
		want     string
	}{{192, "1500m"}, {1, "1000m"}} {
		got, err := readCgroupCapacity(mount, current, tc.affinity)
		if err != nil {
			t.Fatal(err)
		}
		if got["cpu"] != tc.want || got["memory"] != "100" {
			t.Fatalf("unexpected capacity: %#v", got)
		}
	}
}
func TestCgroupLimitsInvalidUnlimitedAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		cpu, memory string
		valid       bool
	}{
		{"max 100000", "max", true}, {"9223372036854775807 1", "0", true},
		{"max bad", "100", false}, {"bad", "100", false}, {"1 0", "100", false}, {"-1 1", "100", false},
		{"1 1", "", false}, {"1 1", "bad", false}, {"1 1", "-1", false}, {"1 1", strings.Repeat("9", 150), false},
	} {
		t.Run(tc.cpu+"/"+tc.memory, func(t *testing.T) {
			dir := t.TempDir()
			writeLimits(t, dir, tc.cpu, tc.memory)
			got, err := readCgroupCapacity(dir, dir, 2)
			if (err == nil) != tc.valid {
				t.Fatalf("capacity=%v error=%v", got, err)
			}
			if tc.valid && got["cpu"] != "2000m" {
				t.Fatal(got)
			}
		})
	}
}
func TestCgroupPathRejectsAmbiguityTraversalAndV1(t *testing.T) {
	for _, data := range []string{"1:cpu:/foo", "0::/../foo", "0::/a\n0::/b", "0::relative"} {
		if _, err := cgroupPath("/sys/fs/cgroup", data); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	if got, err := cgroupPath("/sys/fs/cgroup", "0::/parent/child\n"); err != nil || got != "/sys/fs/cgroup/parent/child" {
		t.Fatalf("%q %v", got, err)
	}
}
func TestLocalObserverRequiresAuthoritativeSupervisor(t *testing.T) {
	for _, socket := range []string{"", filepath.Join(t.TempDir(), "absent.sock")} {
		o := LocalObserver{Version: "v1", PrivateAddress: "10.0.3.85", SupervisorSocket: socket}
		if _, err := o.Observe(context.Background()); err == nil {
			t.Fatal("reported idle without supervisor")
		}
	}
}
func TestGPUObservationRejectsDuplicatesAndInvalidIDs(t *testing.T) {
	uuid := "GPU-d7711468-9cea-865b-0793-c1c66b6d2c40"
	if ids, err := parseGPUs(uuid + "\n"); err != nil || len(ids) != 1 {
		t.Fatalf("%v %v", ids, err)
	}
	for _, value := range []string{uuid + "\n" + uuid, "GPU-unknown", "MIG-uuid", strings.Repeat(uuid+"\n", 257)} {
		if _, err := parseGPUs(value); err == nil {
			t.Fatal("invalid GPU output accepted")
		}
	}
	var output boundedOutput
	if _, err := output.Write(make([]byte, 64<<10+1)); err == nil {
		t.Fatal("unbounded GPU output accepted")
	}
}

// TestCloudRawObservation exercises real cgroup, GPU and Unix-socket boundaries.
// It is opt-in because ordinary developer hosts need not have GPUs or root.
func TestCloudRawObservation(t *testing.T) {
	if os.Getenv("RLARK_TEST_LOCAL_OBSERVATION") != "1" {
		t.Skip("set RLARK_TEST_LOCAL_OBSERVATION=1 on the prepared trusted-root GPU container")
	}
	worker, err := filepath.EvalSymlinks("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	service, err := supervision.Open(supervision.Config{Backend: "process-tree", WorkerExecutable: worker, StateDirectory: state, Executables: []string{worker}, WorkDirectories: []string{t.TempDir()}, LogBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Serve(ctx) }()
	socket := filepath.Join(state, "supervisor.sock")
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket did not appear")
		}
		time.Sleep(time.Millisecond)
	}
	observer := LocalObserver{Version: "test", PrivateAddress: "10.0.3.85", SupervisorSocket: socket}
	observation, err := observer.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Busy || len(observation.VisibleGPUUUIDs) == 0 || observation.Capacity["cpu"] == "" {
		t.Fatalf("incomplete real observation: %+v", observation)
	}
	if len(observation.Capabilities) != 0 {
		t.Fatal("disabled updater advertised an upgrade capability")
	}
	observer.EnableUpgrades = true
	upgradeObservation, err := observer.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(upgradeObservation.Capabilities, UpgradeCapability) || !slices.Contains(upgradeObservation.Capabilities, "agent-platform/"+runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatal("enabled updater did not report its supported protocol and executable platform")
	}
	t.Logf("observed current resource boundary: cpu=%s memory=%s visibleGPUs=%d idle=%v", observation.Capacity["cpu"], observation.Capacity["memory"], len(observation.VisibleGPUUUIDs), !observation.Busy)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Observe(ctx); err == nil {
		t.Fatal("closed supervisor reported idle")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor shutdown blocked")
	}
}
