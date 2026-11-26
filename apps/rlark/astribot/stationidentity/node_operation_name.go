package stationidentity

import (
	"crypto/sha256"
	"encoding/hex"
)

// NodeOperationName identifies a request within its cluster.
func NodeOperationName(clusterID, requestID string) string {
	digest := sha256.Sum256([]byte(clusterID + "\x00" + requestID))
	return "node-op-" + hex.EncodeToString(digest[:16])
}
