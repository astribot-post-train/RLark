package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// JSON string literals are Python-compatible for the absolute Unix paths used
// here. No shell evaluates these values: the Agent passes code as one argv item.
func checkpointWriteCode(path, name string) string {
	p, _ := json.Marshal(path) // String marshaling cannot fail.
	n, _ := json.Marshal(name)
	return fmt.Sprintf(`import json,os
_p=%s
with open(_p,"x",encoding="utf-8") as _f:
  json.dump({"case":%s,"marker":"RLARK_CHECKPOINT"},_f,sort_keys=True)
  _f.flush()
  os.fsync(_f.fileno())
print("RLARK_CHECKPOINT_READY",flush=True)`, p, n)
}

func checkpointVerifyCode(files map[string]string, remove bool) string {
	encoded, _ := json.Marshal(files) // String maps cannot fail to marshal.
	code := fmt.Sprintf(`import json,os
_expected=%s
for _case,_p in _expected.items():
  with open(_p,encoding="utf-8") as _f:
    assert json.load(_f)=={"case":_case,"marker":"RLARK_CHECKPOINT"}
`, encoded)
	if remove {
		code += "for _p in _expected.values():\n  os.unlink(_p)\n"
	}
	return code + `print("RLARK_CHECKPOINTS_VERIFIED",flush=True)`
}

func TestCheckpointAcceptanceDoesNotRecreateMissingOrOverwriteForeignFiles(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to validate cloud acceptance scripts locally")
	}
	run := func(code string, wantSuccess bool) {
		t.Helper()
		out, err := exec.Command(python, "-c", code).CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("checkpoint script success=%t want=%t: %v %s", err == nil, wantSuccess, err, out)
		}
	}
	first := filepath.Join(t.TempDir(), "checkpoint with 'quotes' 雪.json")
	second := filepath.Join(t.TempDir(), "checkpoint.json")
	files := map[string]string{"success": first, "stop": second}
	run(checkpointWriteCode(first, "success"), true)
	run(checkpointWriteCode(first, "success"), false)
	run(checkpointVerifyCode(files, true), false)
	if _, err := os.Stat(first); err != nil {
		t.Fatal("a later missing file must not delete a previously verified checkpoint", err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("verification recreated a missing checkpoint", err)
	}
	run(checkpointWriteCode(second, "wrong-case"), true)
	run(checkpointVerifyCode(files, true), false)
	if _, err := os.Stat(first); err != nil {
		t.Fatal("failed verification must preserve files", err)
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	run(checkpointWriteCode(second, "stop"), true)
	run(checkpointVerifyCode(files, false), true)
	for _, path := range files {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("read-only verification removed checkpoint", err)
		}
	}
	run(checkpointVerifyCode(files, true), true)
	for _, path := range files {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("successful final verifier did not remove its test-owned checkpoint", err)
		}
	}
}
