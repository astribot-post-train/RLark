package enrollment

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/ownership"
	coreprovision "github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/provision"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
	"github.com/google/uuid"
	tenant "github.com/rlinf/rlark/api/metadata"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed notebook_identity.py
var identityScript string

// Probe hashes the current container PID namespace/start/boot identity. A replaced
// Notebook must use explicit replacement; an old record cannot silently adopt it.
var probe = identityScript + `
import json,os,platform,ipaddress
if os.geteuid()!=0: raise RuntimeError('root required')
if platform.machine() not in ('x86_64','amd64'): raise RuntimeError('linux amd64 required')
address=private_address()
ip=ipaddress.ip_address(address)
if ip.version!=4 or not ip.is_private or ip.is_loopback or ip.is_link_local: raise RuntimeError('private IPv4 unavailable')
print(json.dumps({'instanceID':instance_id(),'privateAddress':address}))
`

// Reconciler owns only enrollment status and its create-only node registrations.
// Use an uncached client and one elected controller instance for SSH effects.
type Reconciler struct {
	Client   client.Client
	Runner   Runner
	Packages Bundler
	Profile  Profile
	Now      func() time.Time
}

// Bundler exports only credentials and artifacts for the exact registered node.
type Bundler interface {
	Bundle(context.Context, *ext.ContainerNode, ext.AgentReleaseSpec, string) ([]byte, string, error)
}

