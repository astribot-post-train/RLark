package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	"github.com/rlinf/rlark/extensions/station/network"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// NetworkReconciler answers every NetworkEnrollment from the one SiteNetwork:
// it allocates addresses, computes each node's peers and routes, and writes
// the result to the enrollment's status, where station-api serves it.
// The whole network is one reconcile key, so allocations never race.
type NetworkReconciler struct {
	client  client.Client
	reader  client.Reader
	network types.NamespacedName
	now     func() time.Time
}

func NewNetworkReconciler(writer client.Client, reader client.Reader, siteNetwork types.NamespacedName, now func() time.Time) (*NetworkReconciler, error) {
	if writer == nil || reader == nil || siteNetwork.Namespace == "" || siteNetwork.Name == "" {
		return nil, errors.New("client, uncached reader and SiteNetwork name are required")
	}
	if now == nil {
		now = time.Now
	}
	return &NetworkReconciler{client: writer, reader: reader, network: siteNetwork, now: now}, nil
}

func (r *NetworkReconciler) SetupWithManager(mgr manager.Manager) error {
	whole := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: r.network}}
	})
	return ctrl.NewControllerManagedBy(mgr).Named("site-network").
		For(&stationv1alpha1.SiteNetwork{}).
		Watches(&stationv1alpha1.NetworkEnrollment{}, whole).
		Watches(&stationv1alpha1.Station{}, whole, builder.WithPredicates(predicate.Funcs{
			// Heartbeats touch every Station's status; only a binding,
			// retirement or phase change alters the network.
			UpdateFunc: func(e event.UpdateEvent) bool {
				before, okBefore := e.ObjectOld.(*stationv1alpha1.Station)
				after, okAfter := e.ObjectNew.(*stationv1alpha1.Station)
				return !okBefore || !okAfter || before.Generation != after.Generation || before.Status.Phase != after.Status.Phase
			},
		})).
		Complete(r)
}

// Reconcile answers every enrollment. station-api writes enrollments while this
// runs; losing such a race is not an error, the network is computed again.
func (r *NetworkReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.NamespacedName != r.network {
		return reconcile.Result{}, nil
	}
	result, err := r.reconcile(ctx)
	if apierrors.IsConflict(err) {
		return reconcile.Result{Requeue: true}, nil
	}
	return result, err
}

