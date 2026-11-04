package main

import (
	"context"
	"testing"
)

func TestCommandRejectsMissingOrInvalidConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"--mode=other", "--config=/missing"}, {"--mode=peer"}, {"--mode=peer", "--config=/missing"}, {"--mode=gateway", "--config=/missing", "extra"}} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("accepted invalid command %v", args)
		}
	}
	if err := run(context.Background(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
}
