//go:build linux

package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/google/uuid"
)

// UpgradeTransport is the authenticated remote boundary used by the updater.
type UpgradeTransport interface {
	Upgrade(context.Context, UpgradeRequest) (*UpgradeResponse, error)
	TargetRelease(context.Context, string, string) (*ReleaseManifest, error)
	DownloadRelease(context.Context, *ReleaseManifest) ([]byte, error)
}

// RestartFunc replaces the current Agent process, preserving its arguments and
// environment. It returns only on failure. It must never restart the supervisor.
type RestartFunc func(context.Context, string) error

// UpdaterConfig binds a private journal to one installation and authenticated
// endpoint/credential identity. BootstrapVersion is used only before a journal
// exists; subsequent versions are derived from the running executable digest.
type UpdaterConfig struct {
	Paths            UpdaterPaths
	Scope            string
	BootstrapVersion string
}

// Updater retains both installation and state locks until Close or exec. A
// pending journal always takes precedence over discovering another operation.
type Updater struct {
	mu                     sync.Mutex
	files                  *updaterFiles
	config                 UpdaterConfig
	transport              UpgradeTransport
	restart                RestartFunc
	runningDigest, version string
}

// NewUpdater validates local state and acquires exclusive locks without starting
// background work. The caller owns Close and must heartbeat Version() before Step.
func NewUpdater(config UpdaterConfig, transport UpgradeTransport, restart RestartFunc) (*Updater, error) {
	return newUpdater(config, transport, restart, "/proc/self/exe")
}
func newUpdater(config UpdaterConfig, transport UpgradeTransport, restart RestartFunc, runningPath string) (*Updater, error) {
	if transport == nil || restart == nil || len(config.Scope) != 64 || config.BootstrapVersion == "" {
		return nil, errors.New("updater transport, restart, scope digest and bootstrap version required")
	}
	scope, err := hex.DecodeString(config.Scope)
	if err != nil || len(scope) != 32 || strings.ToLower(config.Scope) != config.Scope || strings.TrimSpace(config.BootstrapVersion) == "" || len(config.BootstrapVersion) > 128 || strings.ContainsAny(config.BootstrapVersion, "\x00\r\n") {
		return nil, ErrUpgradeIdentity
	}
	files, err := openUpdaterFiles(config.Paths)
	if err != nil {
		return nil, err
	}
	u := &Updater{files: files, config: config, transport: transport, restart: restart}
	fail := func(err error) (*Updater, error) { files.close(); return nil, err }
	u.runningDigest, err = fileDigest(runningPath)
	if err != nil {
		return fail(err)
	}
	installed, err := fileDigest(config.Paths.InstallPath)
	if err != nil {
		return fail(err)
	}
	receipt, err := files.load()
	if err != nil {
		return fail(err)
	}
	if receipt == nil {
		if installed != u.runningDigest {
			return fail(ErrUpgradeIdentity)
		}
		u.version = config.BootstrapVersion
		return u, nil
	}
	if err := u.validateJournal(receipt); err != nil {
		return fail(err)
	}
	if installed != receipt.SourceSHA256 && installed != receipt.Release.SHA256 {
		return fail(ErrUpgradeIdentity)
	}
	switch u.runningDigest {
	case receipt.SourceSHA256:
		u.version = receipt.SourceVersion
	case receipt.Release.SHA256:
		u.version = receipt.Release.Version
	default:
		return fail(ErrUpgradeIdentity)
	}
	if receipt.Phase == "Reserved" && (installed != receipt.SourceSHA256 || u.runningDigest != receipt.SourceSHA256) || receipt.Phase == "Succeeded" && (installed != receipt.Release.SHA256 || u.runningDigest != receipt.Release.SHA256) || (receipt.Phase == "RolledBack" || receipt.Phase == "Abandoned") && (installed != receipt.SourceSHA256 || u.runningDigest != receipt.SourceSHA256) {
		return fail(ErrUpgradeIdentity)
	}
	return u, nil
}

// Version returns the version proven by the running executable, or the explicit
// bootstrap version on the first installation. It is immutable within a process.
func (u *Updater) Version() string { return u.version }

// Close releases updater locks. It is safe to call more than once.
func (u *Updater) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.files != nil {
		u.files.close()
		u.files = nil
	}
}

