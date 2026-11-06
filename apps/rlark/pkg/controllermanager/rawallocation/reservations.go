// Package rawallocation implements durable exclusive reservations for preview Raw nodes.
// It deliberately does not launch processes. A caller may dispatch only after
// Reserve returns the complete committed set and must revalidate before execution.
package rawallocation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"sort"
	"time"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrUnavailable means the complete requested group cannot currently be reserved.
var ErrUnavailable = errors.New("raw node group unavailable")

// Request supplies immutable execution identity and explicit node candidates.
// Candidates must be resolved by trusted placement policy, not a workload process.
type Request struct {
	Cluster     client.ObjectKey
	JobUID      string
	TenantID    string
	ExecutionID string
	Environment string
	Nodes       []client.ObjectKey
}

// Manager uses the API as its lock and recovery record; no in-memory lock is authoritative.
// Client must provide uncached reads and status patches with resourceVersion checks.
type Manager struct {
	Client client.Client
	// Now is injected for deterministic liveness checks; nil uses time.Now.
	Now func() time.Time
	// HeartbeatTimeout defaults to 45 seconds. Negative values are invalid.
	HeartbeatTimeout time.Duration
}

func (m Manager) checkLiveness(node *api.Node) error {
	timeout := m.HeartbeatTimeout
	if timeout < 0 {
		return errors.New("raw heartbeat timeout must not be negative")
	}
	if timeout == 0 {
		timeout = 45 * time.Second
	}
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	if node.Status.LastHeartbeatTime == nil || node.Status.LastHeartbeatTime.After(now) || now.Sub(node.Status.LastHeartbeatTime.Time) > timeout {
		return fmt.Errorf("%w: node %s has no current trusted heartbeat", ErrUnavailable, node.Name)
	}
	return nil
}

