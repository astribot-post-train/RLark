package controller

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strings"
	"time"
)

// VerifyKubeletIdentity proves a remote kubelet certificate belongs to the
// expected node and chains to the registered member cluster CA.
func VerifyKubeletIdentity(certPEM, caPEM []byte, expectedNode string, now time.Time) (string, error) {
	if expectedNode == "" {
		return "", errors.New("node name is required")
	}
	block, rest := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return "", errors.New("invalid kubelet client certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", errors.New("invalid kubelet client certificate")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return "", errors.New("invalid member cluster CA")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", errors.New("kubelet certificate does not verify against member cluster CA")
	}
	if cert.Subject.CommonName != "system:node:"+expectedNode {
		return "", errors.New("kubelet certificate node name mismatch")
	}
	groupFound := false
	for _, group := range cert.Subject.Organization {
		if group == "system:nodes" {
			groupFound = true
		}
	}
	if !groupFound {
		return "", errors.New("kubelet certificate group mismatch")
	}
	digest := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(digest[:]), nil
}