func (r *NetworkReconciler) reconcile(ctx context.Context) (reconcile.Result, error) {
	var siteNetwork stationv1alpha1.SiteNetwork
	if err := r.reader.Get(ctx, r.network, &siteNetwork); apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("read SiteNetwork: %w", err)
	}
	var enrollments stationv1alpha1.NetworkEnrollmentList
	if err := r.reader.List(ctx, &enrollments); err != nil {
		return reconcile.Result{}, fmt.Errorf("list NetworkEnrollments: %w", err)
	}
	var stations stationv1alpha1.StationList
	if err := r.reader.List(ctx, &stations); err != nil {
		return reconcile.Result{}, fmt.Errorf("list Stations: %w", err)
	}
	sort.Slice(enrollments.Items, func(i, j int) bool {
		a, b := &enrollments.Items[i], &enrollments.Items[j]
		return a.Spec.ClusterID+"/"+a.Name < b.Spec.ClusterID+"/"+b.Name
	})

	spec := siteNetwork.Spec
	siteOfCluster := map[string]string{}
	topology := network.Topology{Pool: spec.Pool, MTU: int(spec.MTU), Interface: spec.Interface, ListenPort: int(spec.ListenPort), KeepaliveSeconds: int(spec.KeepaliveSeconds)}
	for _, site := range spec.Sites {
		topology.Sites = append(topology.Sites, network.Site{Name: site.Name, Mesh: site.Mesh, Reaches: site.Reaches})
		for _, cluster := range site.Clusters {
			siteOfCluster[cluster] = site.Name
		}
	}
	declared := map[string]stationv1alpha1.SiteNetworkNode{}
	for _, node := range spec.Nodes {
		declared[node.ClusterID+"/"+node.NodeName] = node
	}
	stationByUID := map[types.UID]*stationv1alpha1.Station{}
	for i := range stations.Items {
		stationByUID[stations.Items[i].UID] = &stations.Items[i]
	}

	// Keep the allocations of identities that still exist; a retired or
	// deleted Station gives its address back.
	allocated := map[string]stationv1alpha1.SiteNetworkAllocation{}
	var taken []string
	for _, allocation := range siteNetwork.Status.Allocations {
		if station := stationByUID[types.UID(allocation.StationUID)]; station != nil && !station.Spec.Retired {
			allocated[allocation.StationUID] = allocation
			taken = append(taken, allocation.Address)
		}
	}
	for _, node := range spec.Nodes {
		if node.Address != "" {
			taken = append(taken, node.Address)
		}
	}

	answers := make(map[string]*stationv1alpha1.NetworkEnrollmentStatus, len(enrollments.Items))
	enrolled, declaredOnly := map[string]bool{}, map[string]bool{}
	var members []network.Member
	for i := range enrollments.Items {
		enrollment := &enrollments.Items[i]
		id := enrollment.Spec.ClusterID + "/" + enrollment.Name
		answer := &stationv1alpha1.NetworkEnrollmentStatus{ObservedGeneration: enrollment.Generation, Phase: stationv1alpha1.NetworkEnrollmentRejected, PublicKey: enrollment.Spec.PublicKey}
		answers[id] = answer
		station := stationByUID[types.UID(enrollment.Spec.StationUID)]
		if station == nil || station.Namespace != enrollment.Namespace || station.Spec.Retired || station.Status.Phase == stationv1alpha1.StationPhaseConflict ||
			station.Spec.Binding.NodeUID != enrollment.Spec.NodeUID || station.Spec.Binding.Version != enrollment.Spec.BindingVersion {
			answer.Reason = "the enrollment does not match a current, unretired Station binding"
			continue
		}
		answer.Site = siteOfCluster[enrollment.Spec.ClusterID]
		if answer.Site == "" {
			answer.Reason = "cluster " + enrollment.Spec.ClusterID + " is not in any site of the SiteNetwork"
			continue
		}
		node := declared[id]
		answer.Role = node.Role
		if answer.Role == "" {
			answer.Role = string(network.RoleLeaf)
		}
		if spec.Mode == stationv1alpha1.SiteNetworkModeRouted {
			answer.Address = node.NodeAddress
			if answer.Address == "" && len(enrollment.Spec.Addresses) > 0 {
				answer.Address = enrollment.Spec.Addresses[0]
			}
			answer.Phase, answer.Reason = stationv1alpha1.NetworkEnrollmentEnrolled, ""
			if answer.Address == "" {
				answer.Phase, answer.Reason = stationv1alpha1.NetworkEnrollmentPending, "the node has not reported an address"
			}
			continue
		}
		if enrollment.Spec.PublicKey == "" {
			answer.Phase, answer.Reason = stationv1alpha1.NetworkEnrollmentPending, "the node has not reported a public key"
			continue
		}
		address := node.Address
		if address == "" {
			allocation, ok := allocated[enrollment.Spec.StationUID]
			if !ok {
				free, err := network.Allocate(spec.Pool, taken)
				if err != nil {
					answer.Phase, answer.Reason = stationv1alpha1.NetworkEnrollmentPending, err.Error()
					continue
				}
				allocation = stationv1alpha1.SiteNetworkAllocation{StationUID: enrollment.Spec.StationUID, Address: free}
				taken = append(taken, free)
			}
			allocation.ClusterID, allocation.NodeName = enrollment.Spec.ClusterID, enrollment.Name
			allocated[enrollment.Spec.StationUID] = allocation
			address = allocation.Address
		}
		member := network.Member{
			ID: id, Site: answer.Site, Role: network.Role(answer.Role), Relay: node.Relay, PublicKey: enrollment.Spec.PublicKey,
			Address: address, NodeAddress: node.NodeAddress, Interface: node.Interface, ListenPort: int(node.ListenPort),
		}
		for _, endpoint := range node.Endpoints {
			if endpoint.Address == "" && len(enrollment.Spec.Addresses) > 0 {
				port := int(node.ListenPort)
				if port == 0 {
					port = int(spec.ListenPort)
				}
				endpoint.Address = net.JoinHostPort(enrollment.Spec.Addresses[0], strconv.Itoa(port))
			}
			member.Endpoints = append(member.Endpoints, network.Endpoint{Address: endpoint.Address, Scope: network.EndpointScope(endpoint.Scope)})
		}
		for _, advertisement := range node.Advertise {
			member.Advertise = append(member.Advertise, network.Advertisement{CIDR: advertisement.CIDR, Sites: advertisement.Sites})
		}
		if node.Uplink != nil {
			member.Uplink = &network.Uplink{Gateway: node.Uplink.Gateway, Source: node.Uplink.Source, Interface: node.Uplink.Interface}
		}
		members = append(members, member)
		enrolled[id] = true
	}

	// A declared node with a key takes part before it can enroll: its peers
	// must know it for its tunnel, and with it its path to Station API, to exist.
	for _, node := range spec.Nodes {
		id := node.ClusterID + "/" + node.NodeName
		if node.PublicKey == "" || node.Address == "" || spec.Mode == stationv1alpha1.SiteNetworkModeRouted || enrolled[id] {
			continue
		}
		declaredOnly[id] = true
		member := network.Member{
			ID: id, Site: siteOfCluster[node.ClusterID], Role: network.Role(node.Role), Relay: node.Relay, PublicKey: node.PublicKey,
			Address: node.Address, NodeAddress: node.NodeAddress, Interface: node.Interface, ListenPort: int(node.ListenPort),
		}
		for _, endpoint := range node.Endpoints {
			member.Endpoints = append(member.Endpoints, network.Endpoint{Address: endpoint.Address, Scope: network.EndpointScope(endpoint.Scope)})
		}
		for _, advertisement := range node.Advertise {
			member.Advertise = append(member.Advertise, network.Advertisement{CIDR: advertisement.CIDR, Sites: advertisement.Sites})
		}
		members = append(members, member)
	}

	// Addresses are durable before any node is told to use one.
	status := *siteNetwork.Status.DeepCopy()
	status.Allocations = status.Allocations[:0]
	for _, allocation := range allocated {
		status.Allocations = append(status.Allocations, allocation)
	}
	sort.Slice(status.Allocations, func(i, j int) bool { return status.Allocations[i].StationUID < status.Allocations[j].StationUID })
	if !reflect.DeepEqual(status.Allocations, siteNetwork.Status.Allocations) {
		siteNetwork.Status.Allocations = status.Allocations
		if err := r.client.Status().Update(ctx, &siteNetwork); err != nil {
			return reconcile.Result{}, fmt.Errorf("record address allocations: %w", err)
		}
	}

	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Computed", Message: "every enrollment is answered from the current topology"}
	connected := metav1.Condition{Type: "FullyConnected", Status: metav1.ConditionTrue, Reason: "AllPairsReachable", Message: "every pair of members has a path"}
	status.Members, status.Unreachable = 0, nil
	if spec.Mode != stationv1alpha1.SiteNetworkModeRouted {
		result, err := network.Compute(topology, members)
		if err != nil {
			ready = metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "InvalidTopology", Message: err.Error()}
			for _, member := range members {
				if !declaredOnly[member.ID] {
					answers[member.ID].Phase, answers[member.ID].Reason = stationv1alpha1.NetworkEnrollmentPending, "the SiteNetwork is invalid: "+err.Error()
				}
			}
		}
		for _, member := range members {
			answer := answers[member.ID]
			config, ok := result.Configs[member.ID]
			if declaredOnly[member.ID] {
				// Declared ahead of its enrollment: it is in its peers'
				// configurations and has no enrollment to answer.
				if ok {
					status.Members++
				}
				continue
			}
			answer.Problems = result.Problems[member.ID]
			if !ok {
				if err == nil {
					answer.Reason = "the node is not configurable: " + fmt.Sprint(answer.Problems)
				}
				continue
			}
			data, marshalErr := json.Marshal(config)
			if marshalErr != nil {
				return reconcile.Result{}, marshalErr
			}
			digest := sha256.Sum256(data)
			answer.Phase, answer.Reason = stationv1alpha1.NetworkEnrollmentEnrolled, ""
			answer.Config, answer.ConfigHash = string(data), hex.EncodeToString(digest[:])
			answer.TunnelAddress, answer.Address = member.Address, member.Address
			if member.NodeAddress != "" {
				answer.Address = member.NodeAddress
			}
			status.Members++
		}
		if len(result.Unreachable) > 0 {
			status.Unreachable = result.Unreachable[:min(32, len(result.Unreachable))]
			connected = metav1.Condition{Type: "FullyConnected", Status: metav1.ConditionFalse, Reason: "UnreachablePairs", Message: strconv.Itoa(len(result.Unreachable)) + " ordered pairs of members have no path"}
		}
	} else {
		for _, answer := range answers {
			if answer.Phase == stationv1alpha1.NetworkEnrollmentEnrolled {
				status.Members++
			}
		}
	}

	for i := range enrollments.Items {
		enrollment := &enrollments.Items[i]
		answer := answers[enrollment.Spec.ClusterID+"/"+enrollment.Name]
		if reflect.DeepEqual(*answer, enrollment.Status) {
			continue
		}
		enrollment.Status = *answer
		if err := r.client.Status().Update(ctx, enrollment); err != nil {
			return reconcile.Result{}, fmt.Errorf("answer NetworkEnrollment %s/%s: %w", enrollment.Namespace, enrollment.Name, err)
		}
	}

	status.ObservedGeneration = siteNetwork.Generation
	for _, condition := range []metav1.Condition{ready, connected} {
		condition.ObservedGeneration = siteNetwork.Generation
		condition.LastTransitionTime = metav1.NewTime(r.now())
		apimeta.SetStatusCondition(&status.Conditions, condition)
	}
	if !reflect.DeepEqual(status, siteNetwork.Status) {
		siteNetwork.Status = status
		if err := r.client.Status().Update(ctx, &siteNetwork); err != nil {
			return reconcile.Result{}, fmt.Errorf("update SiteNetwork status: %w", err)
		}
	}
	return reconcile.Result{}, nil
}
