package docker

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestEmbeddedLauncherProcessBoundary(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for launcher process tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-m", "unittest", "discover", "-s", ".", "-p", "launcher_test.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher process suite: %v\n%s", err, output)
	}
}
