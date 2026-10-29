package sync

import (
	"errors"
	"testing"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestIsOrphan(t *testing.T) {
	obj := &rlarkv1alpha1.Job{ObjectMeta: metav1.ObjectMeta{UID: types.UID("current")}}
	for _, tc := range []struct {
		name      string
		uid       string
		err       error
		orphan    bool
		wantError bool
	}{
		{"same UID", "current", nil, false, false},
		{"recreated", "old", nil, true, false},
		{"not found", "old", apierrors.NewNotFound(schema.GroupResource{Resource: "jobs"}, "job"), true, false},
		{"API unavailable", "old", errors.New("unavailable"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := isOrphan(tc.uid, obj, tc.err)
			if got != tc.orphan || (err != nil) != tc.wantError {
				t.Fatalf("got orphan=%v error=%v", got, err)
			}
		})
	}
}
