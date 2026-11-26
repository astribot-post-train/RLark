package stationidentity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	defaultJoinCodeTTL = 2 * time.Hour
	minJoinCodeTTL     = 10 * time.Minute
	maxJoinCodeTTL     = 7 * 24 * time.Hour
	// JoinCodeLength is the number of characters in an issued code.
	JoinCodeLength                    = 12
	AdmissionSSHLoginAnnotation       = "station.rlark.io/ssh-login"
	AdmissionSSHHostKeyAnnotation     = "station.rlark.io/ssh-host-key"
	AdmissionInstallFailureAnnotation = "station.rlark.io/install-failure"
)

// IssueAdmissionRequest holds the desired admission record and lifetime.
type IssueAdmissionRequest struct {
	ClusterID string `json:"cluster_id"`
	NodeName  string `json:"node_name"`
	// Site is the site the machine stands in when it is not its cluster's.
	Site string `json:"site,omitempty"`
	// Address pins the tunnel address; empty lets station-controller allocate one.
	Address string `json:"address,omitempty"`
	// Uplink is the gateway of a machine whose default route does not lead
	// out, where its site declares no leafUplink.
	Uplink string `json:"uplink,omitempty"`
	// NodeClass names the NodeClass of the machine; the node follows it.
	NodeClass  string                 `json:"node_class,omitempty"`
	Taints     []string               `json:"taints,omitempty"`
	Labels     map[string]string      `json:"labels,omitempty"`
	Machine    map[string]interface{} `json:"machine,omitempty"`
	TTLSeconds int64                  `json:"ttl_seconds,omitempty"`
}

