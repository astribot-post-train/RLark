package storage

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Vectors copied from rclone v1.71.2 fs/config/obscure/obscure_test.go so the
// gateway output stays compatible with the rclone CSI driver.
func TestRcloneObscureMatchesRcloneVectors(t *testing.T) {
	for _, test := range []struct{ in, want, iv string }{
		{"", "YWFhYWFhYWFhYWFhYWFhYQ", "aaaaaaaaaaaaaaaa"},
		{"potato", "YWFhYWFhYWFhYWFhYWFhYXMaGgIlEQ", "aaaaaaaaaaaaaaaa"},
		{"potato", "YmJiYmJiYmJiYmJiYmJiYp3gcEWbAw", "bbbbbbbbbbbbbbbb"},
	} {
		obscureRand = bytes.NewBufferString(test.iv)
		got, err := RcloneObscure(test.in)
		obscureRand = rand.Reader
		require.NoError(t, err)
		assert.Equal(t, test.want, got)

		revealed, err := RcloneReveal(got)
		require.NoError(t, err)
		assert.Equal(t, test.in, revealed)
	}
}

func TestRcloneObscureRoundTripUsesRandomIV(t *testing.T) {
	first, err := RcloneObscure("p@ss:word=with;symbols")
	require.NoError(t, err)
	second, err := RcloneObscure("p@ss:word=with;symbols")
	require.NoError(t, err)
	assert.NotEqual(t, first, second)

	revealed, err := RcloneReveal(first)
	require.NoError(t, err)
	assert.Equal(t, "p@ss:word=with;symbols", revealed)
}

func TestRcloneRevealRejectsInvalidInput(t *testing.T) {
	_, err := RcloneReveal("not base64!")
	assert.Error(t, err)
	_, err = RcloneReveal("YWFh")
	assert.Error(t, err)
}
