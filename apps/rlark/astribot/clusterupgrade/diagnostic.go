package clusterupgrade

import (
	"encoding/json"
	"regexp"
)

// Diagnostic is the subset of an executor failure that is safe to show in the
// management API. Executors log detailed errors locally and only publish this
// structured message. An arbitrary container termination message is not public.
type Diagnostic struct {
	Kind   string `json:"kind"`
	Field  string `json:"field,omitempty"`
	Holder string `json:"holder,omitempty"`
}

var fieldName = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,79}$`)
var holderName = regexp.MustCompile(`^(upgrade-job|rlarkctl-release)/[a-f0-9-]{36}$`)

func PublicMessage(raw string) string {
	if len(raw) > 512 {
		return ""
	}
	var diagnostic Diagnostic
	if err := json.Unmarshal([]byte(raw), &diagnostic); err != nil {
		return ""
	}
	switch diagnostic.Kind {
	case "unsupported-field":
		if fieldName.MatchString(diagnostic.Field) {
			return "unsupported DeployConfig field " + diagnostic.Field
		}
	case "invalid-config":
		return "invalid upgrade DeployConfig"
	case "lock-conflict":
		if holderName.MatchString(diagnostic.Holder) {
			return "the target cluster is held by " + diagnostic.Holder
		}
		return "the target cluster is held by another upgrade or release"
	case "rollout-failed":
		return "the updated Agent failed to become ready"
	case "rollback-conflict":
		return "rollback stopped because the target changed during the upgrade"
	}
	return ""
}
