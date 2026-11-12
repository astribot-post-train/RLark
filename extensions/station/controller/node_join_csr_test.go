package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"testing"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestVerifyJoinCSRRequiresOperationAndApproval(t *testing.T) {
	ctx := context.Background()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("certificate")})
	digest := sha256.Sum256([]byte("operation"))
	csr := &certv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{Name: "csr"}, Spec: certv1.CertificateSigningRequestSpec{Username: "system:bootstrap:" + hex.EncodeToString(digest[:3]), SignerName: certv1.KubeAPIServerClientKubeletSignerName}, Status: certv1.CertificateSigningRequestStatus{Certificate: certificate, Conditions: []certv1.CertificateSigningRequestCondition{{Type: certv1.CertificateApproved, Status: corev1.ConditionTrue}}}}
	client := fake.NewSimpleClientset(csr)
	if ok, err := VerifyJoinCSR(ctx, client, "operation", certificate); err != nil || !ok {
		t.Fatalf("owned CSR: %v %v", ok, err)
	}
	if ok, err := VerifyJoinCSR(ctx, client, "other", certificate); err != nil || ok {
		t.Fatalf("other operation: %v %v", ok, err)
	}
	if ok, err := VerifyJoinCSR(ctx, client, "operation", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("other")})); err != nil || ok {
		t.Fatalf("other certificate: %v %v", ok, err)
	}
	csr.Status.Conditions = append(csr.Status.Conditions, certv1.CertificateSigningRequestCondition{Type: certv1.CertificateDenied, Status: corev1.ConditionTrue})
	client = fake.NewSimpleClientset(csr)
	if ok, err := VerifyJoinCSR(ctx, client, "operation", certificate); err != nil || ok {
		t.Fatalf("denied CSR: %v %v", ok, err)
	}
}
