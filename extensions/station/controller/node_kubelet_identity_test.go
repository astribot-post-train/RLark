package controller

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestVerifyKubeletIdentity(t *testing.T) {
	now := time.Now()
	caPub, caKey, _ := ed25519.GenerateKey(rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	issue := func(name string, groups []string, usages []x509.ExtKeyUsage) []byte {
		cert := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name, Organization: groups}, ExtKeyUsage: usages, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, pub, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	good := issue("system:node:worker", []string{"system:nodes"}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if digest, err := VerifyKubeletIdentity(good, caPEM, "worker", now); err != nil || len(digest) != 64 {
		t.Fatalf("valid identity: %v", err)
	}
	for _, cert := range [][]byte{issue("system:node:other", []string{"system:nodes"}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}), issue("system:node:worker", []string{"other"}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}), issue("system:node:worker", []string{"system:nodes"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})} {
		if _, err := VerifyKubeletIdentity(cert, caPEM, "worker", now); err == nil {
			t.Fatal("accepted invalid identity")
		}
	}
}
