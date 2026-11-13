package controller

import (
	"bytes"
	"errors"
	"fmt"
)

func NodeResetObservationScript(uid string) (string, error) {
	if err := validateNodeOperationUID(uid); err != nil {
		return "", err
	}
	return fmt.Sprintf("set -eu\nbase='/var/lib/rlark/node-operations/%s.reset'\nif [ -e \"$base.done\" ] && [ ! -e /etc/kubernetes/kubelet.conf ] && [ ! -e /etc/kubernetes/bootstrap-kubelet.conf ]; then printf 'completed\\n'; elif [ -e \"$base\" ]; then printf 'attempted\\n'; else printf 'configured\\n'; if [ -r /var/lib/kubelet/pki/kubelet-client-current.pem ]; then openssl x509 -in /var/lib/kubelet/pki/kubelet-client-current.pem -outform PEM; fi; fi\n", uid), nil
}

func parseNodeResetObservation(output []byte) (string, []byte, error) {
	line, rest, ok := bytes.Cut(output, []byte("\n"))
	if !ok {
		return "", nil, errors.New("invalid reset observation")
	}
	state := string(line)
	cert := bytes.TrimSpace(rest)
	if state != "configured" && state != "attempted" && state != "completed" {
		return "", nil, errors.New("invalid reset state")
	}
	if state == "configured" && (!bytes.HasPrefix(cert, []byte("-----BEGIN CERTIFICATE-----")) || bytes.Contains(cert, []byte("PRIVATE KEY"))) {
		return "", nil, errors.New("kubelet certificate unavailable")
	}
	if state != "configured" && len(cert) != 0 {
		return "", nil, errors.New("unexpected reset observation output")
	}
	return state, cert, nil
}

func NodeResetAttemptScript(uid string) (string, error) {
	if err := validateNodeOperationUID(uid); err != nil {
		return "", err
	}
	return fmt.Sprintf("set -eu\numask 077\nmkdir -p /var/lib/rlark/node-operations\nbase='/var/lib/rlark/node-operations/%s.reset'\n[ ! -e \"$base\" ] || exit 43\n(set -C; : > \"$base\") || exit 43\nkubeadm reset --force\n[ ! -e /etc/kubernetes/kubelet.conf ] && [ ! -e /etc/kubernetes/bootstrap-kubelet.conf ]\n: > \"$base.done\"\n", uid), nil
}