// ReissueAdmissionRequest optionally overrides a replacement code’s lifetime.
type ReissueAdmissionRequest struct {
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// Admission is one machine's admission as an operator reads it. The code
// is never in it: the Gateway answers it once, when it gives it out.
type Admission struct {
	ClusterID string `json:"cluster_id"`
	NodeName  string `json:"node_name"`
	// State is Pending (the code is open), Redeemed (a machine took it and is
	// joining), Joined, or Expired (also a code withdrawn before it was used).
	State      string            `json:"state"`
	Reason     string            `json:"reason,omitempty"`
	Site       string            `json:"site,omitempty"`
	Address    string            `json:"address,omitempty"`
	Uplink     string            `json:"uplink,omitempty"`
	NodeClass  string            `json:"node_class,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Taints     []string          `json:"taints,omitempty"`
	ExpiresAt  string            `json:"expires_at"`
	IssuedBy   string            `json:"issued_by,omitempty"`
	IssuedAt   string            `json:"issued_at,omitempty"`
	RedeemedAt string            `json:"redeemed_at,omitempty"`
	// PublicKey is the WireGuard public key of the machine that redeemed the code.
	PublicKey string `json:"public_key,omitempty"`
	// TunnelAddress is the address station-controller gave the machine.
	TunnelAddress string `json:"tunnel_address,omitempty"`
	// SSHLogin and SSHHostKey are there for a machine joined from the console
	// by SSH: user@host:port, and the host key it showed when it was logged in to.
	SSHLogin   string `json:"ssh_login,omitempty"`
	SSHHostKey string `json:"ssh_host_key,omitempty"`
	// SelfHealed is what RLark found of the machine's earlier attempt or
	// earlier node and put right by itself.
	SelfHealed string `json:"self_healed,omitempty"`
	// JoinTokenAt is when Station API gave the machine its token to join its cluster.
	JoinTokenAt string `json:"join_token_at,omitempty"`
	// Stuck says a machine redeemed the code longer ago than a join takes and is no node yet: Reason says where it stopped.
	Stuck bool `json:"stuck,omitempty"`
	// JoinPhase and JoinStage are where the SSH join of the node stands (its NodeJoin), when it was joined so.
	JoinPhase string `json:"join_phase,omitempty"`
	JoinStage string `json:"join_stage,omitempty"`
}

// joinStuckAfter is how long a redeemed code may take to become a node before its join is shown as stuck: an
// installer takes a few minutes, one that pulls its images through a slow uplink about ten.
const joinStuckAfter = 20 * time.Minute

// IssuedAdmission is the one answer that carries the code, and the line
// the operator runs as root on the machine.
type IssuedAdmission struct {
	Admission Admission `json:"admission"`
	Code      string    `json:"code"`
	Command   string    `json:"command"`
}

// AdmissionView projects an admission without disclosing its join code.
func AdmissionView(object *unstructured.Unstructured, now time.Time) Admission {
	text := func(fields ...string) string {
		value, _, _ := unstructured.NestedString(object.Object, fields...)
		return value
	}
	reason := text("status", "reason")
	view := Admission{
		ClusterID: text("spec", "clusterID"), NodeName: object.GetName(), Site: text("spec", "site"),
		Address: text("spec", "address"), Uplink: text("spec", "uplink", "gateway"), NodeClass: text("spec", "nodeClass"), ExpiresAt: text("spec", "expiresAt"),
		IssuedBy: text("spec", "issuedBy"), IssuedAt: text("spec", "issuedAt"), RedeemedAt: text("spec", "redeemedAt"), PublicKey: text("spec", "publicKey"),
		TunnelAddress: text("status", "address"), Reason: reason,
	}
	view.SSHLogin, view.SSHHostKey = object.GetAnnotations()[AdmissionSSHLoginAnnotation], object.GetAnnotations()[AdmissionSSHHostKeyAnnotation]
	view.SelfHealed, view.JoinTokenAt = object.GetAnnotations()[SelfHealedAnnotation], object.GetAnnotations()[JoinTokenIssuedAnnotation]
	view.Labels, _, _ = unstructured.NestedStringMap(object.Object, "spec", "labels")
	view.Taints, _, _ = unstructured.NestedStringSlice(object.Object, "spec", "taints")
	// The state of the code is read from spec, which the Gateway and Station
	// API write; station-controller's answer only says whether a redeemed
	// code has led to a node yet.
	expires, _ := time.Parse(time.RFC3339, view.ExpiresAt)
	observed, _, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	switch {
	case view.PublicKey == "" && !now.Before(expires):
		view.State = "Expired"
	case view.PublicKey == "":
		view.State = "Pending"
	case observed == object.GetGeneration() && text("status", "phase") == "Joined":
		view.State = "Joined"
	default:
		view.State = "Redeemed"
	}
	if redeemed, err := time.Parse(time.RFC3339, view.RedeemedAt); err == nil && view.State == "Redeemed" {
		view.Stuck = now.Sub(redeemed) > joinStuckAfter
	}
	// Where the installer stopped on a machine joined by SSH (the Gateway follows its output).
	if failure := object.GetAnnotations()[AdmissionInstallFailureAnnotation]; failure != "" && view.State != "Joined" {
		view.Reason = failure
	}
	return view
}

// JoinCodeTTL validates a requested lifetime before converting it to a duration.
func JoinCodeTTL(seconds int64) (time.Duration, error) {
	if seconds == 0 {
		return defaultJoinCodeTTL, nil
	}
	minimum, maximum := int64(minJoinCodeTTL/time.Second), int64(maxJoinCodeTTL/time.Second)
	if seconds < minimum || seconds > maximum {
		return 0, fmt.Errorf("ttl_seconds must be between %d and %d", minimum, maximum)
	}
	return time.Duration(seconds) * time.Second, nil
}

// ValidateIssueAdmission validates the fields of a join-code request.
func ValidateIssueAdmission(request *IssueAdmissionRequest) error {
	if request.ClusterID == "" {
		return errors.New("cluster_id is required")
	}
	if len(validation.IsDNS1123Subdomain(request.NodeName)) > 0 {
		return errors.New("node_name must be a DNS subdomain")
	}
	if request.Site != "" && len(validation.IsDNS1123Label(request.Site)) > 0 {
		return errors.New("site must be a DNS label")
	}
	if request.NodeClass != "" && len(validation.IsDNS1123Label(request.NodeClass)) > 0 {
		return errors.New("node_class must be a DNS label")
	}
	for field, value := range map[string]string{"address": request.Address, "uplink": request.Uplink} {
		if ip := net.ParseIP(value); value != "" && (ip == nil || ip.To4() == nil) {
			return fmt.Errorf("%s must be an IPv4 address", field)
		}
	}
	if len(request.Labels) > 32 {
		return errors.New("at most 32 labels")
	}
	for key, value := range request.Labels {
		if len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			return fmt.Errorf("label %q is not a valid Kubernetes label", key)
		}
	}
	if len(request.Taints) > 8 {
		return errors.New("at most 8 taints")
	}
	for _, taint := range request.Taints {
		rest, effect, found := strings.Cut(taint, ":")
		key, value, _ := strings.Cut(rest, "=")
		if !found || (effect != "NoSchedule" && effect != "PreferNoSchedule" && effect != "NoExecute") ||
			len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			return fmt.Errorf("taint %q must be key[=value]:NoSchedule|PreferNoSchedule|NoExecute", taint)
		}
	}
	return nil
}

// NewJoinCode returns a new join code and its SHA-256 digest.
func NewJoinCode() (string, string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	// 252 is the largest multiple of 36 that fits a byte: rejecting above it
	// keeps every character equally likely.
	code := make([]byte, 0, JoinCodeLength)
	buffer := make([]byte, 2*JoinCodeLength)
	for len(code) < JoinCodeLength {
		if _, err := rand.Read(buffer); err != nil {
			return "", "", err
		}
		for _, b := range buffer {
			if b < 252 && len(code) < JoinCodeLength {
				code = append(code, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	digest := sha256.Sum256(code)
	return string(code), hex.EncodeToString(digest[:]), nil
}
