package stations

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

// Machine describes a machine RLark knows of as an administrator reads it: what was
// seen of it and how its join goes. Nothing of any account is in it.
type Machine struct {
	Name         string `json:"name"`
	Site         string `json:"site"`
	NodeClass    string `json:"node_class"`
	MachineType  string `json:"machine_type"`
	MachineModel string `json:"machine_model,omitempty"`
	Source       string `json:"source"`
	// Phase is Discovered (seen, not a node), Admitted (its join is under
	// way) or Joined; Problem names what is wrong with it, if anything.
	Phase   string `json:"phase"`
	Problem string `json:"problem,omitempty"`
	Message string `json:"message,omitempty"`
	Serial  string `json:"serial,omitempty"`
	// InspectedAt is when RLark last logged in to the Machine's entry device
	// and reached the computer behind it, whose host key is NodeHostKeyFingerprint.
	InspectedAt            string `json:"inspected_at,omitempty"`
	NodeHostKeyFingerprint string `json:"node_host_key_fingerprint,omitempty"`
	HostKeyFingerprint     string `json:"host_key,omitempty"`
	Port                   int64  `json:"port,omitempty"`
	HostKeyAlgorithm       string `json:"host_key_algorithm,omitempty"`
	Banner                 string `json:"banner,omitempty"`
	Address                string `json:"address,omitempty"`
	MAC                    string `json:"mac,omitempty"`
	// LastSeen is when a hub of its site last saw the Machine.
	LastSeen        string `json:"last_seen,omitempty"`
	DiscoveryScanID string `json:"discovery_scan_id,omitempty"`
	ClusterID       string `json:"cluster_id,omitempty"`
	NodeName        string `json:"node_name,omitempty"`
	// Admissible says whether the Machine can be admitted now: one that is
	// not a node, was read and has no problem, or one whose join stopped.
	Admissible bool `json:"admissible"`
	// Joining says that an administrator's request for the Machine is being
	// acted on: it is not admitted again until its join succeeds or stops.
	Joining     bool   `json:"joining"`
	RequestedBy string `json:"requested_by,omitempty"`
	RequestedAt string `json:"requested_at,omitempty"`
	// AdmitClusterID is the cluster an administrator admitted the Machine to.
	AdmitClusterID string `json:"admit_cluster_id,omitempty"`
	// Join is where the NodeOperation of the administrator's present request
	// stands: why it stopped, when it did, is the Machine's Message; one that
	// succeeded while the Machine is Discovered again made a node removed since,
	// and the request is spent.
	Join             *MachineJoin           `json:"join,omitempty"`
	NodeJoin         *MachineNodeJoin       `json:"node_join,omitempty"`
	ObservedIdentity map[string]interface{} `json:"observed_identity,omitempty"`
	IdentityVerified string                 `json:"identity_verified,omitempty"`
	IdentityReason   string                 `json:"identity_reason,omitempty"`
}

type MachineJoin struct {
	Phase string `json:"phase,omitempty"`
	// Stage is the last stage it completed.
	Stage        string `json:"stage,omitempty"`
	Reason       string `json:"reason,omitempty"`
	MachineType  string `json:"machine_type,omitempty"`
	MachineModel string `json:"machine_model,omitempty"`
}

// MachineJoinFromNodeJoin projects the join details exposed in the station API.
func MachineJoinFromNodeJoin(join *rlark.NodeJoin) *MachineJoin {
	return &MachineJoin{
		Phase: string(join.Status.Phase), Stage: join.Status.Stage,
		Reason:       join.Annotations["station.rlark.io/join-reason"],
		MachineType:  join.Annotations["station.rlark.io/machine-type"],
		MachineModel: join.Annotations["station.rlark.io/machine-model"],
	}
}

