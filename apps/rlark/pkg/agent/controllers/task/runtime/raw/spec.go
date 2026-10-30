package raw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func decodeManifest(data json.RawMessage) (ProcessManifest, error) {
	var spec ProcessManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, fault.Wrap(fault.InvalidArgument, "decode Raw process manifest", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return spec, fault.New(fault.InvalidArgument, "Raw process manifest must contain exactly one object")
	}
	if err := spec.Spec.Validate(); err != nil {
		return spec, fault.Wrap(fault.InvalidArgument, "invalid Raw process manifest", err)
	}
	if spec.APIVersion != "raw.rlark.io/v1alpha1" || spec.Kind != "Process" || spec.Metadata.Name == "" || spec.Metadata.Namespace == "" || spec.Metadata.UID == "" {
		return spec, fault.New(fault.InvalidArgument, "complete Raw process schema and metadata required")
	}
	return spec, nil
}

// Validate rejects ambiguous executables and environment entries before effects.
// Empty Env is an explicit empty environment, never os.Environ(). The Supervisor
// must separately authorize directories and executable paths on its local node.
func (s ProcessSpec) Validate() error {
	if len(s.Argv) == 0 || !filepath.IsAbs(s.Argv[0]) || filepath.Clean(s.Argv[0]) != s.Argv[0] {
		return fmt.Errorf("a canonical absolute executable path is required")
	}
	if !filepath.IsAbs(s.Directory) || filepath.Clean(s.Directory) != s.Directory {
		return fmt.Errorf("a canonical absolute working directory is required")
	}
	for _, arg := range s.Argv {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("NUL in process argument")
		}
	}
	seen := map[string]bool{}
	for _, entry := range s.Env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !environmentName.MatchString(name) || seen[name] || strings.ContainsRune(entry, 0) {
			return fmt.Errorf("invalid or duplicate process environment entry")
		}
		seen[name] = true
	}
	return nil
}

// ProcessManifest carries the identity the Coordinator uses to match desired and
// observed objects. Spec alone is not a valid Runtime object declaration.
type ProcessManifest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   ProcessMetadata `json:"metadata"`
	Spec       ProcessSpec     `json:"spec"`
}

// ProcessMetadata fences the process to one management Task incarnation.
type ProcessMetadata struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

func manifestFor(task rt.TaskIdentity, spec ProcessSpec) ProcessManifest {
	return ProcessManifest{APIVersion: "raw.rlark.io/v1alpha1", Kind: "Process", Metadata: ProcessMetadata{Namespace: task.Logical.Namespace, Name: task.Logical.Name, UID: task.ObjectUID}, Spec: spec}
}

// DesiredProcess constructs the sole immutable process object for an owned Task.
// It performs no admission or external effects.
func DesiredProcess(task rt.TaskIdentity, spec ProcessSpec) (rt.DesiredObject, error) {
	if err := validIdentity(task); err != nil {
		return rt.DesiredObject{}, err
	}
	if err := spec.Validate(); err != nil {
		return rt.DesiredObject{}, err
	}
	data, err := json.Marshal(manifestFor(task, spec))
	if err != nil {
		return rt.DesiredObject{}, err
	}
	return rt.DesiredObject{LogicalName: "process", Kind: ProcessKind, Manifest: rt.TypedManifest{Type: ProcessKind, Object: data}, Ownership: rt.ResourceOwnership{Scope: rt.IdentityTaskIncarnation, OwnerKey: task.ObjectUID}, Lifecycle: rt.LifecycleDeleteOnTaskCleanup, UpdateMode: rt.UpdateUnsupported}, nil
}

// ValidateDesired rejects unsupported object features before Runtime I/O.
func (p *Provider) ValidateDesired(d rt.DesiredObject) error {
	if d.Kind != ProcessKind || d.Manifest.Type != ProcessKind || d.Ownership.Scope != rt.IdentityTaskIncarnation || d.Ownership.IncarnationPolicy != "" || d.Lifecycle != rt.LifecycleDeleteOnTaskCleanup || d.UpdateMode != rt.UpdateUnsupported || len(d.Units) != 0 || len(d.OwnedFields) != 0 || d.ReconcileProfile != (rt.ObjectKind{}) {
		return fault.New(fault.InvalidArgument, "unsupported Raw object contract")
	}
	manifest, err := decodeManifest(d.Manifest.Object)
	if err != nil {
		return err
	}
	if d.Ownership.OwnerKey != manifest.Metadata.UID {
		return fault.New(fault.PermissionDenied, "Raw manifest ownership conflicts with Task UID")
	}
	return nil
}

// Lower rejects Pod execution units; Raw processes are already Runtime-ready.
func (p *Provider) Lower(ctx context.Context, request rt.UnitLoweringRequest) (rt.DesiredObject, error) {
	if err := ctx.Err(); err != nil {
		return rt.DesiredObject{}, err
	}
	if request.Runtime.Kind != "Raw" || request.Runtime.ID != p.ID {
		return rt.DesiredObject{}, fault.New(fault.InvalidArgument, "incorrect Raw Runtime selection")
	}
	if err := p.ValidateDesired(request.Object); err != nil {
		return rt.DesiredObject{}, err
	}
	return request.Object, nil
}
