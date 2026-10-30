package main

import (
	"context"
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandValidatesTransportBeforeManagementAccess(t *testing.T) {
	require.ErrorIs(t, run(context.Background(), []string{"--help"}), flag.ErrHelp)
	for _, args := range [][]string{{}, {"unexpected"}, {"--unknown"}, {"--gateway-jwt-key-file=/missing", "--gateway-origin=https://gateway.example", "--addr=0.0.0.0:9445"}} {
		require.Error(t, run(context.Background(), args))
	}
	for _, addr := range []string{"0.0.0.0:9445", ":9445", "192.168.1.1:9445", "localhost:9445", "invalid"} {
		require.Error(t, validateListen(addr, "", ""))
	}
	require.NoError(t, validateListen("127.0.0.1:9445", "", ""))
	require.NoError(t, validateListen("[::1]:9445", "", ""))
	require.NoError(t, validateListen(":9445", "cert", "key"))
	require.Error(t, validateListen("127.0.0.1:9445", "cert", ""))
}
