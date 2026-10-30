package docker

import "testing"

func TestLocalHostRejectsRemoteAndNonCanonicalSockets(t *testing.T) {
	for _, host := range []string{DefaultLocalHost, "unix:///run/custom-docker.sock"} {
		if err := ValidateLocalHost(host); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"", "tcp://localhost:2375", "http://localhost", "npipe://foo", "unix://relative", "unix:///", "unix:///run/../docker.sock", "unix:///run//docker.sock", "unix:///run/docker.sock?x=y", "unix:///run/docker.sock\n", "unix:///%2f.sock"} {
		if ValidateLocalHost(host) == nil {
			t.Fatalf("accepted %q", host)
		}
	}
}
