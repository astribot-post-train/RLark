// container-runtime-admin performs explicit administrator-owned provisioning.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func decode(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // Input is read-only; decode errors remain authoritative.
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("JSON request exceeds 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("one strict JSON request required")
	}
	return nil
}
func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("command required: cluster-create, node-register, node-package")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := fs.String("config", "", "strict JSON request file")
	kubeconfig := fs.String("kubeconfig", "", "administrator management kubeconfig")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || fs.NArg() != 0 {
		return errors.New("--config is required; positional arguments are unsupported")
	}
	if args[0] != "cluster-create" && args[0] != "node-register" && args[0] != "node-package" {
		return errors.New("unsupported administrator command")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return fmt.Errorf("load administrator management credentials: %w", err)
	}
	cfg.Timeout = 15 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, native.AddToScheme, ext.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	management, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	service := provision.Service{Client: management}
	var result any
	switch args[0] {
	case "cluster-create":
		var req provision.ClusterRequest
		if err := decode(*path, &req); err != nil {
			return err
		}
		result, err = service.CreateCluster(ctx, req)
	case "node-register":
		var req provision.NodeRequest
		if err := decode(*path, &req); err != nil {
			return err
		}
		result, err = service.RegisterNode(ctx, req)
	case "node-package":
		var req provision.PackageRequest
		if err := decode(*path, &req); err != nil {
			return err
		}
		result, err = service.ExportPackage(ctx, req)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
