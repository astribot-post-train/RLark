package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/console"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.FromContext(ctx).Error(err, "container-runtime API failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	var addr, kubeconfig, certificate, key, agentCA, artifactDirectory, logAddr, jwtKey string
	var enableTasks, enableUpgrades bool
	// controller-runtime dependencies register kubeconfig on flag.CommandLine
	// during init. Keep this executable's flags private to avoid a startup panic.
	fs := flag.NewFlagSet("container-runtime-api", flag.ContinueOnError)
	fs.BoolVar(&enableTasks, "enable-task-execution", false, "enable preview node-scoped Raw/Docker Task transport")
	fs.BoolVar(&enableUpgrades, "enable-agent-upgrades", false, "enable preview node-scoped Agent upgrade grant/completion transport")
	fs.StringVar(&addr, "addr", ":9444", "HTTPS listen address")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "Management API kubeconfig (empty uses in-cluster config)")
	fs.StringVar(&certificate, "tls-cert", "", "HTTPS server certificate path")
	fs.StringVar(&key, "tls-key", "", "HTTPS server private key path")
	fs.StringVar(&agentCA, "agent-ca", "", "Dedicated container Agent client CA path")
	fs.StringVar(&artifactDirectory, "artifact-directory", "", "Read-only verified Agent artifact directory (optional)")
	fs.StringVar(&logAddr, "log-read-addr", "", "optional explicit loopback HTTP administrator logs/stats read address")
	fs.StringVar(&jwtKey, "gateway-jwt-key-file", "", "Gateway administrator JWT signing key; required with log-read-addr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("positional arguments are unsupported")
	}
	if err := validateLogListen(logAddr, jwtKey, enableTasks); err != nil {
		return err
	}
	if certificate == "" || key == "" || agentCA == "" {
		return errors.New("tls-cert, tls-key and dedicated agent-ca are required")
	}
	ca, err := os.ReadFile(agentCA)
	if err != nil {
		return fmt.Errorf("read Agent CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("agent CA contains no certificate")
	}
	tlsCert, err := tls.LoadX509KeyPair(certificate, key)
	if err != nil {
		return fmt.Errorf("load API certificate: %w", err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("build management API configuration: %w", err)
	}
	config.Timeout = 15 * time.Second
	scheme := runtime.NewScheme()
	if err := native.AddToScheme(scheme); err != nil {
		return err
	}
	if err := ext.AddToScheme(scheme); err != nil {
		return err
	}
	management, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create uncached management client: %w", err)
	}
	var store artifact.Store
	if artifactDirectory != "" {
		store, err = artifact.NewDirectoryStore(artifactDirectory)
		if err != nil {
			return err
		}
	}
	reporter := agent.Reporter{Client: management, ReleaseStore: store}
	var broker *agent.LogBroker
	var dockerBroker *agent.DockerDiagnosticBroker
	var logServer *http.Server
	if logAddr != "" {
		keyBytes, err := os.ReadFile(jwtKey)
		if err != nil {
			return fmt.Errorf("read Gateway verification key: %w", err)
		}
		auth, err := console.NewGatewayJWT(keyBytes)
		if err != nil {
			return err
		}
		broker = &agent.LogBroker{}
		dockerBroker = &agent.DockerDiagnosticBroker{}
		dockerReads := agent.DockerDiagnosticsService{Tasks: agent.TaskService{Reporter: reporter}, Broker: dockerBroker}
		reads := agent.RuntimeLogService{Raw: agent.LogService{Tasks: agent.TaskService{Reporter: reporter}, Broker: broker}, Docker: dockerReads}
		readMux := http.NewServeMux()
		readMux.Handle(console.LogPath, console.LogReadAPI{Auth: auth, Reader: reads})
		readMux.Handle(console.StatsPath, console.StatsReadAPI{Auth: auth, Reader: dockerReads})
		logServer = &http.Server{Addr: logAddr, Handler: readMux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 25 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	}
	server := &http.Server{Addr: addr, Handler: agent.Handler{Reporter: reporter, EnableTasks: enableTasks, EnableUpgrades: enableUpgrades, Logs: broker, DockerDiagnostics: dockerBroker},
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{tlsCert}}}
	g, groupCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return serve(groupCtx, server, true) })
	if logServer != nil {
		g.Go(func() error { return serve(groupCtx, logServer, false) })
	}
	return g.Wait()
}

func validateLogListen(addr, key string, tasks bool) error {
	if addr == "" && key == "" {
		return nil
	}
	if addr == "" || key == "" || !tasks {
		return errors.New("log-read-addr requires gateway-jwt-key-file and enable-task-execution")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("explicit loopback log read address required")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("log read listener must use an explicit loopback IP")
	}
	return nil
}

func serve(ctx context.Context, server *http.Server, tls bool) error {
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				_ = server.Close()
			}
		case <-stopped:
		}
	}()
	var err error
	if tls {
		err = server.ListenAndServeTLS("", "")
	} else {
		err = server.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
