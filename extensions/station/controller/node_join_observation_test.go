package controller

import (
	"strings"
	"testing"
)

func TestJoinObservationRejectsUnexpectedOrPrivateData(t *testing.T) {
	state, cert, err := parseNodeJoinObservation([]byte("attempted\n-----BEGIN CERTIFICATE-----\ndata\n-----END CERTIFICATE-----\n"))
	if err != nil || !state.Attempted || len(cert) == 0 {
		t.Fatalf("valid observation: %+v %v", state, err)
	}
	for _, output := range []string{"attempted", "unknown\n", "fresh\n-----BEGIN PRIVATE KEY-----\nsecret", "attempted\n-----BEGIN CERTIFICATE-----\n-----BEGIN PRIVATE KEY-----"} {
		if _, _, err := parseNodeJoinObservation([]byte(output)); err == nil {
			t.Fatalf("accepted invalid observation %q", output)
		}
	}
	script, err := NodeJoinObservationScript("01234567-89ab-cdef-0123-456789abcdef")
	if err != nil || !strings.Contains(script, "openssl x509") || strings.Contains(script, "cat /var/lib/kubelet/pki") {
		t.Fatalf("unsafe observation script: %v", err)
	}
}
