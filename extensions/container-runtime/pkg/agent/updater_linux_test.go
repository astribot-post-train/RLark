//go:build linux

package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type fakeUpgradeTransport struct {
	operation, allocation ext.NativeObjectReference
	release               ReleaseManifest
	data                  []byte
	phase                 string
	attempt               string
	calls                 []string
	hook                  func(UpgradeRequest, *UpgradeResponse) (*UpgradeResponse, error)
	downloadHook          func([]byte) []byte
	manifestHook          func(*ReleaseManifest) *ReleaseManifest
}

func (f *fakeUpgradeTransport) Upgrade(ctx context.Context, req UpgradeRequest) (*UpgradeResponse, error) {
	r, err := f.upgrade(ctx, req)
	if err == nil && f.hook != nil {
		return f.hook(req, r)
	}
	return r, err
}
func (f *fakeUpgradeTransport) upgrade(_ context.Context, req UpgradeRequest) (*UpgradeResponse, error) {
	f.calls = append(f.calls, req.Action)
	response := func() *UpgradeResponse {
		return &UpgradeResponse{Operation: f.operation, Allocation: &f.allocation, Phase: f.phase, AttemptID: f.attempt, ResourceVersion: "rv2", Release: UpgradeRelease{Namespace: f.release.Namespace, Name: f.release.Name, UID: f.release.UID, Version: f.release.Version, OS: f.release.OS, Architecture: f.release.Architecture, Protocol: f.release.Protocol, SHA256: f.release.SHA256}, FromVersion: "old"}
	}
	switch req.Action {
	case "assignment":
		if f.phase == "" {
			f.phase = "Reserved"
		}
		return response(), nil
	case "get":
		return response(), nil
	case "claim":
		f.phase = "Applying"
		f.attempt = req.AttemptID
		r := response()
		r.Granted = true
		now := metav1.NewTime(time.Now())
		r.StartedAt = &now
		return r, nil
	case "complete":
		f.phase = "Applying"
		r := response()
		r.Completion = &ext.AgentUpgradeCompletion{AttemptID: req.AttemptID, Result: req.Result, Version: req.Version}
		return r, nil
	case "acknowledge":
		r := response()
		r.Completion = &ext.AgentUpgradeCompletion{AttemptID: req.AttemptID, Result: req.Result, Version: req.Version}
		r.AcknowledgedAttemptID = req.AttemptID
		return r, nil
	default:
		return nil, errors.New("unexpected action")
	}
}
func (f *fakeUpgradeTransport) TargetRelease(context.Context, string, string) (*ReleaseManifest, error) {
	if f.manifestHook != nil {
		return f.manifestHook(&f.release), nil
	}
	return &f.release, nil
}
func (f *fakeUpgradeTransport) DownloadRelease(context.Context, *ReleaseManifest) ([]byte, error) {
	if f.downloadHook != nil {
		return f.downloadHook(f.data), nil
	}
	return f.data, nil
}

func TestUpdaterPersistsInstallFailureAndResumesRollback(t *testing.T) {
	source, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	release, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	installDir := filepath.Join(dir, "install")
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(installDir, 0700); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(installDir, "agent")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(install, source, 0700); err != nil {
		t.Fatal(err)
	}
	digest := binaryDigest(source)
	releaseDigest := binaryDigest(release)
	scope := digest
	tr := &fakeUpgradeTransport{operation: ext.NativeObjectReference{Namespace: "ns", Name: "op", UID: "opuid"}, allocation: ext.NativeObjectReference{Namespace: "ns", Name: "alloc", UID: "allocuid"}, release: ReleaseManifest{Namespace: "ns", Name: "rel", UID: "reluid", Version: "next", OS: "linux", Architecture: runtime.GOARCH, Protocol: "v1", SHA256: releaseDigest, Bytes: int64(len(release))}, data: release}
	restart := func(context.Context, string) error { return errors.New("exec failed") }
	u, err := newUpdater(UpdaterConfig{Paths: UpdaterPaths{InstallPath: install, StateDirectory: state}, Scope: scope, BootstrapVersion: "old"}, tr, restart, "/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Step(context.Background(), Observation{Version: "old"}); err == nil {
		t.Fatal("expected replacement failure")
	}
	if got, _ := fileDigest(install); got != digest {
		t.Fatalf("install not restored: %s", got)
	}
	if _, err := os.Stat(filepath.Join(state, "upgrade.json")); err != nil {
		t.Fatalf("rollback receipt missing: %v", err)
	}
	u.Close()
	u, err = newUpdater(UpdaterConfig{Paths: UpdaterPaths{InstallPath: install, StateDirectory: state}, Scope: scope, BootstrapVersion: "old"}, tr, restart, "/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Step(context.Background(), Observation{Version: "old"}); err != nil {
		t.Fatal(err)
	}
	receipt, err := u.files.load()
	if err != nil || receipt.Phase != "RolledBack" {
		t.Fatalf("receipt not finalized: %+v %v", receipt, err)
	}
	u.Close()
}

