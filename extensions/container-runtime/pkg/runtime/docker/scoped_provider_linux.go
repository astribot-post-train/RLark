//go:build linux

package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/docker/docker/errdefs"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

// ScopedProvider serializes and journals Docker effects under a single private
// host owner. Reconstructing it must reuse the same Journal. Agent identity and
// allocation authorization remain independent of this local cancellation fence.
type ScopedProvider struct {
	*Provider
	Journal  *Journal
	EngineID string
	GPUUUIDs []string
	// StartAdmission is independently checked immediately before ContainerStart.
	// Group execution must supply it; create authorization cannot grant start.
	StartAdmission interface {
		Authorize(context.Context, rt.TaskIdentity) error
	}
}

var _ RuntimeProvider = (*ScopedProvider)(nil)

func (p *ScopedProvider) checkEngine(ctx context.Context) error {
	if p == nil || p.Provider == nil || p.Client == nil || p.Journal == nil || p.EngineID == "" {
		return errors.New("scoped Docker Engine, identity and journal required")
	}
	info, err := p.Client.Info(ctx)
	if err != nil {
		return fmt.Errorf("read bound Docker Engine: %w", err)
	}
	if info.ID != p.EngineID {
		return fault.New(fault.PermissionDenied, "Docker Engine incarnation changed")
	}
	return nil
}

// Apply durably records a create intent before contacting the daemon. An
// uncertain create with no ID is recoverable only when its exact container is
// observed; daemon absence alone cannot release that execution.
func (p *ScopedProvider) Apply(ctx context.Context, req rt.ApplyRequest) (rt.ApplyResult, error) {
	return p.ensureScoped(ctx, req, true, false)
}

// Prepare journals the same exact create identity as Apply, but leaves the
// container stopped. It does not publish a group readiness receipt or grant
// start permission; those remain separate admission responsibilities.
func (p *ScopedProvider) Prepare(ctx context.Context, req rt.ApplyRequest) (rt.ApplyResult, error) {
	return p.ensureScoped(ctx, req, false, false)
}

// StartPrepared starts only a journalled existing container. It cannot create
// or replace a missing prepared instance, including after an uncertain start.
func (p *ScopedProvider) StartPrepared(ctx context.Context, req rt.ApplyRequest) (rt.ApplyResult, error) {
	if p == nil || p.StartAdmission == nil {
		return rt.ApplyResult{}, errors.New("separate prepared-container start admission required")
	}
	if req.Current == nil || req.Current.ID == "" || req.Current.ID != req.Current.UID {
		return rt.ApplyResult{}, fault.New(fault.InvalidArgument, "exact prepared container reference required")
	}
	return p.ensureScoped(ctx, req, true, true)
}

