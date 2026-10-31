// Package taskcontract provides shared comparisons for API-persisted Tasks.
package taskcontract

import (
	"bytes"
	"encoding/json"
	"reflect"

	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

// EqualSpec compares the API meaning of two specs, including opaque JSON.
// Object key order and omitted empty fields do not change API meaning. Numbers
// retain their exact representation rather than rounding through float64.
func EqualSpec(a, b native.TaskSpec) bool {
	decode := func(spec native.TaskSpec) (any, error) {
		data, err := json.Marshal(spec)
		if err != nil {
			return nil, err
		}
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		var value any
		err = d.Decode(&value)
		return value, err
	}
	x, err := decode(a)
	if err != nil {
		return false
	}
	y, err := decode(b)
	return err == nil && reflect.DeepEqual(x, y)
}
