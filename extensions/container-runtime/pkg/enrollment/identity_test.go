package enrollment

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"testing"
)

// Exercise the exact shipped SSH program, including identity stability across
// separate Python/SSH processes. Discovery must not fingerprint its own process.
func TestNotebookProbeIdentityStableAcrossProcesses(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required")
	}
	var first string
	for i := 0; i < 2; i++ {
		out, err := exec.Command(python, "-c", identityScript+"\nprint(instance_id())").CombinedOutput()
		if err != nil {
			if i == 0 && regexp.MustCompile(`PermissionError`).Match(out) {
				t.Skip("proc identity is restricted in the test sandbox")
			}
			t.Fatalf("instance probe: %v: %s", err, out)
		}
		value := string(out)
		if !regexp.MustCompile(`^[a-f0-9]{64}\n$`).MatchString(value) || i > 0 && value != first {
			t.Fatalf("unstable or invalid instance fingerprint: %q", value)
		}
		first = value
	}
	// Confirm the launch guard and the SSH discovery share one implementation.
	out, err := exec.Command(python, "-c", identityScript+"\nimport json; print(json.dumps({'instanceID':instance_id(),'privateAddress':private_address()}))").CombinedOutput()
	if err != nil {
		t.Fatalf("address probe: %v: %s", err, out)
	}
	var observed map[string]string
	if json.Unmarshal(out, &observed) != nil || observed["instanceID"]+"\n" != first || observed["privateAddress"] == "" {
		t.Fatalf("inconsistent launch probe: %s", out)
	}
}