type updaterFixture struct {
	config       UpdaterConfig
	transport    *fakeUpgradeTransport
	source       string
	sourceDigest string
}

func setupUpdater(t *testing.T) updaterFixture {
	t.Helper()
	dir := t.TempDir()
	installDir := filepath.Join(dir, "install")
	state := filepath.Join(dir, "state")
	mustAgent(t, os.Mkdir(installDir, 0700))
	mustAgent(t, os.Mkdir(state, 0700))
	// Use a small, real ELF source; the target remains a distinct valid ELF.
	source, err := os.ReadFile("/bin/true")
	mustAgent(t, err)
	target, err := os.ReadFile("/proc/self/exe")
	mustAgent(t, err)
	install := filepath.Join(installDir, "agent")
	mustAgent(t, os.WriteFile(install, source, 0700))
	sourcePath := filepath.Join(dir, "running-source")
	mustAgent(t, os.WriteFile(sourcePath, source, 0700))
	config := UpdaterConfig{Paths: UpdaterPaths{InstallPath: install, StateDirectory: state}, Scope: binaryDigest([]byte("certificate and endpoint")), BootstrapVersion: "old"}
	tr := &fakeUpgradeTransport{operation: ext.NativeObjectReference{Namespace: "ns", Name: "op", UID: "opuid"}, allocation: ext.NativeObjectReference{Namespace: "ns", Name: "alloc", UID: "allocuid"}, release: ReleaseManifest{Namespace: "ns", Name: "rel", UID: "reluid", Version: "next", OS: "linux", Architecture: runtime.GOARCH, Protocol: "v1", SHA256: binaryDigest(target), Bytes: int64(len(target))}, data: target}
	return updaterFixture{config: config, transport: tr, source: sourcePath, sourceDigest: binaryDigest(source)}
}
func (f updaterFixture) open(t *testing.T, restart RestartFunc, running string) *Updater {
	t.Helper()
	if running == "" {
		running = f.source
	}
	u, err := newUpdater(f.config, f.transport, restart, running)
	mustAgent(t, err)
	t.Cleanup(u.Close)
	return u
}
func TestUpdaterLostClaimAndCompletionSurviveRestart(t *testing.T) {
	f := setupUpdater(t)
	claimLost, completionLost := false, false
	attempts := map[string]bool{}
	f.transport.hook = func(req UpgradeRequest, r *UpgradeResponse) (*UpgradeResponse, error) {
		if req.Action == "claim" {
			attempts[req.AttemptID] = true
			if !claimLost {
				claimLost = true
				return nil, errors.New("claim committed; response lost")
			}
		}
		if req.Action == "complete" && !completionLost {
			completionLost = true
			return nil, errors.New("completion committed; response lost")
		}
		return r, nil
	}
	restartCount := 0
	restart := func(context.Context, string) error { restartCount++; return nil } // Successful exec simulated by opening installed bytes next.
	u := f.open(t, restart, "")
	if err := u.Step(context.Background(), Observation{Version: "old"}); err == nil {
		t.Fatal("lost claim not injected")
	}
	if restartCount != 0 {
		t.Fatal("installed after uncertain claim")
	}
	u.Close()
	u = f.open(t, restart, "")
	if err := u.Step(context.Background(), Observation{Version: "old"}); err == nil {
		t.Fatal("exec unexpectedly returned without error")
	}
	if restartCount != 1 || len(attempts) != 1 {
		t.Fatalf("duplicate attempt/restart: %d %#v", restartCount, attempts)
	}
	u.Close()
	u = f.open(t, restart, f.config.Paths.InstallPath)
	if u.Version() != "next" {
		t.Fatal("bootstrap version overrode running digest")
	}
	if err := u.Step(context.Background(), Observation{Version: u.Version()}); err == nil {
		t.Fatal("lost completion not injected")
	}
	u.Close()
	u = f.open(t, restart, f.config.Paths.InstallPath)
	mustAgent(t, u.Step(context.Background(), Observation{Version: u.Version()}))
	receipt, err := u.files.load()
	mustAgent(t, err)
	if receipt.Phase != "Succeeded" {
		t.Fatal(receipt.Phase)
	}
	u.Close()
	u = f.open(t, restart, f.config.Paths.InstallPath)
	mustAgent(t, u.Step(context.Background(), Observation{Version: u.Version()}))
	if restartCount != 1 || len(attempts) != 1 {
		t.Fatal("replayed finished installation")
	}
}
func TestUpdaterRejectsUnprovenInstallation(t *testing.T) {
	for _, mode := range []string{"digest", "manifest", "allocation", "attempt", "journal-write", "install-rename", "backup-symlink", "staging-symlink", "busy", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := setupUpdater(t)
			restarted := false
			u := f.open(t, func(context.Context, string) error { restarted = true; return nil }, "")
			obs := Observation{Version: "old"}
			ctx := context.Background()
			switch mode {
			case "digest":
				f.transport.downloadHook = func(data []byte) []byte { copyData := append([]byte{}, data...); copyData[0] ^= 1; return copyData }
			case "manifest":
				f.transport.manifestHook = func(m *ReleaseManifest) *ReleaseManifest { cp := *m; cp.UID = "foreign"; return &cp }
			case "allocation":
				f.transport.hook = func(req UpgradeRequest, r *UpgradeResponse) (*UpgradeResponse, error) {
					if req.Action == "get" {
						r.Allocation = nil
					}
					return r, nil
				}
			case "attempt":
				f.transport.hook = func(req UpgradeRequest, r *UpgradeResponse) (*UpgradeResponse, error) {
					if req.Action == "get" {
						r.Phase = "Applying"
						r.AttemptID = "22222222-2222-2222-2222-222222222222"
					}
					return r, nil
				}
			case "journal-write":
				mustAgent(t, os.Mkdir(u.files.receiptPath(), 0700))
			case "install-rename":
				f.transport.downloadHook = func(data []byte) []byte {
					mustAgent(t, os.Remove(f.config.Paths.InstallPath))
					mustAgent(t, os.Mkdir(f.config.Paths.InstallPath, 0700))
					return data
				}
			case "backup-symlink":
				mustAgent(t, os.Symlink(f.source, f.config.Paths.InstallPath+".backup"))
			case "staging-symlink":
				mustAgent(t, os.Symlink(f.source, filepath.Join(f.config.Paths.StateDirectory, "agent-staged")))
			case "busy":
				obs.Busy = true
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := u.Step(ctx, obs)
			if mode != "busy" && err == nil {
				t.Fatal("unsafe installation accepted")
			}
			if restarted {
				t.Fatal("restarted despite unproven state")
			}
			if mode != "install-rename" {
				digest, err := fileDigest(f.config.Paths.InstallPath)
				mustAgent(t, err)
				if digest != f.sourceDigest {
					t.Fatal("installed target despite failure")
				}
			}
			if mode == "busy" || mode == "cancelled" || mode == "journal-write" {
				if len(f.transport.calls) != 0 {
					t.Fatalf("unexpected server effect: %v", f.transport.calls)
				}
			}
		})
	}
}
func TestUpdaterRejectsSharedPathsAndForeignResume(t *testing.T) {
	for _, mode := range []string{"concurrent", "same-install-other-state", "same-state-other-install", "symlink", "shared-state", "corrupt-journal", "scope", "running-binary"} {
		t.Run(mode, func(t *testing.T) {
			f := setupUpdater(t)
			restart := func(context.Context, string) error { return errors.New("no exec") }
			cfg := f.config
			running := f.source
			switch mode {
			case "concurrent":
				f.open(t, restart, "")
			case "same-install-other-state":
				f.open(t, restart, "")
				cfg.Paths.StateDirectory = t.TempDir()
			case "same-state-other-install":
				f.open(t, restart, "")
				cfg.Paths.InstallPath = filepath.Join(t.TempDir(), "other")
				data, err := os.ReadFile(f.source)
				mustAgent(t, err)
				mustAgent(t, os.WriteFile(cfg.Paths.InstallPath, data, 0700))
			case "symlink":
				mustAgent(t, os.Remove(cfg.Paths.InstallPath))
				mustAgent(t, os.Symlink(f.source, cfg.Paths.InstallPath))
			case "shared-state":
				mustAgent(t, os.Chmod(cfg.Paths.StateDirectory, 0755))
			case "corrupt-journal":
				mustAgent(t, os.WriteFile(filepath.Join(cfg.Paths.StateDirectory, "upgrade.json"), []byte(`{"phase":"Succeeded"}`), 0600))
			case "scope":
				u := f.open(t, restart, "")
				_ = u.Step(context.Background(), Observation{Version: "old"})
				u.Close()
				cfg.Scope = binaryDigest([]byte("foreign credential"))
			case "running-binary":
				running = "/proc/self/exe"
			}
			u, err := newUpdater(cfg, f.transport, restart, running)
			if err == nil {
				u.Close()
				t.Fatal("unsafe startup accepted")
			}
		})
	}
}

