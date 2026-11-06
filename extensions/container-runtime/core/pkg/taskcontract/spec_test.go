package taskcontract

import (
	"testing"

	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

func TestEqualSpecPreservesMeaningAndExactNumbers(t *testing.T) {
	var a, b native.TaskSpec
	a.Workload.Parameters.Raw = []byte(`{"argv":["/bin/true"],"n":9007199254740993}`)
	b.Workload.Parameters.Raw = []byte(`{ "n":9007199254740993, "argv":["/bin/true"] }`)
	if !EqualSpec(a, b) {
		t.Fatal("API key ordering changed meaning")
	}
	b.Workload.Parameters.Raw = []byte(`{"argv":["/bin/true"],"n":9007199254740992}`)
	if EqualSpec(a, b) {
		t.Fatal("different integer values rounded to equal")
	}
	b = *a.DeepCopy()
	b.Placement.Environment = "different"
	if EqualSpec(a, b) {
		t.Fatal("environment change ignored")
	}
	b = *a.DeepCopy()
	b.Workload.Parameters.Raw = []byte(`invalid`)
	if EqualSpec(a, b) {
		t.Fatal("invalid JSON accepted")
	}
}
