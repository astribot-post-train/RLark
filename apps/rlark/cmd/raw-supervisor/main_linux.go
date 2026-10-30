//go:build linux

// raw-supervisor runs separately from the Agent inside an existing container.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/raw/supervision"
)

func run(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	var config supervision.Config
	decodeErr := decoder.Decode(&config)
	if decodeErr == nil {
		if err := decoder.Decode(new(any)); err != io.EOF {
			decodeErr = errors.New("supervisor configuration must contain one JSON object")
		}
	}
	if err := errors.Join(decodeErr, f.Close()); err != nil {
		return err
	}
	service, err := supervision.Open(config)
	if err != nil {
		return err
	}
	return errors.Join(service.Serve(ctx), service.Close())
}

func main() {
	config := flag.String("config", "", "private JSON configuration file (required)")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if *config == "" {
		slog.Error("--config is required")
		os.Exit(2)
	}
	if err := run(ctx, *config); err != nil {
		slog.Error("Raw supervisor stopped", "error", err)
		os.Exit(1)
	}
}
