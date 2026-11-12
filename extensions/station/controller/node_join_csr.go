package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"

	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// VerifyJoinCSR binds the observed kubelet certificate to the bootstrap token
// issued for this operation. Name and CA alone cannot prove operation ownership.
func VerifyJoinCSR(ctx context.Context, member kubernetes.Interface, operationUID types.UID, certificate []byte) (bool, error) {
	if member == nil || operationUID == "" {
		return false, errors.New("member client and operation UID are required")
	}
	block, _ := pem.Decode(certificate)
	if block == nil || block.Type != "CERTIFICATE" {
		return false, errors.New("invalid observed kubelet certificate")
	}
	digest := sha256.Sum256([]byte(operationUID))
	username := "system:bootstrap:" + hex.EncodeToString(digest[:3])
	list, err := member.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list kubelet certificate requests: %w", err)
	}
	for _, csr := range list.Items {
		if csr.Spec.Username != username || csr.Spec.SignerName != certv1.KubeAPIServerClientKubeletSignerName {
			continue
		}
		if len(csr.Status.Certificate) == 0 {
			continue
		}
		issued, _ := pem.Decode(csr.Status.Certificate)
		if issued == nil || issued.Type != "CERTIFICATE" || !bytes.Equal(issued.Bytes, block.Bytes) {
			continue
		}
		approved, denied := false, false
		for _, condition := range csr.Status.Conditions {
			if condition.Type == certv1.CertificateApproved && condition.Status == "True" {
				approved = true
			}
			if (condition.Type == certv1.CertificateDenied || condition.Type == certv1.CertificateFailed) && condition.Status == "True" {
				denied = true
			}
		}
		if approved && !denied {
			return true, nil
		}
	}
	return false, nil
}
