package enrollment

import (
	"os/exec"
	"runtime"
	"testing"
)

func TestServiceManagerRecovery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process receipts")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required")
	}
	if out, err := exec.Command(python, "-B", "services_test.py").CombinedOutput(); err != nil {
		t.Fatalf("service recovery: %v\n%s", err, out)
	}
}
