package controller

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestLiveManagementClusterAndNodeReaders(t *testing.T) {
	kubeconfig := os.Getenv("STATION_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set STATION_TEST_KUBECONFIG to an isolated management API kubeconfig")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := (ManagementClusterResolver{Reader: c}).ResolveNamespace(context.Background(), "rlark-local-dev-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if registration.ClusterID != "rlark-local-dev-gpu" || registration.Namespace != "rlark-local-dev-gpu" {
		t.Fatalf("unexpected live registration: %#v", registration)
	}
	ready, err := (ManagementNodeReadiness{Reader: c}).Ready(context.Background(), registration.ClusterID, registration.Namespace, "cailiang")
	if err != nil || !ready {
		t.Fatalf("live Node readiness=%t err=%v", ready, err)
	}
}
