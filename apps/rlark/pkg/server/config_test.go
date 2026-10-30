package server

import "testing"

func TestClientConfigBuildRestConfigSetsRateLimits(t *testing.T) {
	config := ClientConfig{
		ServerAddress:         "https://rlark.example.com",
		ServerNamespace:       "data-cluster",
		InsecureSkipTLSVerify: true,
	}

	restConfig, err := config.BuildRestConfig()
	if err != nil {
		t.Fatal(err)
	}
	if restConfig.QPS != defaultClientQPS {
		t.Fatalf("QPS = %v, want %v", restConfig.QPS, defaultClientQPS)
	}
	if restConfig.Burst != defaultClientBurst {
		t.Fatalf("Burst = %d, want %d", restConfig.Burst, defaultClientBurst)
	}
}

func TestBuildRawRestConfigUsesScopedAPI(t *testing.T) {
	config := ClientConfig{ServerAddress: "https://control.example:8443", ServerNamespace: "agents", InsecureSkipTLSVerify: true}
	restConfig, err := config.BuildRawRestConfig()
	if err != nil {
		t.Fatal(err)
	}
	if restConfig.Host != "https://control.example:8443/api/raw/kubernetes" {
		t.Fatalf("Raw REST host = %q", restConfig.Host)
	}
}
