// The independent console is deliberately separate from the Agent mTLS API.
package main

import (
	"context"
	"crypto/tls"
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
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/console"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/submission"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.FromContext(ctx).Error(err, "container console failed")
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	var addr, kubeconfig, keyFile, certFile, tlsKey, uiDirectory, gateway, logOrigin string
	var enableJobs, enableRegistration bool
	fs := flag.NewFlagSet("container-runtime-console", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", "127.0.0.1:9445", "listen address; HTTP is loopback only")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "restricted management kubeconfig; empty uses in-cluster config")
	fs.StringVar(&keyFile, "gateway-jwt-key-file", "", "read-only Gateway signing key file (at least 32 bytes)")
	fs.StringVar(&gateway, "gateway-origin", "", "trusted Gateway HTTPS origin; loopback HTTP allowed for development")
	fs.StringVar(&logOrigin, "log-api-origin", "", "optional loopback HTTP administrator log API origin")
	fs.BoolVar(&enableJobs, "enable-job-submission", false, "enable preview single-process Raw Job submit/list/suspend; requires additive Job RBAC")
	fs.BoolVar(&enableRegistration, "enable-node-registration", false, "enable administrator Raw node registration; requires additive registration RBAC")
	fs.StringVar(&certFile, "tls-cert", "", "optional HTTPS certificate; required off loopback")
	fs.StringVar(&tlsKey, "tls-key", "", "HTTPS private key")
	fs.StringVar(&uiDirectory, "ui-directory", "", "built independent UI directory (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("positional arguments are unsupported")
	}
	if keyFile == "" || gateway == "" {
		return errors.New("gateway-jwt-key-file and gateway-origin required")
	}
	if err := validateListen(addr, certFile, tlsKey); err != nil {
		return err
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("read Gateway verification key: %w", err)
	}
	auth, err := console.NewGatewayJWT(key)
	if err != nil {
		return err
	}
	login, err := console.NewLogin(gateway, auth)
	if err != nil {
		return err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("load management configuration: %w", err)
	}
	cfg.Timeout = 15 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{native.AddToScheme, ext.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	management, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create management client: %w", err)
	}
	api := &console.API{Client: management, Creator: provision.Service{Client: management}, Auth: auth}
	if enableRegistration {
		api.Registrar = provision.Service{Client: management}
	}
	if enableJobs {
		api.Jobs = submission.Service{Client: management}
	}
	if logOrigin != "" {
		api.Logs, err = console.NewLogProxy(logOrigin)
		if err != nil {
			return err
		}
	}
	if uiDirectory != "" {
		info, err := os.Stat(uiDirectory + "/index.html")
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("ui-directory must contain built index.html")
		}
		api.UI = http.FileServer(http.Dir(uiDirectory))
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/login", login)
	mux.Handle("/", api)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				log.FromContext(ctx).Error(err, "console shutdown failed")
			}
		case <-stopped:
		}
	}()
	if certFile != "" {
		err = server.ListenAndServeTLS(certFile, tlsKey)
	} else {
		err = server.ListenAndServe()
	}
	close(stopped)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func validateListen(addr, cert, key string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if (cert == "") != (key == "") {
		return errors.New("both TLS certificate and key required")
	}
	if cert == "" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("HTTPS required off loopback")
		}
	}
	return nil
}
