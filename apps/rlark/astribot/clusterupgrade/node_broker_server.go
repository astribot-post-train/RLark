package clusterupgrade

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/apis"
	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type brokerMemberConfig struct {
	ClusterUID    types.UID `json:"clusterUID"`
	Kubeconfig    string    `json:"kubeconfig"`
	ExecutorImage string    `json:"executorImage,omitempty"`
}

type brokerServerConfig struct {
	AutoIssueBatchGrants bool                          `json:"autoIssueBatchGrants,omitempty"`
	Listen               string                        `json:"listen"`
	TLSCert              string                        `json:"tlsCert"`
	TLSKey               string                        `json:"tlsKey"`
	Management           string                        `json:"managementKubeconfig"`
	Members              map[string]brokerMemberConfig `json:"members"`
}

type brokerMember struct {
	uid           types.UID
	executorImage string
	client        kubernetes.Interface
}

// ServeNodeImageBroker loads a trusted explicit member inventory and runs the
// broker on an internal TLS listener. It never uses the release kubeconfig
// implicitly and cannot enroll a member using data received over HTTP.
func ServeNodeImageBroker(ctx context.Context, configFile string) error {
	file, err := os.Open(configFile)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	var config brokerServerConfig
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid node image broker configuration")
	}
	if config.Management == "" || config.Listen == "" || config.TLSCert == "" || config.TLSKey == "" || len(config.Members) == 0 {
		return errors.New("node image broker requires TLS, management identity and an explicit member inventory")
	}
	host, _, err := net.SplitHostPort(config.Listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		return errors.New("node image broker requires an explicit listen address")
	}
	mgmtConfig, err := loadBrokerKubeconfig(config.Management)
	if err != nil {
		return fmt.Errorf("load node image broker management identity: %w", err)
	}
	mgmt, err := dynamic.NewForConfig(mgmtConfig)
	if err != nil {
		return err
	}
	members := make(map[string]brokerMember, len(config.Members))
	for name, member := range config.Members {
		if member.ClusterUID == "" || member.Kubeconfig == "" {
			return fmt.Errorf("member %s requires an identity and verification kubeconfig", name)
		}
		conf, err := loadBrokerKubeconfig(member.Kubeconfig)
		if err != nil {
			return fmt.Errorf("load verifier identity for %s: %w", name, err)
		}
		memberClient, err := kubernetes.NewForConfig(conf)
		if err != nil {
			return err
		}
		if config.AutoIssueBatchGrants {
			if _, err := pinnedImage(member.ExecutorImage); err != nil {
				return fmt.Errorf("member %s requires a digest-pinned approved executor for batch grants", name)
			}
		}
		members[name] = brokerMember{uid: member.ClusterUID, executorImage: member.ExecutorImage, client: memberClient}
	}
	broker := NodeImageBroker{
		Clusters: mgmt.Resource(schema.GroupVersionResource{Group: "rlinf.io", Version: "v1alpha1", Resource: "clusters"}),
		Member: func(_ context.Context, name string, uid types.UID) (kubernetes.Interface, error) {
			member, found := members[name]
			if !found || uid != member.uid {
				return nil, ErrNodeImageGrant
			}
			return member.client, nil
		},
	}
	if config.AutoIssueBatchGrants {
		scheme := runtime.NewScheme()
		if err := rlark.AddToScheme(scheme); err != nil {
			return err
		}
		management, err := client.New(mgmtConfig, client.Options{Scheme: scheme})
		if err != nil {
			return err
		}
		authorizer, err := kubernetes.NewForConfig(mgmtConfig)
		if err != nil {
			return err
		}
		broker.Issuer = &BatchGrantIssuer{
			Management: management,
			CheckControllerOwnership: func(ctx context.Context, namespace, child, parent string) error {
				return verifyBatchWriters(ctx, authorizer, namespace, child, parent)
			},
			ApprovedExecutor: func(name string, uid types.UID) (string, bool) {
				member, ok := members[name]
				return member.executorImage, ok && member.uid == uid
			},
		}
	}
	server := &http.Server{
		Addr: config.Listen, Handler: broker, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	})
	defer stop()
	err = server.ListenAndServeTLS(config.TLSCert, config.TLSKey)
	if errors.Is(err, http.ErrServerClosed) {
		<-stopped
		return nil
	}
	return err
}

func loadBrokerKubeconfig(path string) (*rest.Config, error) {
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	server, err := url.Parse(config.Host)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || config.Insecure {
		return nil, errors.New("broker kubeconfig must use verified HTTPS")
	}
	return config, nil
}

// Effective permissions can differ from the role templates. Old Agents must
// refresh their roles before the management plane can trust child provenance.
func verifyBatchWriters(ctx context.Context, management kubernetes.Interface, namespace, child, parent string) error {
	identity := "system:serviceaccount:" + namespace + ":" + apis.RLarkAgentServiceAccountName
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"}
	for _, target := range []authv1.ResourceAttributes{
		{Resource: "clusterupgradejobs", Namespace: namespace, Name: child},
		{Resource: "upgradejobs", Name: parent},
		{Resource: "clusters", Name: namespace},
	} {
		target.Group = rlark.GroupVersion.Group
		for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
			attributes := target
			attributes.Verb = verb
			if verb == "create" || verb == "deletecollection" {
				attributes.Name = ""
			}
			sar, err := management.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authv1.SubjectAccessReview{
				Spec: authv1.SubjectAccessReviewSpec{User: identity, Groups: groups, ResourceAttributes: &attributes},
			}, metav1.CreateOptions{})
			if err != nil {
				return err
			}
			if sar.Status.Allowed || sar.Status.EvaluationError != "" {
				return ErrBatchAuthority
			}
		}
	}
	return nil
}
