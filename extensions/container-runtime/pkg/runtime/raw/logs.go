package raw

import (
	"context"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/diagnostics"

	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

const MaxLogBytes = diagnostics.MaxLogBytes

type LogSnapshot = diagnostics.LogSnapshot

// LogReader is optional and separate from process control and observation.
type LogReader interface {
	Logs(context.Context, rt.TaskIdentity, int64) (LogSnapshot, error)
}