// Reserve obtains every requested node and commits all before returning bindings.
// Partial acquisition is persisted for reconstruction; competing requests cannot
// steal it. A failed commit remains reserved for the same execution to retry.
func (m Manager) Reserve(ctx context.Context, req Request) ([]api.RawTaskBinding, error) {
	if m.Client == nil {
		return nil, errors.New("management client is required")
	}
	if req.JobUID == "" || req.TenantID == "" || req.ExecutionID == "" || req.Environment == "" || len(req.Nodes) == 0 {
		return nil, errors.New("complete Raw allocation identity, environment and nodes are required")
	}
	var cluster api.Cluster
	if err := m.Client.Get(ctx, req.Cluster, &cluster); err != nil {
		return nil, fmt.Errorf("read Raw Cluster: %w", err)
	}
	if cluster.UID == "" || cluster.Spec.AgentNamespace == "" || cluster.Spec.Cordoned || cluster.Spec.Draining || !cluster.DeletionTimestamp.IsZero() || tenant.IDOf(&cluster) != req.TenantID {
		return nil, fmt.Errorf("%w: Cluster is not eligible", ErrUnavailable)
	}
	// An overlapping authoritative namespace is ambiguous even if labels agree.
	var clusters api.ClusterList
	if err := m.Client.List(ctx, &clusters); err != nil {
		return nil, fmt.Errorf("check Cluster membership: %w", err)
	}
	for _, other := range clusters.Items {
		if other.UID != cluster.UID && other.Spec.AgentNamespace == cluster.Spec.AgentNamespace {
			return nil, errors.New("ambiguous Cluster agent namespace")
		}
	}
	keys := append([]client.ObjectKey(nil), req.Nodes...)
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	nodes := make([]api.Node, 0, len(keys))
	for i, key := range keys {
		if key.Namespace != cluster.Spec.AgentNamespace || (i > 0 && key == keys[i-1]) {
			return nil, errors.New("node candidates must be unique members of the authoritative Cluster namespace")
		}
		var node api.Node
		if err := m.Client.Get(ctx, key, &node); err != nil {
			return nil, fmt.Errorf("read Raw Node: %w", err)
		}
		if err := m.checkLiveness(&node); err != nil {
			return nil, err
		}
		if err := eligible(&node, req); err != nil {
			return nil, err
		}
		for _, previous := range nodes {
			if previous.Status.Raw.NetworkScope != node.Status.Raw.NetworkScope {
				return nil, errors.New("raw group must share one authorized private network scope")
			}
			for _, gpu := range node.Status.Raw.AuthorizedGPUUUIDs {
				if slices.Contains(previous.Status.Raw.AuthorizedGPUUUIDs, gpu) {
					return nil, errors.New("raw nodes advertise overlapping exclusive GPU UUIDs")
				}
			}
			if previous.Status.Raw.NetworkScope == node.Status.Raw.NetworkScope && previous.Status.Raw.TrainingIP == node.Status.Raw.TrainingIP {
				return nil, errors.New("raw nodes share a training address; exclusive port isolation cannot be proven")
			}
		}
		nodes = append(nodes, node)
	}
	// Reject duplicate authorized scopes across the inventory, including idle
	// nodes. Otherwise two single-node Jobs could each reserve overlapping GPUs
	// or a shared address and defeat exclusive ownership/port isolation.
	var inventory api.NodeList
	if err := m.Client.List(ctx, &inventory); err != nil {
		return nil, fmt.Errorf("check exclusive Raw inventory: %w", err)
	}
	for _, selected := range nodes {
		for _, other := range inventory.Items {
			if other.UID == selected.UID || other.Spec.AgentType != api.AgentTypeRaw || other.Status.Raw == nil {
				continue
			}
			for _, id := range selected.Status.Raw.AuthorizedGPUUUIDs {
				if slices.Contains(other.Status.Raw.AuthorizedGPUUUIDs, id) {
					return nil, fmt.Errorf("%w: duplicate authorized GPU scope in Raw inventory", ErrUnavailable)
				}
			}
			if selected.Status.Raw.NetworkScope == other.Status.Raw.NetworkScope && selected.Status.Raw.TrainingIP == other.Status.Raw.TrainingIP {
				return nil, fmt.Errorf("%w: duplicate training address in Raw inventory", ErrUnavailable)
			}
		}
	}
	members := make([]string, 0, len(nodes))
	for _, node := range nodes {
		members = append(members, string(node.UID))
	}
	digest := groupDigest(members)
	acquired := make([]api.Node, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		wanted := reservation(node, req)
		wanted.GroupDigest = digest
		wanted.ClusterUID = string(cluster.UID)
		if current := node.Status.Reservation; current != nil {
			compare := *current
			compare.Committed = false
			compare.TaskUID = "" // A later Task binding does not change reservation identity.
			if !reflect.DeepEqual(compare, *wanted) {
				return nil, errors.Join(fmt.Errorf("%w: node %s reserved by another execution", ErrUnavailable, node.Name), m.rollback(ctx, acquired, req))
			}
		} else {
			before := node.DeepCopy()
			node.Status.Reservation = wanted
			if err := m.Client.Status().Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, errors.Join(fmt.Errorf("reserve Raw Node %s: %w", node.Name, err), m.rollback(ctx, acquired, req))
			}
		}
		acquired = append(acquired, *node)
	}
	bindings := make([]api.RawTaskBinding, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		if !node.Status.Reservation.Committed {
			before := node.DeepCopy()
			node.Status.Reservation.Committed = true
			if err := m.Client.Status().Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, fmt.Errorf("commit Raw group (retry same execution): %w", err)
			}
		}
		r := node.Status.Reservation
		bindings = append(bindings, api.RawTaskBinding{TaskUID: r.TaskUID, NodeName: node.Name, NodeUID: r.NodeUID, Incarnation: r.Incarnation, ExecutionID: r.ExecutionID, AllocationID: r.AllocationID, JobUID: r.JobUID, GroupDigest: r.GroupDigest, ClusterUID: r.ClusterUID})
	}
	return bindings, nil
}

func eligible(node *api.Node, req Request) error {
	raw := node.Status.Raw
	if node.UID == "" || node.Spec.AgentType != api.AgentTypeRaw || node.Spec.Unschedulable || node.Status.Phase != api.NodeOnline || !node.DeletionTimestamp.IsZero() || tenant.IDOf(node) != req.TenantID || raw == nil || raw.Incarnation == "" || raw.Environment != req.Environment || raw.NetworkScope == "" || net.ParseIP(raw.TrainingIP) == nil || len(raw.AuthorizedGPUUUIDs) == 0 {
		return fmt.Errorf("%w: node %s lacks validated Raw capability or authorized GPUs", ErrUnavailable, node.Name)
	}
	seen := map[string]bool{}
	for _, id := range raw.AuthorizedGPUUUIDs {
		if id == "" || seen[id] {
			return fmt.Errorf("%w: invalid authorized GPU scope", ErrUnavailable)
		}
		seen[id] = true
	}
	return nil
}

