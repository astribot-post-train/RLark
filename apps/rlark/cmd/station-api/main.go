package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rlinf/rlark/apps/rlark/pkg/configs"
	"github.com/rlinf/rlark/apps/rlark/pkg/stationidentity"
	"github.com/spf13/cobra"
	"github.com/uptrace/bun/driver/pgdriver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func main() {
	config := configs.DefaultKubernetesClientConfig()
	var address, credentialsNamespace, certFile, keyFile, gatewayCredentialFile string
	var bootstrap stationidentity.Bootstrap
	var bootstrapCAFile string
	cmd := &cobra.Command{
		Use:   "station-api",
		Short: "Run the Station identity and observation API",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			return run(ctx, config, address, credentialsNamespace, certFile, keyFile, gatewayCredentialFile, &bootstrap, bootstrapCAFile)
		},
	}
	config.SetupFlags(cmd.Flags())
	cmd.Flags().StringVar(&address, "addr", ":8443", "HTTPS listener address")
	cmd.Flags().StringVar(&credentialsNamespace, "member-credentials-namespace", "station-member-credentials", "namespace containing registered member kubeconfig Secrets")
	cmd.Flags().StringVar(&certFile, "tls-cert-file", os.Getenv("STATION_API_TLS_CERT_FILE"), "TLS certificate path mounted into the container")
	cmd.Flags().StringVar(&keyFile, "tls-key-file", os.Getenv("STATION_API_TLS_KEY_FILE"), "TLS private key path mounted into the container")
	cmd.Flags().StringVar(&gatewayCredentialFile, "gateway-credential-file", "", "Optional dedicated Gateway network-summary credential file")
	cmd.Flags().StringVar(&bootstrapCAFile, "bootstrap-ca-file", "", "CA certificate machines verify this API with; enables node admission by one-time code")
	cmd.Flags().StringVar(&bootstrap.ImageFile, "bootstrap-image-file", "", "Installer image served to admitted machines, as a docker archive")
	cmd.Flags().StringVar(&bootstrap.ImageRef, "bootstrap-image-ref", "", "Name of the installer image inside the archive")
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "station-api failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, kubeConfig configs.KubernetesClientConfig, address, credentialsNamespace, certFile, keyFile, gatewayCredentialFile string, bootstrap *stationidentity.Bootstrap, bootstrapCAFile string) error {
	if address == "" || credentialsNamespace == "" || certFile == "" || keyFile == "" {
		return errors.New("listener, credential namespace and TLS certificate paths are required")
	}
	dsn := os.Getenv("STATION_API_DATABASE_DSN")
	if dsn == "" {
		return errors.New("STATION_API_DATABASE_DSN is required")
	}
	if err := stationidentity.ValidatePostgresDSN(dsn); err != nil {
		return err
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return errors.New("load Station API TLS certificate")
	}

	config, err := kubeConfig.BuildRestConfig()
	if err != nil {
		return errors.New("load management API credentials")
	}
	config.Timeout = 10 * time.Second
	management, err := dynamic.NewForConfig(config)
	if err != nil {
		return errors.New("create management API client")
	}
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.New("create management core API client")
	}
	registry, err := stationidentity.NewKubernetesClusterRegistry(management, core, credentialsNamespace, nil)
	if err != nil {
		return errors.New("configure trusted member registry")
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithTimeout(5*time.Second), pgdriver.WithReadTimeout(10*time.Second), pgdriver.WithWriteTimeout(10*time.Second)))
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	defer func() { _ = db.Close() }()
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		pingCancel()
		return errors.New("connect to Station API database")
	}
	pingCancel()
	challenges, err := stationidentity.NewPostgresChallengeStore(db, "station_api.challenges")
	if err != nil {
		return errors.New("configure durable challenge store")
	}
	observationLimiter, err := stationidentity.NewTokenBucketObservationLimiter(1, 5, 10000, 10*time.Minute)
	if err != nil {
		return errors.New("configure Station observation rate limiter")
	}
	enrollmentLimiter, err := stationidentity.NewTokenBucketObservationLimiter(1, 5, 10000, 10*time.Minute)
	if err != nil {
		return errors.New("configure network enrollment rate limiter")
	}
	api := &stationidentity.API{
		Registry:           registry,
		Challenges:         challenges,
		ObservationLimiter: observationLimiter,
		Observations: &stationidentity.KubernetesObservationSink{
			Client: management, Namespaces: stationidentity.KubernetesClusterNamespaceResolver{Client: management},
		},
		Enrollments: &stationidentity.KubernetesNetworkEnroller{
			Client: management, Namespaces: stationidentity.KubernetesClusterNamespaceResolver{Client: management},
		},
		EnrollmentLimiter: enrollmentLimiter,
		Ready: func(readyCtx context.Context) error {
			if err := db.PingContext(readyCtx); err != nil {
				return err
			}
			_, err := management.Resource(rlarkClusterGVR).List(readyCtx, metav1.ListOptions{Limit: 1})
			return err
		},
	}
	if bootstrapCAFile != "" {
		ca, err := os.ReadFile(bootstrapCAFile)
		if err != nil || bootstrap.ImageFile == "" || bootstrap.ImageRef == "" {
			return errors.New("node admission needs a readable CA file, the installer archive and its image name")
		}
		bootstrap.CA, bootstrap.Management, bootstrap.Member = ca, management, registry.MemberClient
		api.Bootstrap = bootstrap
	}
	if gatewayCredentialFile != "" {
		api.NetworkSummary = &stationidentity.NodeNetworkSummaryReader{Registry: registry, Client: management, Namespaces: stationidentity.KubernetesClusterNamespaceResolver{Client: management}}
		api.GatewayCredential = func() ([]byte, error) { return readGatewayCredential(gatewayCredentialFile) }
	}
	server := &http.Server{
		Addr: address, Handler: api,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.New("bind Station API listener")
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ServeTLS(listener, certFile, keyFile) }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("Station API listener stopped")
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return errors.New("shut down Station API listener")
		}
		return nil
	}
}

func readGatewayCredential(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("invalid gateway credential")
	}
	return []byte(token), nil
}

var rlarkClusterGVR = schema.GroupVersionResource{Group: "rlinf.io", Version: "v1alpha1", Resource: "clusters"}