func TestUpdaterMTLSInstallationAndStartupCompletion(t *testing.T) {
	f := newUpgradeTransportFixture(t)
	// Use the real scoped handlers, retained allocation and immutable artifact.
	now := time.Now().Truncate(time.Second)
	f.reporter.Now = func() time.Time { return now }
	pool, certificate := agentCertificate(t, f.identity.Metadata())
	server := httptest.NewUnstartedServer(Handler{Reporter: f.reporter, EnableUpgrades: true})
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport, err := NewClient(server.URL, certificate, roots)
	mustAgent(t, err)
	defer transport.HTTPClient.CloseIdleConnections()
	local := setupUpdater(t)
	local.config.Scope = binaryDigest(certificate.Certificate[0])
	restart := func(context.Context, string) error { return nil }
	u, err := newUpdater(local.config, transport, restart, local.source)
	mustAgent(t, err)
	defer u.Close()
	obs := Observation{Version: u.Version(), PrivateAddress: "10.0.3.85", VisibleGPUUUIDs: []string{"GPU-a"}}
	mustAgent(t, transport.Heartbeat(context.Background(), obs))
	if err := u.Step(context.Background(), obs); err == nil {
		t.Fatal("simulated exec unexpectedly returned")
	}
	journal, err := u.files.load()
	mustAgent(t, err)
	if journal == nil || journal.Phase != "Switching" {
		t.Fatalf("no durable switch: %+v", journal)
	}
	var operation ext.AgentUpgrade
	mustAgent(t, f.base.Get(context.Background(), client.ObjectKeyFromObject(f.op), &operation))
	if operation.Status.Phase != "Applying" || operation.Status.Completion != nil {
		t.Fatal("completed before target heartbeat")
	}
	u.Close()
	now = now.Add(time.Second)
	u, err = newUpdater(local.config, transport, restart, local.config.Paths.InstallPath)
	mustAgent(t, err)
	defer u.Close()
	if u.Version() != "next" {
		t.Fatal("wrong running version")
	}
	obs.Version = u.Version()
	mustAgent(t, transport.Heartbeat(context.Background(), obs))
	mustAgent(t, u.Step(context.Background(), obs))
	mustAgent(t, f.base.Get(context.Background(), client.ObjectKeyFromObject(f.op), &operation))
	if operation.Status.Phase != "Applying" || operation.Status.Completion == nil || operation.Status.Completion.Result != "Succeeded" {
		t.Fatalf("missing exact completion: %+v", operation.Status)
	}
	var lock ext.ContainerAllocation
	mustAgent(t, f.base.Get(context.Background(), client.ObjectKeyFromObject(f.alloc), &lock))
	if lock.Status.CleanupConfirmed || !lock.DeletionTimestamp.IsZero() {
		t.Fatal("updater released controller-owned lock")
	}
}