func (p *ScopedProvider) ensureScoped(ctx context.Context, req rt.ApplyRequest, start, preparedOnly bool) (rt.ApplyResult, error) {
	if p == nil || p.Journal == nil {
		return rt.ApplyResult{}, errors.New("docker execution journal required")
	}
	if err := validateTask(req.Task); err != nil || req.Task.Generation < 1 {
		return rt.ApplyResult{}, errors.Join(errors.New("valid Docker Task generation required"), err)
	}
	unlock, err := p.Journal.acquire(ctx)
	if err != nil {
		return rt.ApplyResult{}, err
	}
	defer unlock()
	if err := p.checkEngine(ctx); err != nil {
		return rt.ApplyResult{}, err
	}
	if err := p.ValidateDesired(req.Desired); err != nil {
		return rt.ApplyResult{}, err
	}
	m, err := decodeManifest(req.Desired.Manifest.Object)
	if err != nil {
		return rt.ApplyResult{}, err
	}
	if !sameUUIDs(m.Spec.GPUUUIDs, p.GPUUUIDs) {
		return rt.ApplyResult{}, fault.New(fault.PermissionDenied, "Docker devices do not match the exact allocation")
	}
	sum := sha256.Sum256(req.Desired.Manifest.Object)
	digest := hex.EncodeToString(sum[:])
	record, err := p.Journal.read(req.Task)
	if err != nil {
		return rt.ApplyResult{}, err
	}
	if record != nil && (record.Cancelled || record.Digest != digest || record.Task.Generation != req.Task.Generation) {
		return rt.ApplyResult{}, fault.New(fault.Conflict, "Docker execution was cancelled or changed")
	}
	if preparedOnly && (record == nil || record.ContainerID == "" || !record.Creating) {
		return rt.ApplyResult{}, fault.New(fault.FailedPrecondition, "prepared container journal is absent")
	}
	if preparedOnly && record.ContainerID != req.Current.ID {
		return rt.ApplyResult{}, fault.New(fault.Conflict, "prepared receipt and local container journal disagree")
	}
	if p.Admission == nil {
		return rt.ApplyResult{}, fault.New(fault.FailedPrecondition, "Docker admission required")
	}
	if err := p.Admission.Authorize(ctx, req.Task); err != nil {
		return rt.ApplyResult{}, err
	}
	current, err := p.Client.ContainerInspect(ctx, containerName(req.Task))
	if preparedOnly && errdefs.IsNotFound(err) {
		return rt.ApplyResult{}, fault.New(fault.FailedPrecondition, "prepared container disappeared; replacement is forbidden")
	}
	if err != nil && !errdefs.IsNotFound(err) {
		return rt.ApplyResult{}, err
	}
	if err == nil {
		if !owned(current, req.Task) {
			return rt.ApplyResult{}, fault.New(fault.Conflict, "Docker name is owned by another execution")
		}
		if record == nil {
			return rt.ApplyResult{}, fault.New(fault.Conflict, "Docker execution has no local creation record")
		}
		if record.ContainerID != "" && record.ContainerID != current.ID {
			return rt.ApplyResult{}, fault.New(fault.Conflict, "Docker container ID changed")
		}
		record.ContainerID = current.ID
	} else if record != nil && record.Creating {
		return rt.ApplyResult{}, fault.New(fault.FailedPrecondition, "Docker creation outcome is uncertain; retain allocation for operator recovery")
	}
	if record == nil {
		record = &executionRecord{Task: req.Task, Digest: digest}
	}
	// Persist identity first, but mark uncertainty only at the create boundary.
	if err := p.Journal.write(*record); err != nil {
		return rt.ApplyResult{}, err
	}
	local := *p.Provider
	local.beforeCreate = func(ctx context.Context) error {
		if preparedOnly {
			return fault.New(fault.FailedPrecondition, "prepared instance disappeared; replacement is forbidden")
		}
		if err := p.Admission.Authorize(ctx, req.Task); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		record.Creating = true
		return p.Journal.write(*record)
	}
	local.afterCreate = func(_ context.Context, id string) error {
		if id == "" || record.ContainerID != "" && record.ContainerID != id {
			return errors.New("docker start identity conflict")
		}
		record.ContainerID = id
		return p.Journal.write(*record)
	}
	local.beforeStart = func(ctx context.Context, id string) error {
		if record.ContainerID == "" || record.ContainerID != id {
			return errors.New("docker start identity conflict")
		}
		if p.StartAdmission != nil {
			return p.StartAdmission.Authorize(ctx, req.Task)
		}
		return p.Admission.Authorize(ctx, req.Task)
	}
	var result rt.ApplyResult
	var applyErr error
	if start {
		result, applyErr = local.Apply(ctx, req)
	} else {
		result, applyErr = local.Prepare(ctx, req)
	}
	if applyErr != nil {
		// A successful create/start response can be lost. Re-read using the caller's
		// bounded context; if it too fails, preserve the uncertain intent.
		if found, err := p.Client.ContainerInspect(ctx, containerName(req.Task)); err == nil && owned(found, req.Task) && found.ID != "" {
			if record.ContainerID != "" && record.ContainerID != found.ID {
				return rt.ApplyResult{}, errors.Join(applyErr, errors.New("docker ID changed after create"))
			}
			record.ContainerID = found.ID
			return rt.ApplyResult{}, errors.Join(applyErr, p.Journal.write(*record))
		}
		return rt.ApplyResult{}, applyErr
	}
	if result.Ref.ID == "" || record.ContainerID != "" && result.Ref.ID != record.ContainerID {
		return rt.ApplyResult{}, errors.New("docker create result identity conflict")
	}
	record.ContainerID = result.Ref.ID
	if err := p.Journal.write(*record); err != nil {
		return rt.ApplyResult{}, err
	}
	return result, nil
}