func (r *Reconciler) SetupWithManager(m ctrl.Manager) error {
	return builder.ControllerManagedBy(m).For(&ext.NodeEnrollment{}).Complete(r)
}
func (r Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
func (r Reconciler) save(ctx context.Context, before, op *ext.NodeEnrollment) (ctrl.Result, error) {
	if !reflect.DeepEqual(before.Status, op.Status) {
		if err := r.Client.Status().Patch(ctx, op, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}
func (r Reconciler) fail(ctx context.Context, before, op *ext.NodeEnrollment, reason string, retry bool) (ctrl.Result, error) {
	op.Status.ObservedGeneration = op.Generation
	op.Status.Phase = "Failed"
	if retry {
		op.Status.Phase = "Waiting"
	}
	meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{Type: "Completed", Status: metav1.ConditionFalse, ObservedGeneration: op.Generation, Reason: reason, Message: "Inspect prerequisites and retry the same operation; stopped Notebooks must first be started in Kingsoft Cloud", LastTransitionTime: metav1.NewTime(r.now())})
	return r.save(ctx, before, op)
}
func allowed(ip string, ranges []string) bool {
	value := net.ParseIP(ip)
	for _, raw := range ranges {
		_, subnet, err := net.ParseCIDR(raw)
		if err == nil && subnet.Contains(value) {
			return true
		}
	}
	return false
}

// Reconcile reconstructs progress from API objects and current host state.
// It never stops tasks/services, uncordons nodes, or powers on cloud resources.
func (r Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var op ext.NodeEnrollment
	if err := r.Client.Get(ctx, req.NamespacedName, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !op.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	before := op.DeepCopy()
	if op.Spec.CancelRequested {
		if op.Status.Phase == "Succeeded" || op.Status.Phase == "Failed" {
			return ctrl.Result{}, nil
		}
		op.Status.Phase = "Cancelled"
		op.Status.ObservedGeneration = op.Generation
		meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{Type: "Completed", Status: metav1.ConditionFalse, Reason: "Cancelled", Message: "No further bootstrap effects; already joined nodes and services are retained", ObservedGeneration: op.Generation, LastTransitionTime: metav1.NewTime(r.now())})
		_, err := r.save(ctx, before, &op)
		return ctrl.Result{}, err
	}
	completed := op.Status.Phase == "Succeeded"
	parsedID, parseErr := uuid.Parse(op.Spec.OperationID)
	if parseErr != nil || parsedID == uuid.Nil || parsedID.String() != op.Spec.OperationID {
		return r.fail(ctx, before, &op, "InvalidOperationID", false)
	}
	if op.Status.Phase == "Failed" {
		return ctrl.Result{}, nil
	}
	if r.Runner == nil || r.Packages == nil || r.Profile.ClusterUID != op.Spec.ClusterRecord.UID || op.Spec.TenantID == "" || tenant.IDOf(&op) != op.Spec.TenantID || op.Spec.OperationID == "" || coreprovision.ValidateEnrollmentNodes(op.Spec.Nodes) != nil {
		return r.fail(ctx, before, &op, "InvalidEnrollment", false)
	}
	var cc ext.ContainerCluster
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.ClusterRecord.Namespace, Name: op.Spec.ClusterRecord.Name}, &cc); err != nil {
		return ctrl.Result{}, err
	}
	if ownership.MatchesNative(op.Spec.ClusterRecord, &cc) != nil || cc.Namespace != op.Namespace || cc.Spec.Runtime != "Raw" || tenant.IDOf(&cc) != op.Spec.TenantID || !cc.DeletionTimestamp.IsZero() {
		return r.fail(ctx, before, &op, "ClusterChanged", false)
	}
	var release ext.AgentRelease
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: op.Spec.Release.Namespace, Name: op.Spec.Release.Name}, &release); err != nil {
		return ctrl.Result{}, err
	}
	if ownership.MatchesNative(op.Spec.Release, &release) != nil || release.Namespace != op.Namespace || tenant.IDOf(&release) != op.Spec.TenantID || !reflect.DeepEqual(release.Spec, op.Spec.ReleaseSpec) || !release.Status.Available || release.Status.ObservedGeneration != release.Generation || !release.DeletionTimestamp.IsZero() {
		return r.fail(ctx, before, &op, "ReleaseChanged", false)
	}
	// Deterministic queue prevents competing commands for a cluster. The elected
	// controller processes one external effect at a time; current-state retries
	// remain safe when leadership changes after a lost SSH response.
	var operations ext.NodeEnrollmentList
	if err := r.Client.List(ctx, &operations, client.InNamespace(op.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for _, other := range operations.Items {
		terminal := other.Spec.CancelRequested || other.Status.Phase == "Succeeded" || other.Status.Phase == "Failed" || other.Status.Phase == "Cancelled"
		if !completed && other.UID != op.UID && other.Spec.ClusterRecord == op.Spec.ClusterRecord && other.DeletionTimestamp.IsZero() && !terminal && (other.CreationTimestamp.Before(&op.CreationTimestamp) || other.CreationTimestamp.Equal(&op.CreationTimestamp) && other.Name < op.Name) {
			return r.fail(ctx, before, &op, "EarlierOperationPending", true)
		}
	}
	var gateway ext.EnrollmentNode
	for _, n := range op.Spec.Nodes {
		if n.Role == ext.NodeRoleGateway {
			gateway = n
		}
	}
	if !allowed(gateway.Address, r.Profile.AllowedGatewayCIDRs) {
		return r.fail(ctx, before, &op, "SSHAddressDenied", false)
	}
	// Discover nodes in gateway-first order, independent of JSON input ordering.
	targets := slices.Clone(op.Spec.Nodes)
	slices.SortStableFunc(targets, func(a, b ext.EnrollmentNode) int {
		if a.Role == b.Role {
			return 0
		}
		if a.Role == ext.NodeRoleGateway {
			return -1
		}
		return 1
	})
	allDone := true
	for _, target := range targets {
		if target.Role == ext.NodeRoleWorker && !allowed(target.Address, r.Profile.AllowedWorkerCIDRs) {
			return r.fail(ctx, before, &op, "SSHAddressDenied", false)
		}
		index := slices.IndexFunc(op.Status.Nodes, func(n ext.EnrollmentNodeStatus) bool { return n.Name == target.Name })
		if index < 0 {
			op.Status.Nodes = append(op.Status.Nodes, ext.EnrollmentNodeStatus{Name: target.Name})
			index = len(op.Status.Nodes) - 1
		}
		status := &op.Status.Nodes[index]
		// Periodic probe is separate from Agent liveness and never extends its lease.
		if status.ProbedAt == nil || r.now().Sub(status.ProbedAt.Time) >= 30*time.Second || status.ProbedAt.After(r.now()) {
			effectCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			output, err := r.Runner.Run(effectCtx, Target{gateway, target}, "python3 -c "+quote(probe), nil)
			cancel()
			at := metav1.NewTime(r.now())
			status.ProbedAt = &at
			if err != nil {
				status.Reachability = "Unreachable"
				status.Reason = "SSHUnavailable"
				if completed {
					continue
				}
				return r.fail(ctx, before, &op, "SSHUnavailable", true)
			}
			var observed struct {
				InstanceID     string `json:"instanceID"`
				PrivateAddress string `json:"privateAddress"`
			}
			if json.Unmarshal(output, &observed) != nil || len(observed.InstanceID) != 64 || net.ParseIP(observed.PrivateAddress) == nil || !net.ParseIP(observed.PrivateAddress).IsPrivate() {
				status.Reachability = "Unknown"
				return r.fail(ctx, before, &op, "InvalidProbe", true)
			}
			status.Reachability = "Reachable"
			status.Reason = "SSHObserved"
			if status.InstanceID != "" && status.InstanceID != observed.InstanceID {
				return r.fail(ctx, before, &op, "NotebookReplaced", false)
			}
			firstProbe := status.InstanceID == ""
			status.InstanceID = observed.InstanceID
			// A platform SSH address may DNAT into a Notebook network namespace.
			// Pin the authenticated probe's private address independently from the
			// immutable SSH endpoint; neither may drift on a retained operation.
			if target.PrivateAddress != "" && target.PrivateAddress != observed.PrivateAddress {
				return r.fail(ctx, before, &op, "PrivateAddressChanged", false)
			}
			if status.PrivateAddress != "" && status.PrivateAddress != observed.PrivateAddress {
				return r.fail(ctx, before, &op, "PrivateAddressChanged", false)
			}
			status.PrivateAddress = observed.PrivateAddress
			// Persist identity before issuing credentials; retries can fence replacement.
			if firstProbe {
				return r.save(ctx, before, &op)
			}
		}
		if status.Reachability != "Reachable" {
			if completed {
				continue
			}
			return r.fail(ctx, before, &op, "SSHUnavailable", true)
		}
		// Completed operations only refresh network reachability; they cannot
		// reinstall, restart services, or compete with a later operation.
		if completed {
			continue
		}
		var node ext.ContainerNode
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: target.Name}, &node)
		if apierrors.IsNotFound(err) && target.NodeUID == "" && status.NodeUID == "" {
			operationID := uuid.NewSHA1(uuid.MustParse(op.Spec.OperationID), []byte(target.Name)).String()
			created, e := (provision.Service{Client: r.Client}).RegisterNode(ctx, provision.NodeRequest{OperationID: operationID, Cluster: cc.Spec.Cluster.Name, ClusterUID: cc.Spec.Cluster.UID, ContainerClusterUID: string(cc.UID), TenantID: op.Spec.TenantID, Name: target.Name, InstanceID: status.InstanceID, PrivateAddress: status.PrivateAddress, Environment: r.Profile.Environment, NetworkScope: r.Profile.NetworkScope, Role: target.Role})
			if e != nil {
				return r.fail(ctx, before, &op, "RegistrationConflict", true)
			}
			status.NodeUID = string(created.UID)
			status.Phase = "Registered"
			return r.save(ctx, before, &op)
		} else if err != nil {
			return r.fail(ctx, before, &op, "NodeUnavailable", true)
		}
		expectedUID := target.NodeUID
		if expectedUID == "" {
			expectedUID = status.NodeUID
		}
		operationID := uuid.NewSHA1(uuid.MustParse(op.Spec.OperationID), []byte(target.Name)).String()
		if expectedUID == "" && node.Annotations[ext.AgentCredentialAnnotation] == operationID {
			expectedUID = string(node.UID)
		}
		role := node.Spec.Role
		if role == "" {
			role = ext.NodeRoleWorker
		}
		if expectedUID == "" || string(node.UID) != expectedUID || node.Spec.Cluster != cc.Spec.Cluster || node.Spec.InstanceID != status.InstanceID || node.Spec.PrivateAddress != status.PrivateAddress || role != target.Role || tenant.IDOf(&node) != op.Spec.TenantID || !node.DeletionTimestamp.IsZero() {
			return r.fail(ctx, before, &op, "NodeIdentityConflict", false)
		}
		status.NodeUID = string(node.UID)
		if status.Phase != "Started" && status.Phase != "Succeeded" {
			// No process is killed/reconfigured while work may own this node. New nodes
			// cannot dispatch before their first authenticated observation.
			if err := r.idle(ctx, &node); err != nil {
				return r.fail(ctx, before, &op, "NodeBusy", true)
			}
			if node.Status.AgentVersion != "" && node.Status.AgentVersion != release.Spec.Version {
				return r.fail(ctx, before, &op, "UseIdleAgentUpgrade", false)
			}
			gatewayIndex := slices.IndexFunc(op.Status.Nodes, func(n ext.EnrollmentNodeStatus) bool { return n.Name == gateway.Name })
			if gatewayIndex < 0 || op.Status.Nodes[gatewayIndex].PrivateAddress == "" {
				return r.fail(ctx, before, &op, "GatewayAddressUnknown", true)
			}
			data, path, e := r.Packages.Bundle(ctx, &node, release.Spec, op.Status.Nodes[gatewayIndex].PrivateAddress)
			if e != nil {
				return r.fail(ctx, before, &op, "PackageUnavailable", true)
			}
			// A cancellation accepted while a package was being built fences the
			// next external effect. An already issued SSH command may still finish.
			var current ext.NodeEnrollment
			if e := r.Client.Get(ctx, req.NamespacedName, &current); e != nil {
				return ctrl.Result{}, client.IgnoreNotFound(e)
			}
			if current.UID != op.UID || current.Spec.CancelRequested || !current.DeletionTimestamp.IsZero() {
				return ctrl.Result{Requeue: true}, nil
			}
			effectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			identity := fmt.Sprintf("%s/%s/%s", node.UID, node.Spec.InstanceID, release.Spec.SHA256)
			output, e := r.Runner.Run(effectCtx, Target{gateway, target}, installCommand(path, identity, r.Profile.Python), packageReader(data))
			cancel()
			if e != nil {
				return r.fail(ctx, before, &op, "ServiceStartFailed", true)
			}
			var observed struct {
				Identity string   `json:"identity"`
				Services []string `json:"services"`
			}
			if json.Unmarshal(output, &observed) != nil || observed.Identity != identity || !slices.Contains(observed.Services, "agent") || !slices.Contains(observed.Services, "task-manager") || target.Role == ext.NodeRoleGateway && !slices.Contains(observed.Services, "gateway") {
				return r.fail(ctx, before, &op, "ServiceReceiptInvalid", true)
			}
			status.Services = observed.Services
			status.Phase = "Started"
			status.Reason = "ProcessesObserved"
			return r.save(ctx, before, &op)
		}
		at := node.Status.LastHeartbeatTime
		condition := meta.FindStatusCondition(node.Status.Conditions, "Ready")
		ready := at != nil && !at.After(r.now()) && r.now().Sub(at.Time) <= 45*time.Second && node.Status.ObservedGeneration == node.Generation && node.Status.AgentCredentialID == node.Annotations[ext.AgentCredentialAnnotation] && node.Status.AgentVersion == release.Spec.Version && condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == node.Generation
		if ready {
			status.Phase = "Succeeded"
			status.Reason = "AgentObserved"
		} else {
			allDone = false
		}
	}
	if completed {
		return r.save(ctx, before, &op)
	}
	op.Status.ObservedGeneration = op.Generation
	op.Status.Phase = "Waiting"
	reason := "AwaitingAgentHeartbeat"
	truth := metav1.ConditionFalse
	if allDone {
		op.Status.Phase = "Succeeded"
		reason = "ServicesAndAgentsObserved"
		truth = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{Type: "Completed", Status: truth, Reason: reason, Message: "Process startup and authenticated Agent heartbeat are checked independently", ObservedGeneration: op.Generation, LastTransitionTime: metav1.NewTime(r.now())})
	return r.save(ctx, before, &op)
}
func (r Reconciler) idle(ctx context.Context, node *ext.ContainerNode) error {
	var allocations ext.ContainerAllocationList
	if err := r.Client.List(ctx, &allocations, client.InNamespace(node.Namespace)); err != nil {
		return err
	}
	for _, a := range allocations.Items {
		if a.Spec.NodeRecordUID == string(node.UID) || a.Spec.Node == node.Spec.Node {
			return errors.New("node allocation held")
		}
	}
	var nn native.Node
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: node.Spec.Node.Namespace, Name: node.Spec.Node.Name}, &nn); err != nil {
		return err
	}
	if ownership.MatchesNative(node.Spec.Node, &nn) != nil || nn.Status.Reservation != nil || len(nn.Status.DockerReservations) != 0 || node.Status.Busy {
		return errors.New("node busy or replaced")
	}
	return nil
}
