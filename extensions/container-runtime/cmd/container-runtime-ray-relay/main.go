// Command container-runtime-ray-relay hosts the opt-in preview Ray transport.
// Gateway mode is intended for A; peer mode is intended for each approved node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/rayrelay"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "container-runtime-ray-relay:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("container-runtime-ray-relay", flag.ContinueOnError)
	mode := fs.String("mode", "", "gateway (A) or peer (Raw Ray node)")
	config := fs.String("config", "", "private JSON transport configuration")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || *config == "" || (*mode != "gateway" && *mode != "peer") {
		return errors.New("--mode=gateway|peer and --config are required; positional arguments are unsupported")
	}
	if *mode == "gateway" {
		server, err := rayrelay.LoadGateway(*config)
		if err != nil {
			return err
		}
		return server.Serve(ctx)
	}
	peer, err := rayrelay.LoadPeer(*config)
	if err != nil {
		return err
	}
	return peer.Run(ctx)
}