func sameUUIDs(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return len(x) > 0 && slices.Equal(x, y)
}

// Cleanup persists permanent cancellation before stop/remove, then verifies
// exact absence. It also handles stop-before-create. Creating-without-an-ID
// remains uncertain after a timeout and cannot be force-released by the Agent.
func (p *ScopedProvider) Cleanup(ctx context.Context, task rt.TaskIdentity) (string, error) {
	if p == nil || p.Journal == nil {
		return "", errors.New("docker execution journal required")
	}
	if err := validateTask(task); err != nil || task.Generation < 1 {
		return "", errors.Join(errors.New("valid Docker Task generation required"), err)
	}
	unlock, err := p.Journal.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	record, err := p.Journal.read(task)
	if err != nil {
		return "", err
	}
	if record == nil {
		record = &executionRecord{Task: task}
	}
	record.Cancelled = true
	if err := p.Journal.write(*record); err != nil {
		return "", err
	}
	if err := p.checkEngine(ctx); err != nil {
		return "", err
	}
	current, err := p.Client.ContainerInspect(ctx, containerName(task))
	if err != nil && !errdefs.IsNotFound(err) {
		return "", err
	}
	if err == nil {
		if !owned(current, task) || current.ID == "" || record.ContainerID != "" && record.ContainerID != current.ID {
			return "", fault.New(fault.Conflict, "Docker cleanup ownership conflict")
		}
		if !record.Creating {
			return "", fault.New(fault.Conflict, "Docker cleanup cannot adopt an unrecorded container")
		}
		record.ContainerID = current.ID
		if err := p.Journal.write(*record); err != nil {
			return "", err
		}
		if err := p.Provider.Delete(ctx, rt.DeleteRequest{Task: task, Ref: p.ref(task, current.ID, task.Logical.Name)}); err != nil {
			return "", err
		}
	} else if record.Creating && record.ContainerID == "" {
		return "", fault.New(fault.FailedPrecondition, "Docker create may still be committing; cleanup is uncertain")
	}
	if record.ContainerID != "" {
		if _, err := p.Client.ContainerInspect(ctx, record.ContainerID); !errdefs.IsNotFound(err) {
			if err == nil {
				return "", fault.New(fault.Unavailable, "Docker container removal not observed")
			}
			return "", err
		}
	}
	// The stable name must also be absent. Never accept successful remove alone.
	if _, err := p.Client.ContainerInspect(ctx, containerName(task)); !errdefs.IsNotFound(err) {
		if err == nil {
			return "", fault.New(fault.Conflict, "Docker name remains occupied")
		}
		return "", err
	}
	inventory, err := p.Provider.ListManaged(ctx, task)
	if err != nil {
		return "", err
	}
	if len(inventory.Objects) != 0 {
		return "", fault.New(fault.Conflict, "Docker Task inventory remains")
	}
	record.Cleaned = true
	if err := p.Journal.write(*record); err != nil {
		return "", err
	}
	return record.ContainerID, nil
}

// Delete intentionally routes all Coordinator cleanup through the cancellation
// journal rather than using daemon absence as an implicit release proof.
func (p *ScopedProvider) Delete(ctx context.Context, req rt.DeleteRequest) error {
	if req.Ref.RuntimeID != p.runtimeID() || req.Ref.LogicalTask != req.Task.Logical || req.Ref.ID == "" || req.Ref.ID != req.Ref.UID {
		return fault.New(fault.PermissionDenied, "Docker cleanup reference is invalid")
	}
	_, err := p.Cleanup(ctx, req.Task)
	return err
}

// record returns a private, immutable copy for recovery tests and diagnostics.
// It does not expose workload configuration or permit record deletion.
func (p *ScopedProvider) record(ctx context.Context, task rt.TaskIdentity) (*executionRecord, error) {
	unlock, err := p.Journal.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := p.Journal.read(task)
	if r == nil || err != nil {
		return nil, err
	}
	out := *r
	return &out, nil
}
