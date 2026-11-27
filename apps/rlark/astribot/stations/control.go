package stations

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

// ControlRoutes binds the station operations to the Gateway’s node store and authorization.
type ControlRoutes struct {
	GetNode                 func(context.Context, string, string) (*rlarkv1alpha1.Node, error)
	ListNodes               func(context.Context, string, metav1.ListOptions) (*rlarkv1alpha1.NodeList, error)
	PatchNode               func(context.Context, string, string, []byte) (*rlarkv1alpha1.Node, error)
	Admissions              func(string) dynamic.ResourceInterface
	AuthorizeNode           func(*gin.Context, string, string) bool
	Actor                   func(*gin.Context) string
	IsOperator              func(*gin.Context) bool
	RespondError            func(*gin.Context, int, ErrorKind, string)
	RespondErrorWithDetails func(*gin.Context, int, ErrorKind, string, map[string]any)
	RespondKubeError        func(*gin.Context, string, error)
	RespondWriteError       func(*gin.Context, error)
	ForwardPose             func(context.Context, string, string, string, []byte) (*http.Response, error)
}

// Register mounts the station operations with the caller’s shared operator middleware.
func (r ControlRoutes) Register(nodes gin.IRouter, requireOperator gin.HandlerFunc) {
	nodes.PUT("/:name/robot-power", requireOperator, r.SetPower)
	nodes.PUT("/:name/station-mark", requireOperator, r.setMark)
	nodes.POST("/:name/robot-restart", r.restart)
	nodes.PUT("/:name/station", requireOperator, r.setName)
	nodes.POST("/:name/robot-pose", requireOperator, r.Pose)
	nodes.PUT("/:name/robot-claim", r.claim)
	nodes.DELETE("/:name/robot-claim", r.release)
}