type MachineNodeJoin struct {
	Phase  string `json:"phase,omitempty"`
	Stage  string `json:"stage,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// MachineClass is what the list shows of a NodeClass: the labels a Machine of
// it gets, whose values an administrator may replace for one Machine, and
// which of its passwords have been entered.
type MachineClass struct {
	Kind                string                 `json:"kind,omitempty"`
	Model               string                 `json:"model,omitempty"`
	Labels              map[string]string      `json:"labels,omitempty"`
	Taints              []string               `json:"taints,omitempty"`
	Naming              map[string]interface{} `json:"naming,omitempty"`
	HasDiscovery        bool                   `json:"hasDiscovery"`
	HostKeyFingerprints []string               `json:"hostKeyFingerprints,omitempty"`
	Access              *MachineClassAccess    `json:"access,omitempty"`
}

type MachineClassAccess struct {
	Entry MachineCredentialStatus  `json:"entry"`
	Node  MachineCredentialStatus  `json:"node"`
	Sudo  *MachineCredentialStatus `json:"sudo,omitempty"`
}

type MachineCredentialStatus struct {
	Configured bool `json:"configured"`
}

// MachineView is a Machine as the list shows it, with the join of its
// present request among joins (the join records supplied by the caller).
func MachineView(object *unstructured.Unstructured, joins map[string]*MachineJoin, classes map[string]MachineClass) Machine {
	text := func(fields ...string) string {
		value, _, _ := unstructured.NestedString(object.Object, fields...)
		return value
	}
	nodeClass, _, _ := unstructured.NestedString(object.Object, "spec", "nodeClass")
	classKind, classModel := machineClassIdentity(classes, nodeClass)
	view := Machine{
		Name: object.GetName(), Site: text("spec", "site"), NodeClass: nodeClass, Source: text("spec", "source"),
		Phase: text("status", "phase"), Problem: text("status", "problem"), Message: text("status", "message"), Serial: text("status", "serial"),
		InspectedAt: text("status", "inspectedAt"), NodeHostKeyFingerprint: text("status", "nodeHostKeyFingerprint"), HostKeyFingerprint: text("status", "entry", "hostKeyFingerprint"),
		Address: text("status", "entry", "address"), MAC: text("status", "entry", "mac"), LastSeen: text("status", "entry", "lastSeen"),
		DiscoveryScanID: text("status", "entry", "discoveryScanID"),
		Port:            nestedInt64(object.Object, "status", "entry", "port"), HostKeyAlgorithm: text("status", "entry", "hostKeyAlgorithm"), Banner: text("status", "entry", "banner"),
		ClusterID: text("status", "clusterID"), NodeName: text("status", "nodeName"),
		RequestedBy: text("spec", "admit", "requestedBy"), RequestedAt: text("spec", "admit", "requestedAt"), AdmitClusterID: text("spec", "admit", "clusterID"),
		Join: joins[object.GetName()],
	}
	view.MachineModel = text("spec", "admit", "model")
	if view.MachineModel == "" {
		view.MachineModel, _, _ = unstructured.NestedString(object.Object, "status", "observedIdentity", "model")
	}
	view.ObservedIdentity, _, _ = unstructured.NestedMap(object.Object, "status", "observedIdentity")
	view.IdentityVerified = text("status", "identityVerified")
	view.IdentityReason = text("status", "identityReason")
	if nodeJoin, found, _ := unstructured.NestedMap(object.Object, "status", "nodeJoin"); found {
		view.NodeJoin = &MachineNodeJoin{Phase: stringValue(nodeJoin["phase"]), Stage: stringValue(nodeJoin["stage"]), Reason: stringValue(nodeJoin["reason"])}
	}
	if view.NodeJoin == nil {
		if join := joins[object.GetName()]; join != nil {
			view.NodeJoin = &MachineNodeJoin{Phase: join.Phase, Stage: join.Stage, Reason: join.Reason}
		}
	}
	if view.MachineModel == "" {
		view.MachineModel = classModel
	}
	view.MachineType = text("spec", "admit", "type")
	if declaration, found, _ := unstructured.NestedMap(object.Object, "spec", "admit", "machine"); found && view.MachineType == "" {
		view.MachineType = stringValue(declaration["category"])
	}
	if view.MachineType == "" {
		view.MachineType = text("status", "observedIdentity", "category")
	}
	if view.NodeJoin != nil && view.MachineType == "" {
		if join := joins[object.GetName()]; join != nil {
			view.MachineType, view.MachineModel = join.MachineType, join.MachineModel
		}
	}
	if view.MachineType == "" {
		view.MachineType = classKind
	}
	if view.MachineType == "" {
		if view.NodeClass != "" {
			view.MachineType = "Robot"
		} else {
			view.MachineType = "Unknown"
		}
	}
	// A request is acted on from when it is written until the join it starts succeeds or stops; station-controller
	// may first read the Machine, and retries a fault that is not the Machine's.
	view.Joining = view.Phase != "Joined" && view.AdmitClusterID != "" && (view.Join == nil || view.Join.Phase != "Succeeded" && view.Join.Phase != "Blocked" && view.Join.Phase != "Failed")
	// Read means its entry device let RLark in and the computer behind it answered.
	entryCandidate := view.Phase == "Discovered" && view.Problem == "" && view.NodeClass == "" && view.Source == "Discovery" && view.Address != "" && text("status", "entry", "hostKeyFingerprint") != "" && view.HostKeyAlgorithm != ""
	view.Admissible = !view.Joining && (view.Serial != "" && (view.Phase == "Discovered" && view.Problem == "" && view.InspectedAt != "" && view.NodeHostKeyFingerprint != "" ||
		view.Phase == "Admitted" && view.Problem != "") || entryCandidate)
	return view
}

func stringValue(value interface{}) string { text, _ := value.(string); return text }

func nestedInt64(object map[string]interface{}, fields ...string) int64 {
	value, _, _ := unstructured.NestedInt64(object, fields...)
	return value
}

func machineClassIdentity(classes map[string]MachineClass, name string) (string, string) {
	class, ok := classes[name]
	if !ok {
		return "", ""
	}
	kind, model := class.Kind, class.Model
	if model == "" {
		model = class.Labels["rlark.io/robot-model"]
	}
	if kind == "" && (model != "" || class.Labels["rlark.io/node-category"] == "robot") {
		kind = "Robot"
	}
	if kind == "" && name != "" {
		kind = "Robot"
	}
	return kind, model
}
