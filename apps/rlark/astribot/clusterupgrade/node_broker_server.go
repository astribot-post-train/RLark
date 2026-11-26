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
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type brokerMemberConfig struct {
	ClusterUID types.UID `json:"clusterUID"`
	Kubeconfig string    `json:"kubeconfig"`
}

type brokerServerConfig struct {
	Listen     string                        `json:"listen"`
	TLSCert    string                        `json:"tlsCert"`
	TLSKey     string                        `json:"tlsKey"`
	Management string                        `json:"managementKubeconfig"`
	Members    map[string]brokerMemberConfig `json:"members"`
}

type brokerMember struct {
	uid    types.UID
	client kubernetes.Interface
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
	mgmtConfig, err := clientcmd.BuildConfigFromFlags("", config.Management)
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
		conf, err := clientcmd.BuildConfigFromFlags("", member.Kubeconfig)
		if err != nil {
			return fmt.Errorf("load verifier identity for %s: %w", name, err)
		}
		memberClient, err := kubernetes.NewForConfig(conf)
		if err != nil {
			return err
		}
		members[name] = brokerMember{uid: member.ClusterUID, client: memberClient}
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
