//go:build linux

package docker

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

func withPorts(t *testing.T, req rt.ApplyRequest, ports []int32) rt.ApplyRequest {
	t.Helper()
	m, err := decodeManifest(req.Desired.Manifest.Object)
	if err != nil {
		t.Fatal(err)
	}
	m.Spec.RayPorts = ports
	req.Desired, err = DesiredContainer(req.Task, m.Spec)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestScopedPrepareRestartAndSeparatelyAuthorizedStart(t *testing.T) {
	p, e, req, path := scopedFixture(t)
	ctx := context.Background()
	req = withPorts(t, req, []int32{24000, 24001})
	probes := 0
	p.PortPreflight = func(_ context.Context, ports []int32) error {
		probes++
		if !slices.Equal(ports, []int32{24000, 24001}) {
			t.Fatal(ports)
		}
		return nil
	}
	first, err := p.Prepare(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if e.creates != 1 || e.starts != 0 || e.current.State.Status != "created" {
		t.Fatal("prepare started or failed to create")
	}
	record, err := p.record(ctx, req.Task)
	if err != nil || record == nil || record.ContainerID != first.Ref.ID || !record.Creating || record.Cancelled {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	reopen(t, p, path)
	again, err := p.Prepare(ctx, req)
	if err != nil || first.Ref != again.Ref || e.creates != 1 || e.starts != 0 {
		t.Fatalf("prepare recovery=%+v err=%v", again, err)
	}
	deny := true
	p.Admission = admissionFunc(func(context.Context, rt.TaskIdentity) error {
		if deny {
			return fault.New(fault.PermissionDenied, "start denied")
		}
		return nil
	})
	if _, err := p.Apply(ctx, req); !fault.Is(err, fault.PermissionDenied) {
		t.Fatal(err)
	}
	if e.starts != 0 {
		t.Fatal("started after admission revoked")
	}
	deny = false
	if _, err := p.Apply(ctx, req); err != nil {
		t.Fatal(err)
	}
	if e.creates != 1 || e.starts != 1 {
		t.Fatalf("effects create=%d start=%d", e.creates, e.starts)
	}
	oldProbes := probes
	p.PortPreflight = func(context.Context, []int32) error { return errors.New("own ports occupied") }
	if _, err := p.Apply(ctx, req); err != nil {
		t.Fatal("running recovery probed own ports", err)
	}
	if probes != oldProbes || e.starts != 1 {
		t.Fatal("running recovery restarted execution")
	}
	if _, err := p.Cleanup(ctx, req.Task); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Prepare(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatal("prepare crossed cancellation", err)
	}
}

func TestPreparedCreateLossPortConflictAndCleanupRecovery(t *testing.T) {
	p, e, req, path := scopedFixture(t)
	ctx := context.Background()
	req = withPorts(t, req, []int32{24000})
	blocked := true
	p.PortPreflight = func(context.Context, []int32) error {
		if blocked {
			return errors.New("foreign listener")
		}
		return nil
	}
	if _, err := p.Prepare(ctx, req); !fault.Is(err, fault.Unavailable) {
		t.Fatal(err)
	}
	record, err := p.record(ctx, req.Task)
	if err != nil || record == nil || record.Creating || e.creates != 0 {
		t.Fatalf("conflict created uncertainty: %+v %v", record, err)
	}
	blocked = false
	e.lostCreate = true
	if _, err := p.Prepare(ctx, req); err == nil {
		t.Fatal("lost create hidden")
	}
	reopen(t, p, path)
	e.lostCreate = false
	if _, err := p.Prepare(ctx, req); err != nil {
		t.Fatal(err)
	}
	blocked = true
	if _, err := p.Prepare(ctx, req); err == nil {
		t.Fatal("retained prepared container ignored new conflict")
	}
	if _, err := p.Apply(ctx, req); err == nil {
		t.Fatal("start ignored new conflict")
	}
	if e.starts != 0 || e.creates != 1 {
		t.Fatal("partial preparation duplicated/started")
	}
	if _, err := p.Cleanup(ctx, req.Task); err != nil {
		t.Fatal("port conflict blocked cleanup", err)
	}
	if e.current != nil {
		t.Fatal("prepared container not removed")
	}
}

func TestScopedStartRechecksAdmissionAfterPortProbe(t *testing.T) {
	p, e, req, _ := scopedFixture(t)
	ctx := context.Background()
	req = withPorts(t, req, []int32{24000})
	p.PortPreflight = func(context.Context, []int32) error { return nil }
	if _, err := p.Prepare(ctx, req); err != nil {
		t.Fatal(err)
	}
	deny := false
	p.Admission = admissionFunc(func(context.Context, rt.TaskIdentity) error {
		if deny {
			return errors.New("revoked")
		}
		return nil
	})
	p.PortPreflight = func(context.Context, []int32) error { deny = true; return nil }
	if _, err := p.Apply(ctx, req); err == nil {
		t.Fatal("start admission not rechecked")
	}
	if e.starts != 0 {
		t.Fatal("revoked execution started")
	}
}

func TestPortPlanMetadataIsImmutableAndInvalidListsFailBeforeIO(t *testing.T) {
	p, e, req, _ := scopedFixture(t)
	ctx := context.Background()
	req = withPorts(t, req, []int32{24000, 24001})
	p.PortPreflight = func(context.Context, []int32) error { return nil }
	if _, err := p.Prepare(ctx, req); err != nil {
		t.Fatal(err)
	}
	e.current.Config.Labels[hostPortsLabel] = `[24000,24002]`
	if _, err := p.Prepare(ctx, req); !fault.Is(err, fault.Conflict) {
		t.Fatalf("port drift=%v", err)
	}
	if e.starts != 0 || e.creates != 1 {
		t.Fatal("port drift changed execution")
	}
	for _, ports := range [][]int32{{0}, {1023}, {65536}, {24000, 24000}, make([]int32, 513)} {
		if ValidateHostPortPlan(ports) == nil {
			t.Fatalf("accepted %v", ports)
		}
	}
}

func TestRealSocketPreflightDetectsForeignIPv4AndIPv6AndHonorsCancellation(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			address := "127.0.0.1:0"
			if network == "tcp6" {
				address = "[::1]:0"
			}
			listener, err := net.Listen(network, address)
			if err != nil {
				t.Skipf("address family unavailable: %v", err)
			}
			port := int32(listener.Addr().(*net.TCPAddr).Port)
			if err := CheckHostPorts(context.Background(), []int32{port}); err == nil {
				t.Fatal("foreign listener not detected")
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if err := CheckHostPorts(context.Background(), []int32{port}); err != nil {
				t.Fatal("port not released by preflight", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := CheckHostPorts(ctx, []int32{port}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
