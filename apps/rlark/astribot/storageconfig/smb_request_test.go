package storageconfig

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSMBReachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)

	assert.NoError(t, validateSMBReachable(context.Background(), host, port))

	require.NoError(t, listener.Close())
	assert.Error(t, validateSMBReachable(context.Background(), host, port))
}