func TestUpdaterExecPreservesProcessAndResumes(t *testing.T) {
	f := setupUpdater(t)
	// Give the target a distinct digest while retaining an executable test binary.
	source, err := os.ReadFile("/proc/self/exe")
	mustAgent(t, err)
	mustAgent(t, os.WriteFile(f.config.Paths.InstallPath, source, 0700))
	f.transport.data = append(source, byte(0))
	f.transport.release.SHA256 = binaryDigest(f.transport.data)
	f.transport.release.Bytes = int64(len(f.transport.data))
	payload, err := json.Marshal(struct {
		Config  UpdaterConfig
		Release ReleaseManifest
	}{f.config, f.transport.release})
	mustAgent(t, err)
	targetFile := filepath.Join(t.TempDir(), "target")
	mustAgent(t, os.WriteFile(targetFile, f.transport.data, 0700))
	cmd := exec.Command(f.config.Paths.InstallPath, "-test.run=^TestUpdaterExecHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "RLARK_TEST_UPDATER="+string(payload), "RLARK_TEST_UPDATER_TARGET="+targetFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real Agent exec/resume failed: %s %v", out, err)
	}
	var receipt upgradeJournal
	data, err := os.ReadFile(filepath.Join(f.config.Paths.StateDirectory, "upgrade.json"))
	mustAgent(t, err)
	mustAgent(t, json.Unmarshal(data, &receipt))
	if receipt.Phase != "Succeeded" {
		t.Fatal("real exec did not confirm target")
	}
}

