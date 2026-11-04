package enrollment

import (
	"reflect"
	"testing"
)

func TestPerNodeWorkloadAllowlistOverrides(t *testing.T) {
	p := Profile{Executables: []string{"/usr/bin/python3.10"}, WorkDirectories: []string{"/tmp"}, ExecutablesByNode: map[string][]string{"b": {"/root/python-rlark"}}, WorkDirectoriesByNode: map[string][]string{"b": {"/root/RLinf"}}}
	executables, directories, err := p.workloadPaths("b")
	if err != nil || !reflect.DeepEqual(executables, []string{"/root/python-rlark"}) || !reflect.DeepEqual(directories, []string{"/root/RLinf"}) {
		t.Fatal("worker did not use its private allowlist", err)
	}
	executables, directories, err = p.workloadPaths("a")
	if err != nil || !reflect.DeepEqual(executables, p.Executables) || !reflect.DeepEqual(directories, p.WorkDirectories) {
		t.Fatal("default allowlists changed", err)
	}
	p.ExecutablesByNode["b"] = nil
	if _, _, err := p.workloadPaths("b"); err == nil {
		t.Fatal("empty explicit override fell back to defaults")
	}
	p.ExecutablesByNode["b"] = []string{"relative/python"}
	if _, _, err := p.workloadPaths("b"); err == nil {
		t.Fatal("relative executable accepted")
	}
}

func TestLaunchValuesUseDiscoveredAddresses(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://$GATEWAY_PRIVATE_ADDRESS:9444", "https://10.1.3.214:9444"},
		{"--agent-listen=$NODE_PRIVATE_ADDRESS:9444", "--agent-listen=10.1.2.82:9444"},
		{"--config=$INSTALL/services.json", "--config=/opt/rlark/node/services.json"},
		{"--hub-listen=0.0.0.0:30000", "--hub-listen=0.0.0.0:30000"},
	} {
		value, err := expandLaunchValue(tc.input, "/opt/rlark/node", "10.1.2.82", "10.1.3.214")
		if err != nil || value != tc.want {
			t.Fatalf("expand %q = %q, %v; want %q", tc.input, value, err, tc.want)
		}
	}
	for _, address := range []string{"", "36.103.180.181", "::1", "127.0.0.1", "invalid"} {
		if _, err := expandLaunchValue("https://$GATEWAY_PRIVATE_ADDRESS:9444", "/opt/node", "10.1.2.82", address); err == nil {
			t.Fatalf("accepted unverified gateway address %q", address)
		}
	}
	if _, err := expandLaunchValue("$UNKNOWN", "/opt/node", "10.1.2.82", "10.1.3.214"); err == nil {
		t.Fatal("unknown placeholder accepted")
	}
}
