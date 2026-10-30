package raw

import (
	"context"

	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

// MaxLogBytes bounds each local and remote snapshot independently of retention.
const MaxLogBytes = 64 << 10

// LogSnapshot is the beginning of retained combined stdout/stderr. Bytes remain
// opaque on transport; neither callers nor renderers should execute log content.
type LogSnapshot struct {
	Data                  []byte `json:"data"`
	Truncated             bool   `json:"truncated"`
	RetentionLimitReached bool   `json:"retentionLimitReached"`
}

// LogReader is optional and separate from process control and observation.
type LogReader interface {
	Logs(context.Context, rt.TaskIdentity, int64) (LogSnapshot, error)
}