func TestUpdaterExecHelper(t *testing.T) {
	payload := os.Getenv("RLARK_TEST_UPDATER")
	if payload == "" {
		t.Skip("exec helper subprocess only")
	}
	var inputs struct {
		Config  UpdaterConfig
		Release ReleaseManifest
	}
	mustAgent(t, json.Unmarshal([]byte(payload), &inputs))
	target, err := os.ReadFile(os.Getenv("RLARK_TEST_UPDATER_TARGET"))
	mustAgent(t, err)
	tr := &fakeUpgradeTransport{operation: ext.NativeObjectReference{Namespace: "ns", Name: "op", UID: "opuid"}, allocation: ext.NativeObjectReference{Namespace: "ns", Name: "alloc", UID: "allocuid"}, release: inputs.Release, data: target}
	u, err := NewUpdater(inputs.Config, tr, func(ctx context.Context, path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return syscall.Exec(path, append([]string{path}, os.Args[1:]...), os.Environ())
	})
	mustAgent(t, err)
	defer u.Close()
	pidPath := filepath.Join(inputs.Config.Paths.StateDirectory, "exec-pid")
	if u.Version() == "old" {
		mustAgent(t, os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0600))
	} else {
		pid, err := os.ReadFile(pidPath)
		mustAgent(t, err)
		if string(pid) != strconv.Itoa(os.Getpid()) {
			t.Fatal("exec changed PID")
		}
		r, err := u.files.load()
		mustAgent(t, err)
		tr.phase = "Applying"
		tr.attempt = r.AttemptID
	}
	mustAgent(t, u.Step(context.Background(), Observation{Version: u.Version()}))
	if u.Version() != "next" {
		t.Fatal("source survived successful exec")
	}
}

func TestUpdaterAcknowledgesOnlyAfterDurableJournalAndRetries(t *testing.T) {
	f := setupUpdater(t)
	lost := false
	acknowledged := false
	f.transport.hook = func(req UpgradeRequest, response *UpgradeResponse) (*UpgradeResponse, error) {
		if req.Action == "acknowledge" {
			data, err := os.ReadFile(filepath.Join(f.config.Paths.StateDirectory, "upgrade.json"))
			mustAgent(t, err)
			var journal upgradeJournal
			mustAgent(t, json.Unmarshal(data, &journal))
			if journal.Phase != "RolledBack" || journal.AttemptID != req.AttemptID {
				t.Fatal("acknowledgement preceded durable exact terminal journal")
			}
			if !lost {
				lost = true
				return nil, errors.New("acknowledgement response lost")
			}
			acknowledged = true
		}
		return response, nil
	}
	restart := func(context.Context, string) error { return errors.New("exec failed") }
	u := f.open(t, restart, "")
	if err := u.Step(context.Background(), Observation{Version: "old"}); err == nil {
		t.Fatal("exec failure missing")
	}
	if err := u.Step(context.Background(), Observation{Version: "old"}); err == nil || !lost {
		t.Fatal("lost acknowledgement missing")
	}
	u.Close()
	u = f.open(t, restart, "")
	mustAgent(t, u.Step(context.Background(), Observation{Version: "old"}))
	if !acknowledged {
		t.Fatal("persisted journal did not retry acknowledgement")
	}
}
