package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Task calls the restricted Task endpoint. Errors contain only bounded status
// codes; upstream response bodies never enter logs. No redirects are followed.
func (c *Client) Task(ctx context.Context, req TaskRequest) (*Assignment, error) {
	if c == nil || c.HTTPClient == nil || c.Endpoint == "" {
		return nil, errors.New("configured Agent client required")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, ErrCapability
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/task", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	res, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send scoped Task request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		if res.StatusCode == http.StatusConflict {
			return nil, apierrors.NewConflict(native.GroupVersion.WithResource("tasks").GroupResource(), "", errors.New("Task changed; reread current state"))
		}
		return nil, fmt.Errorf("scoped Task request returned HTTP %d", res.StatusCode)
	}
	var result *Assignment
	decoder := json.NewDecoder(io.LimitReader(res.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode scoped Task response: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("exactly one scoped Task response required")
	}
	return result, nil
}

// ScopedTasks adapts a single immutable allocation to Host's Get/Patch/status
// contracts. Each call reaches the API; there is no cached authorization.
type ScopedTasks struct {
	Client     *Client
	Allocation ext.NativeObjectReference
	Task       ext.NativeObjectReference
}

var _ host.TaskClient = ScopedTasks{}
var _ host.StatusWriter = ScopedTasks{}

func (s ScopedTasks) request(action string) TaskRequest {
	return TaskRequest{Action: action, Allocation: s.Allocation, TaskUID: s.Task.UID}
}
func (s ScopedTasks) accept(a *Assignment) error {
	if a == nil || objectRef(&a.Allocation) != s.Allocation || objectRef(&a.Task) != s.Task {
		return ErrIdentity
	}
	return nil
}

// Get reads only the exact Task associated with this immutable allocation.
func (s ScopedTasks) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	t, ok := obj.(*native.Task)
	if !ok || key.Namespace != s.Task.Namespace || key.Name != s.Task.Name || len(opts) != 0 {
		return ErrCapability
	}
	a, err := s.Client.Task(ctx, s.request("get"))
	if err != nil {
		return err
	}
	if err := s.accept(a); err != nil {
		return err
	}
	*t = *a.Task.DeepCopy()
	return nil
}

// Patch accepts only Host's JSON claim patch. Server-side field ownership and
// optimistic concurrency remain authoritative even for a modified Agent.
func (s ScopedTasks) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	t, ok := obj.(*native.Task)
	if !ok || objectRef(t) != s.Task || len(opts) != 0 || patch == nil || patch.Type() != types.JSONPatchType {
		return ErrCapability
	}
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	req := s.request("claim")
	req.ResourceVersion = t.ResourceVersion
	req.Patch = data
	a, err := s.Client.Task(ctx, req)
	if err != nil {
		return err
	}
	if err := s.accept(a); err != nil {
		return err
	}
	*t = *a.Task.DeepCopy()
	return nil
}

// WriteStatus sends a fenced typed intent rather than arbitrary Status JSON.
func (s ScopedTasks) WriteStatus(ctx context.Context, t *native.Task, intent host.StatusIntent) error {
	if objectRef(t) != s.Task {
		return ErrIdentity
	}
	req := s.request("status")
	req.ResourceVersion = t.ResourceVersion
	req.Status = &intent
	a, err := s.Client.Task(ctx, req)
	if err != nil {
		return err
	}
	if err := s.accept(a); err != nil {
		return err
	}
	*t = *a.Task.DeepCopy()
	return nil
}
