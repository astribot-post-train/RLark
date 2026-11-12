package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SiteNetworkMode says how the nodes of the network reach each other.
// +kubebuilder:validation:Enum=Tunnel;Routed
type SiteNetworkMode string

const (
	// SiteNetworkModeTunnel gives every node a WireGuard address and peers.
	SiteNetworkModeTunnel SiteNetworkMode = "Tunnel"
	// SiteNetworkModeRouted keeps enrollment and the address registry but
	// configures no tunnel: the nodes' own addresses are routable.
	SiteNetworkModeRouted SiteNetworkMode = "Routed"
)

// SiteNetwork declares one network of nodes across sites: the address pool,
// the sites, and the nodes that are more than a leaf. An administrator owns
// spec; station-controller owns status. Nodes enroll themselves with a
// NetworkEnrollment and never appear here unless they need a role.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sitenet
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.pool`
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=`.status.members`
type SiteNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SiteNetworkSpec   `json:"spec"`
	Status            SiteNetworkStatus `json:"status,omitempty"`
}

type SiteNetworkSpec struct {
	// +kubebuilder:default=Tunnel
	Mode SiteNetworkMode `json:"mode,omitempty"`
	// Pool is the IPv4 prefix tunnel addresses are allocated from.
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=18
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pool is immutable"
	Pool string `json:"pool"`
	// +kubebuilder:default=1380
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=9000
	MTU int32 `json:"mtu,omitempty"`
	// +kubebuilder:default=st-wg0
	// +kubebuilder:validation:MaxLength=15
	Interface string `json:"interface,omitempty"`
	// +kubebuilder:default=51820
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	ListenPort int32 `json:"listenPort,omitempty"`
	// +kubebuilder:default=25
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	KeepaliveSeconds int32 `json:"keepaliveSeconds,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Sites []SiteNetworkSite `json:"sites"`
	// Nodes lists only the nodes that need more than the leaf default.
	// +kubebuilder:validation:MaxItems=256
	Nodes []SiteNetworkNode `json:"nodes,omitempty"`
}

type SiteNetworkSite struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Clusters are the registered RLark Cluster IDs whose nodes are in this site.
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	Clusters []string `json:"clusters,omitempty"`
	// Mesh links the hubs of this site to each other over the tunnel.
	Mesh bool `json:"mesh,omitempty"`
	// Reaches lists the sites whose Site-scoped endpoints this site can dial.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Reaches []string `json:"reaches,omitempty"`
}

type SiteNetworkNode struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ClusterID string `json:"clusterID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName"`
	// +kubebuilder:default=Leaf
	// +kubebuilder:validation:Enum=Leaf;Hub
	Role string `json:"role,omitempty"`
	// Relay makes a hub carry traffic between the leaves that dial it.
	Relay bool `json:"relay,omitempty"`
	// Address pins the tunnel address; empty lets the controller allocate one.
	// +kubebuilder:validation:MaxLength=15
	Address string `json:"address,omitempty"`
	// NodeAddress is the address other nodes use for this node when it is not
	// the tunnel address, for example a cloud VPC address.
	// +kubebuilder:validation:MaxLength=15
	NodeAddress string `json:"nodeAddress,omitempty"`
	// +kubebuilder:validation:MaxLength=15
	Interface string `json:"interface,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	ListenPort int32 `json:"listenPort,omitempty"`
	// +kubebuilder:validation:MaxItems=4
	Endpoints []SiteNetworkEndpoint `json:"endpoints,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Advertise []SiteNetworkAdvertisement `json:"advertise,omitempty"`
	Uplink    *SiteNetworkUplink         `json:"uplink,omitempty"`
}

type SiteNetworkEndpoint struct {
	// Address is ip:port. Empty means the address the node reported at
	// enrollment and its listen port, for a hub whose address is assigned by DHCP.
	// +kubebuilder:validation:MaxLength=21
	Address string `json:"address,omitempty"`
	// +kubebuilder:validation:Enum=Public;Site
	Scope string `json:"scope"`
}

type SiteNetworkAdvertisement struct {
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=18
	CIDR string `json:"cidr"`
	// Sites limits the receivers; empty means every other site.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Sites []string `json:"sites,omitempty"`
}

