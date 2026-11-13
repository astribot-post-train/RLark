package controller

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// NodeJoinObservationScript emits a fixed state line and, when present, the
// public certificate only. kubelet-client-current.pem may also contain a key.
func NodeJoinObservationScript(operationUID string) (string, error) {
	if err := validateNodeOperationUID(operationUID); err != nil {
		return "", err
	}
	return fmt.Sprintf("set -eu\nmarker='/var/lib/rlark/node-operations/%s.join'\nif [ -e \"$marker\" ]; then printf 'attempted\\n'; elif [ -e /etc/kubernetes/kubelet.conf ] || [ -e /etc/kubernetes/bootstrap-kubelet.conf ]; then printf 'configured\\n'; else printf 'fresh\\n'; fi\nif [ -r /var/lib/kubelet/pki/kubelet-client-current.pem ]; then openssl x509 -in /var/lib/kubelet/pki/kubelet-client-current.pem -outform PEM; fi\n", operationUID), nil
}

func parseNodeJoinObservation(output []byte) (NodeJoinState, []byte, error) {
	line, rest, ok := bytes.Cut(output, []byte("\n"))
	if !ok {
		return NodeJoinState{}, nil, errors.New("invalid join observation")
	}
	state := NodeJoinState{}
	switch string(line) {
	case "attempted":
		state.Attempted = true
	case "configured":
		state.ExistingConfiguration = true
	case "fresh":
	default:
		return NodeJoinState{}, nil, errors.New("invalid join observation state")
	}
	certificate := bytes.TrimSpace(rest)
	if len(certificate) != 0 && (!bytes.HasPrefix(certificate, []byte("-----BEGIN CERTIFICATE-----")) || strings.Contains(string(certificate), "PRIVATE KEY")) {
		return NodeJoinState{}, nil, errors.New("invalid join observation certificate")
	}
	return state, certificate, nil
}
