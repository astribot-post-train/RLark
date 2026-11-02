package allocation

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const Finalizer = "containers.rlark.io/allocation-cleanup"

// Name is stable across Agent/controller restarts and container replacements on
// the same native Node UID. Never use display names as lock identities.
func Name(nodeUID string) string {
	sum := sha256.Sum256([]byte(nodeUID))
	return "node-" + hex.EncodeToString(sum[:20])
}

// UpgradeName is the canonical per-Cluster operation slot.
func UpgradeName(uid string) string { return "cluster-" + strings.TrimPrefix(Name(uid), "node-") }
