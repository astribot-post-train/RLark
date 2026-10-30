package main

import (
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestStartupFlagsAreIndependentFromDependencyGlobals(t *testing.T) {
	// The real dependency graph registers kubeconfig on the global FlagSet.
	// Parse repeatedly in this process to catch both dependency collisions and
	// accidental re-registration; neither case should panic before validation.
	for range 2 {
		if err := run(context.Background(), []string{"--help"}); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("help: %v", err)
		}
		if err := run(context.Background(), []string{"--kubeconfig=/missing/management"}); err == nil || !strings.Contains(err.Error(), "tls-cert") {
			t.Fatalf("expected missing TLS prerequisites before management access: %v", err)
		}
	}
}

func TestStartupRejectsUnexpectedArguments(t *testing.T) {
	for _, args := range [][]string{{"--unknown"}, {"unexpected"}} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
