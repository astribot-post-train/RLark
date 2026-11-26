// Package workflow owns Astribot's staged node admission and cluster provisioning.
package workflow

import (
	"context"
	"github.com/rlinf/rlark/apps/rlark/astribot/registry"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/api/kubeclients/clientset/versioned"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	"github.com/rlinf/rlark/apps/rlark/pkg/addons"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Bootstrap is the verified Station API used to obtain installer artifacts.
type Bootstrap interface {
	stationidentity.AdmissionBootstrap
	Open(ctx context.Context, path string) (io.ReadCloser, int64, error)
	BundleDigest(ctx context.Context) (string, error)
}

// Hooks are the cross-feature infrastructure needed by the provisioning stages.
type Hooks struct {
	GetKCPAdminCerts          func() (certPEM, keyPEM, caPEM []byte, err error)
	SignCertViaServer         func(clusterID string) (certPEM, keyPEM string, err error)
	StoreOwnedAgentCertSecret func(context.Context, string, []byte, []byte, []byte) error
	ImagePullSecretsOf        func(context.Context, string) ([]registry.ImagePullSecret, error)
	ProxyKubeRequest          func(context.Context, string, string, string, []byte) (*http.Response, error)
	PublishAddonPackage       func(context.Context, string, string, string) (addons.ItemMeta, int, error)
	FetchPodLogs              func(context.Context, string, string, string) (string, error)
	DistributeJobSecret       func(context.Context, string, string) error
}

// HTTP keeps the Gateway's existing API envelopes, actor identity and cluster namespace mapping.
type HTTP struct {
	RespondError              func(*gin.Context, int, string, string)
	RespondKubeError          func(*gin.Context, string, error)
	DecodeOneJSON             func(*gin.Context, int64, any, bool) bool
	Actor                     func(*gin.Context) string
	StationNamespace          func(*gin.Context, string) (string, error)
	CodeInvalidRequestBody    string
	CodeInvalidRequest        string
	CodeResourceConflict      string
	CodeResourceAlreadyExists string
	CodeNotConfigured         string
}

type Config struct {
	ManagementNamespace string
	ServerAddress       string
	StationCAFile       string
}

// Service owns the in-process run registries and carries the clients for both workflows.
type Service struct {
	kubeClient       versioned.Interface
	rawClient        kubernetes.Interface
	stationClient    dynamic.Interface
	stationBootstrap Bootstrap
	namespace        string
	serverAddress    string
	stationCAFile    string
	provisionDial    DialFunc
	hooks            Hooks
	http             HTTP
	provisions       stagedRuns
	nodeJoins        stagedRuns
}

func New(config Config, client versioned.Interface, raw kubernetes.Interface, station dynamic.Interface, bootstrap Bootstrap, hooks Hooks, httpTools HTTP) *Service {
	return &Service{
		kubeClient: client, rawClient: raw, stationClient: station, stationBootstrap: bootstrap,
		namespace: config.ManagementNamespace, serverAddress: config.ServerAddress, stationCAFile: config.StationCAFile,
		provisionDial: provisioning.DialMachine, hooks: hooks, http: httpTools,
	}
}
