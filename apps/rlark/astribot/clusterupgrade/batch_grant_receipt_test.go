package clusterupgrade

import (
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestCorruptGrantNeedsExpiredReceiptFromAnotherAttempt(t *testing.T) {
	now := time.Now()
	object := &unstructured.Unstructured{}
	object.SetUID("original")
	for _, test := range []struct {
		name           string
		cluster, child types.UID
		until          time.Time
		want           bool
	}{
		{"expired", "original", "before", now.Add(-time.Minute), true},
		{"same attempt", "original", "next", now.Add(-time.Minute), false},
		{"still active", "original", "before", now.Add(time.Minute), false},
		{"replacement", "different", "before", now.Add(-time.Minute), false},
		{"missing receipt", "", "", time.Time{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt, err := json.Marshal(grantReceipt{ClusterUID: test.cluster, ChildUID: test.child, Expires: test.until})
			if err != nil {
				t.Fatal(err)
			}
			object.SetAnnotations(map[string]string{NodeImageGrantAnnotation: "damaged", nodeImageGrantReceipt: string(receipt)})
			if got := mayIssueGrant(object, ExecutorIdentity{ChildUID: "next"}, now); got != test.want {
				t.Fatalf("may issue = %t, want %t", got, test.want)
			}
		})
	}
}
