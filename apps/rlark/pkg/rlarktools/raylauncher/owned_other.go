//go:build !unix

package raylauncher

import (
	"context"
	"errors"
)

// startOwned is unavailable without process groups; shared-host Ray runs on Unix Raw nodes only.
func startOwned(context.Context, string, ...string) func() error {
	return func() error { return errors.New("shared-host Ray requires Unix process groups") }
}
