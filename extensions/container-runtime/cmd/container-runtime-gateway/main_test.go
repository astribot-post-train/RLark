package main

import (
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestGatewayCommandValidation(t *testing.T) {
	if err := run(context.Background(), []string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help unexpectedly requires credentials: %v", err)
	}
	for _, tt := range []struct {
		name    string
		args    []string
		message string
	}{
		{"missing mode", nil, "mode must"},
		{"unexpected argument", []string{"gateway"}, "positional"},
		{"hub ray config", []string{"--mode=hub", "--ray-config=/private/config.json"}, "only supported in gateway"},
		{"missing credentials", []string{"--mode=gateway", "--ray-config=/private/config.json"}, "roots and"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := run(context.Background(), tt.args); err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("invalid startup configuration accepted: %v", err)
			}
		})
	}
}