func (u *Updater) validateJournal(r *upgradeJournal) error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.Scope != u.config.Scope || r.InstallPath != u.config.Paths.InstallPath || r.Release.OS != runtime.GOOS || r.Release.Architecture != runtime.GOARCH {
		return ErrUpgradeIdentity
	}
	return nil
}
func releaseMatches(m *ReleaseManifest, release UpgradeRelease) bool {
	return m != nil && m.Namespace == release.Namespace && m.Name == release.Name && m.UID == release.UID && m.Version == release.Version && m.OS == release.OS && m.Architecture == release.Architecture && m.Protocol == release.Protocol && m.SHA256 == release.SHA256
}
func (r *upgradeJournal) matches(a *UpgradeResponse) bool {
	return a != nil && a.Operation == r.Operation && (a.Allocation == nil || *a.Allocation == r.Allocation) && a.FromVersion == r.SourceVersion && releaseMatches(&r.Release, a.Release)
}
func (r *upgradeJournal) request(action string) UpgradeRequest {
	return UpgradeRequest{Action: action, Operation: r.Operation, Allocation: r.Allocation}
}

// Step reconciles after a successful, fresh heartbeat of obs. Calls are serialized
// with Close. Errors leave durable intent and the server allocation intact.
func (u *Updater) Step(ctx context.Context, obs Observation) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.files == nil {
		return errors.New("agent updater closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if obs.Version != u.version {
		return ErrUpgradeIdentity
	}
	if obs.Busy {
		return nil
	}
	r, err := u.files.load()
	if err != nil {
		return err
	}
	if r != nil {
		if err := u.validateJournal(r); err != nil {
			return err
		}
	}
	if r == nil || r.Phase == "Succeeded" || r.Phase == "RolledBack" || r.Phase == "Abandoned" {
		a, err := u.transport.Upgrade(ctx, UpgradeRequest{Action: "assignment"})
		if err != nil || a == nil {
			return err
		}
		if r != nil && a.Operation == r.Operation {
			if !r.matches(a) {
				return ErrUpgradeIdentity
			}
			if r.Phase != "Abandoned" && a.AcknowledgedAttemptID != r.AttemptID {
				return u.acknowledge(ctx, r, r.Phase, u.version)
			}
			return nil
		}
		if a.Phase != "Reserved" {
			return nil
		}
		if err := validateUpgradeResponse(UpgradeRequest{Action: "assignment"}, a); err != nil {
			return err
		}
		if a.Allocation == nil || a.FromVersion != u.version || a.Release.Version == u.version || a.Release.SHA256 == u.runningDigest || a.Release.OS != runtime.GOOS || a.Release.Architecture != runtime.GOARCH {
			return ErrUpgradeIdentity
		}
		installed, err := fileDigest(u.config.Paths.InstallPath)
		if err != nil {
			return err
		}
		if installed != u.runningDigest {
			return ErrUpgradeIdentity
		}
		manifest, err := u.transport.TargetRelease(ctx, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return err
		}
		if !releaseMatches(manifest, a.Release) {
			return ErrUpgradeIdentity
		}
		if _, err := manifestSpec(manifest); err != nil {
			return err
		}
		r = &upgradeJournal{Scope: u.config.Scope, InstallPath: u.config.Paths.InstallPath, Operation: a.Operation, Allocation: *a.Allocation, ResourceVersion: a.ResourceVersion, AttemptID: uuid.NewString(), SourceVersion: u.version, SourceSHA256: u.runningDigest, Release: *manifest, Phase: "Reserved"}
		// Persist caller-retained attempt before the first possibly successful claim.
		if err := u.files.save(r); err != nil {
			return err
		}
	}
	a, err := u.transport.Upgrade(ctx, r.request("get"))
	if err != nil {
		return err
	}
	if !r.matches(a) {
		return ErrUpgradeIdentity
	}
	if r.Phase == "Rollback" {
		if u.runningDigest != r.SourceSHA256 {
			return ErrUpgradeIdentity
		}
		if err := u.files.rollback(r); err != nil {
			return err
		}
		return u.complete(ctx, r, "RolledBack", r.SourceVersion)
	}
	if r.Phase == "Switching" && u.runningDigest == r.Release.SHA256 {
		installed, err := fileDigest(u.config.Paths.InstallPath)
		if err != nil {
			return err
		}
		if installed != r.Release.SHA256 {
			return ErrUpgradeIdentity
		}
		return u.complete(ctx, r, "Succeeded", r.Release.Version)
	}
	if u.runningDigest != r.SourceSHA256 {
		return ErrUpgradeIdentity
	}
	if a.Phase == "Cancelled" && r.Phase == "Reserved" && a.AttemptID == "" && a.Completion == nil {
		r.Phase = "Abandoned"
		return u.files.save(r)
	}
	if a.Allocation == nil || a.Phase != "Reserved" && a.Phase != "Applying" || a.Phase == "Applying" && a.AttemptID != r.AttemptID || a.Completion != nil {
		return ErrUpgradeConflict
	}
	claim := r.request("claim")
	claim.AttemptID = r.AttemptID
	claim.ResourceVersion = a.ResourceVersion
	granted, err := u.transport.Upgrade(ctx, claim)
	if err != nil {
		return err
	}
	if err := validateUpgradeResponse(claim, granted); err != nil {
		return err
	}
	if !r.matches(granted) || !granted.Granted {
		return ErrUpgradeIdentity
	}
	manifest, err := u.transport.TargetRelease(ctx, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if !releaseMatches(manifest, granted.Release) || *manifest != r.Release {
		return ErrUpgradeIdentity
	}
	data, err := u.transport.DownloadRelease(ctx, manifest)
	if err != nil {
		return err
	}
	spec, err := manifestSpec(manifest)
	if err != nil {
		return err
	}
	if int64(len(data)) != manifest.Bytes {
		return ErrReleaseIdentity
	}
	if err := artifact.Verify(spec, data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Renew the exact grant after download, before any installation effect.
	granted, err = u.transport.Upgrade(ctx, claim)
	if err != nil {
		return err
	}
	if err := validateUpgradeResponse(claim, granted); err != nil {
		return err
	}
	if !r.matches(granted) || !granted.Granted {
		return ErrUpgradeIdentity
	}
	r.Phase = "Switching"
	if err := u.files.save(r); err != nil {
		return err
	}
	if err := u.files.install(r, data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	} // Durable Switching is resumed on restart.
	err = u.restart(ctx, u.config.Paths.InstallPath)
	if err == nil {
		return errors.New("agent process replacement unexpectedly returned")
	}
	// Record rollback intent before touching the target. Restart of the old binary
	// can reconstruct this transition even if restoring the backup is interrupted.
	r.Phase = "Rollback"
	if saveErr := u.files.save(r); saveErr != nil {
		return errors.Join(fmt.Errorf("replace Agent process: %w", err), saveErr)
	}
	if rollbackErr := u.files.rollback(r); rollbackErr != nil {
		return errors.Join(fmt.Errorf("replace Agent process: %w", err), rollbackErr)
	}
	return fmt.Errorf("replace Agent process; source restored, awaiting fresh idle heartbeat: %w", err)
}
func (u *Updater) complete(ctx context.Context, r *upgradeJournal, result, version string) error {
	idle := true
	req := r.request("complete")
	req.AttemptID = r.AttemptID
	req.Result = result
	req.Version = version
	req.Idle = &idle
	response, err := u.transport.Upgrade(ctx, req)
	if err != nil {
		return err
	}
	if err := validateUpgradeResponse(req, response); err != nil {
		return err
	}
	if !r.matches(response) || response.AttemptID != r.AttemptID {
		return ErrUpgradeIdentity
	}
	r.Phase = result
	// Retain verified running digest/version across service restarts with bootstrap arguments.
	if err := u.files.save(r); err != nil {
		return err
	}
	return u.acknowledge(ctx, r, result, version)
}

func (u *Updater) acknowledge(ctx context.Context, r *upgradeJournal, result, version string) error {
	idle := true
	req := r.request("acknowledge")
	req.AttemptID, req.Result, req.Version, req.Idle = r.AttemptID, result, version, &idle
	response, err := u.transport.Upgrade(ctx, req)
	if err != nil {
		return err
	}
	if err := validateUpgradeResponse(req, response); err != nil {
		return err
	}
	if !r.matches(response) || response.AttemptID != r.AttemptID {
		return ErrUpgradeIdentity
	}
	return nil
}

// UpgradingHeartbeater serializes upgrade reconciliation after a successful
// observation heartbeat when workload execution is disabled.
type UpgradingHeartbeater struct {
	Client  Heartbeater
	Updater *Updater
}

// Heartbeat never attempts an upgrade when heartbeat publication fails.
func (h UpgradingHeartbeater) Heartbeat(ctx context.Context, obs Observation) error {
	if h.Client == nil || h.Updater == nil {
		return errors.New("heartbeat client and updater required")
	}
	if err := h.Client.Heartbeat(ctx, obs); err != nil {
		return err
	}
	return h.Updater.Step(ctx, obs)
}