func reservation(node *api.Node, req Request) *api.RawNodeReservation {
	return &api.RawNodeReservation{Environment: req.Environment, JobUID: req.JobUID, TenantID: req.TenantID, ExecutionID: req.ExecutionID, AllocationID: req.ExecutionID + ":" + string(node.UID), NodeUID: string(node.UID), Incarnation: node.Status.Raw.Incarnation}
}

// rollback releases only uncommitted records owned by this exact execution.
// It must never release a committed node: an Agent may already have observed it.
func (m Manager) rollback(ctx context.Context, nodes []api.Node, req Request) error {
	var errs []error
	for _, previous := range nodes {
		var node api.Node
		if err := m.Client.Get(ctx, client.ObjectKeyFromObject(&previous), &node); err != nil {
			errs = append(errs, err)
			continue
		}
		r := node.Status.Reservation
		if r == nil || r.Committed || node.UID != previous.UID || r.JobUID != req.JobUID || r.ExecutionID != req.ExecutionID {
			continue
		}
		before := node.DeepCopy()
		node.Status.Reservation = nil
		if err := m.Client.Status().Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ValidateBinding is the execution admission fence, also required for direct Tasks.
// The caller must separately validate credentials, tenant and complete-group readiness.
func ValidateBinding(node *api.Node, binding *api.RawTaskBinding, tenantID string) error {
	if node == nil || binding == nil || tenantID == "" || node.Status.Raw == nil || node.Status.Reservation == nil {
		return errors.New("raw binding and reservation are required")
	}
	r := node.Status.Reservation
	if r.Environment == "" || r.Environment != node.Status.Raw.Environment {
		return errors.New("raw environment changed after reservation")
	}
	if !r.Committed || binding.TaskUID != r.TaskUID || binding.NodeName != node.Name || binding.NodeUID != string(node.UID) || binding.NodeUID != r.NodeUID || binding.Incarnation != node.Status.Raw.Incarnation || binding.Incarnation != r.Incarnation || binding.AllocationID != r.AllocationID || binding.ExecutionID != r.ExecutionID || binding.JobUID != r.JobUID || binding.ClusterUID == "" || binding.ClusterUID != r.ClusterUID || binding.GroupDigest == "" || binding.GroupDigest != r.GroupDigest || tenantID != r.TenantID || tenantID != tenant.IDOf(node) {
		return errors.New("raw execution binding conflicts with the current reservation")
	}
	return nil
}

// AssignTask fences a committed reservation to exactly one management Task UID.
// It is a controller-only boundary. Task creation must precede this write;
// execution must wait until the returned binding is persisted on that Task.
func (m Manager) AssignTask(ctx context.Context, key client.ObjectKey, binding api.RawTaskBinding, tenantID, taskUID string) (api.RawTaskBinding, error) {
	if m.Client == nil || taskUID == "" {
		return api.RawTaskBinding{}, errors.New("management client and assigned Task UID required")
	}
	if binding.TaskUID != "" && binding.TaskUID != taskUID {
		return api.RawTaskBinding{}, errors.New("binding belongs to another Task")
	}
	var node api.Node
	if err := m.Client.Get(ctx, key, &node); err != nil {
		return api.RawTaskBinding{}, err
	}
	if node.Status.Reservation == nil {
		return api.RawTaskBinding{}, errors.New("raw reservation missing")
	}
	r := node.Status.Reservation
	if r.TaskUID != "" && r.TaskUID != taskUID {
		return api.RawTaskBinding{}, errors.New("raw reservation already assigned to another Task")
	}
	check := binding
	check.TaskUID = r.TaskUID
	if err := ValidateBinding(&node, &check, tenantID); err != nil {
		return api.RawTaskBinding{}, err
	}
	if r.TaskUID == "" {
		before := node.DeepCopy()
		r.TaskUID = taskUID
		if err := m.Client.Status().Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return api.RawTaskBinding{}, err
		}
	}
	binding.TaskUID = taskUID
	return binding, nil
}

// Release removes a reservation only after trusted supervision has confirmed
// cleanup for this exact binding. Absence of a heartbeat is never such evidence.
// This is an internal controller boundary, not an operator force-release API.
func (m Manager) Release(ctx context.Context, key client.ObjectKey, binding api.RawTaskBinding, tenantID string, cleaned bool) error {
	if m.Client == nil {
		return errors.New("management client is required")
	}
	if !cleaned {
		return errors.New("confirmed Raw cleanup is required")
	}
	var node api.Node
	if err := m.Client.Get(ctx, key, &node); err != nil {
		return err
	}
	if node.Status.Reservation == nil {
		if string(node.UID) != binding.NodeUID || tenant.IDOf(&node) != tenantID {
			return errors.New("node incarnation changed")
		}
		return nil
	}
	if err := ValidateBinding(&node, &binding, tenantID); err != nil {
		return err
	}
	before := node.DeepCopy()
	node.Status.Reservation = nil
	return m.Client.Status().Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// ValidateGroup re-reads every assigned node before dispatch. A committed flag on
// one node is insufficient: commit writes are not a multi-object transaction.
// The binding set must come from the controller's durable execution plan.
func (m Manager) ValidateGroup(ctx context.Context, namespace, tenantID string, bindings []api.RawTaskBinding) error {
	if m.Client == nil || namespace == "" || tenantID == "" || len(bindings) == 0 {
		return errors.New("complete Raw group is required")
	}
	members := make([]string, 0, len(bindings))
	for _, b := range bindings {
		members = append(members, b.NodeUID)
	}
	digest := groupDigest(members)
	var clusters api.ClusterList
	if err := m.Client.List(ctx, &clusters); err != nil {
		return fmt.Errorf("revalidate Raw Cluster: %w", err)
	}
	var cluster *api.Cluster
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if c.Spec.AgentNamespace != namespace {
			continue
		}
		if cluster != nil {
			return errors.New("ambiguous Cluster agent namespace")
		}
		cluster = c
	}
	if cluster == nil || cluster.UID == "" || tenant.IDOf(cluster) != tenantID || cluster.Spec.Cordoned || cluster.Spec.Draining || !cluster.DeletionTimestamp.IsZero() {
		return ErrUnavailable
	}
	first := bindings[0]
	seen := map[string]bool{}
	seenGPU := map[string]bool{}
	seenAddress := map[string]bool{}
	networkScope := ""
	for _, b := range bindings {
		if seen[b.NodeUID] || b.ExecutionID != first.ExecutionID || b.JobUID != first.JobUID || b.GroupDigest != digest || b.ClusterUID != string(cluster.UID) {
			return errors.New("inconsistent Raw group identity")
		}
		seen[b.NodeUID] = true
		var node api.Node
		if err := m.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: b.NodeName}, &node); err != nil {
			return err
		}
		if err := ValidateBinding(&node, &b, tenantID); err != nil {
			return err
		}
		if err := m.checkLiveness(&node); err != nil {
			return err
		}
		if node.Status.Raw == nil {
			return ErrUnavailable
		}
		if err := eligible(&node, Request{TenantID: tenantID, Environment: node.Status.Raw.Environment}); err != nil {
			return err
		}
		if networkScope != "" && networkScope != node.Status.Raw.NetworkScope {
			return errors.New("raw group network scope changed")
		}
		networkScope = node.Status.Raw.NetworkScope
		if seenAddress[node.Status.Raw.TrainingIP] {
			return errors.New("raw group shares a training address")
		}
		seenAddress[node.Status.Raw.TrainingIP] = true
		for _, gpu := range node.Status.Raw.AuthorizedGPUUUIDs {
			if seenGPU[gpu] {
				return errors.New("raw group shares an exclusive GPU")
			}
			seenGPU[gpu] = true
		}
	}
	return nil
}

// groupDigest binds each committed reservation to the complete sorted node set.
// A caller cannot dispatch a singleton subset of a partially committed group.
func groupDigest(members []string) string {
	sorted := append([]string(nil), members...)
	sort.Strings(sorted)
	data, _ := json.Marshal(sorted) // []string always has a JSON representation.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