// SiteNetworkUplink makes the node reach the endpoints it dials through a
// given gateway and source address instead of its default route.
type SiteNetworkUplink struct {
	// +kubebuilder:validation:MaxLength=15
	Gateway string `json:"gateway"`
	// +kubebuilder:validation:MaxLength=15
	Source string `json:"source"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	Interface string `json:"interface"`
}

type SiteNetworkStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Allocations are durable: an address stays with its Station identity
	// across rebinds and is released only when the identity is retired.
	// +kubebuilder:validation:MaxItems=4096
	// +listType=map
	// +listMapKey=stationUID
	Allocations []SiteNetworkAllocation `json:"allocations,omitempty"`
	Members     int32                   `json:"members,omitempty"`
	// Unreachable lists the first ordered pairs of members with no path.
	// +kubebuilder:validation:MaxItems=32
	Unreachable []string `json:"unreachable,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type SiteNetworkAllocation struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	StationUID string `json:"stationUID"`
	ClusterID  string `json:"clusterID"`
	NodeName   string `json:"nodeName"`
	// +kubebuilder:validation:MinLength=7
	// +kubebuilder:validation:MaxLength=15
	Address string `json:"address"`
}

// +kubebuilder:object:root=true
type SiteNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SiteNetwork `json:"items"`
}

// NetworkEnrollment is one node's request to join the SiteNetwork and the
// answer to it. station-api writes spec from the node's verified identity and
// its report; station-controller writes status. Its name is the Node name in
// the namespace mapped to the cluster.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=netenroll
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Site",type=string,JSONPath=`.status.site`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.status.role`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
type NetworkEnrollment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NetworkEnrollmentSpec   `json:"spec"`
	Status            NetworkEnrollmentStatus `json:"status,omitempty"`
}

type NetworkEnrollmentSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ClusterID string `json:"clusterID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	NodeUID string `json:"nodeUID"`
	// StationUID and BindingVersion are the identity the request was verified
	// against; the controller rejects the enrollment when they are no longer current.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	StationUID string `json:"stationUID"`
	// +kubebuilder:validation:Minimum=1
	BindingVersion int64 `json:"bindingVersion"`
	// PublicKey is the node's WireGuard public key; the private key never
	// leaves the node. Empty for a node that runs no tunnel.
	// +kubebuilder:validation:MaxLength=44
	PublicKey string `json:"publicKey,omitempty"`
	// Addresses are the node's own addresses as it reported them, the first
	// being the source it uses towards station-api.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=15
	Addresses []string `json:"addresses,omitempty"`
	// ObservedSource is the address station-api saw the request arrive from.
	// It differs from Addresses when the node is behind NAT.
	// +kubebuilder:validation:MaxLength=45
	ObservedSource string `json:"observedSource,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;Enrolled;Rejected
type NetworkEnrollmentPhase string

const (
	NetworkEnrollmentPending  NetworkEnrollmentPhase = "Pending"
	NetworkEnrollmentEnrolled NetworkEnrollmentPhase = "Enrolled"
	NetworkEnrollmentRejected NetworkEnrollmentPhase = "Rejected"
)

type NetworkEnrollmentStatus struct {
	ObservedGeneration int64                  `json:"observedGeneration,omitempty"`
	Phase              NetworkEnrollmentPhase `json:"phase,omitempty"`
	// Reason explains Pending and Rejected in one line.
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason,omitempty"`
	Site   string `json:"site,omitempty"`
	Role   string `json:"role,omitempty"`
	// Address is the one address every other member reaches this node at.
	Address string `json:"address,omitempty"`
	// TunnelAddress is the node's address inside the tunnel, when it has one.
	TunnelAddress string `json:"tunnelAddress,omitempty"`
	// Config is the document station-gateway reconciles, for PublicKey.
	// +kubebuilder:validation:MaxLength=262144
	Config string `json:"config,omitempty"`
	// ConfigHash is the SHA-256 of Config in hex.
	ConfigHash string `json:"configHash,omitempty"`
	// PublicKey is the key Config was computed for.
	PublicKey string `json:"publicKey,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Problems []string `json:"problems,omitempty"`
}

// +kubebuilder:object:root=true
type NetworkEnrollmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetworkEnrollment `json:"items"`
}
