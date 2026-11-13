package release

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// StagesYAML is the order of this build's batch. It is in the binary so that
// the checks a stage names are the ones this build has.
//
//go:embed stages.yaml
var StagesYAML []byte

// Stage is one step of a batch; stages.yaml describes the fields.
type Stage struct {
	Name     string      `json:"name"`
	Requires []string    `json:"requires,omitempty"`
	Changes  []string    `json:"changes,omitempty"`
	Gate     []Condition `json:"gate,omitempty"`
	Check    []Condition `json:"check,omitempty"`
	Note     string      `json:"note,omitempty"`
}

// Condition is a check of Checks by name, or what a person confirms while
// the plane cannot say it.
type Condition struct {
	Check  string
	Manual string
}

func (c *Condition) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &c.Check) == nil {
		return nil
	}
	var manual struct {
		Manual string `json:"manual"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manual); err != nil {
		return err
	}
	c.Manual = manual.Manual
	return nil
}

// LoadStages reads a stages file and returns its stages in an order that
// honours every requires, the file's own order wherever that already does.
func LoadStages(data []byte) ([]Stage, error) {
	var file struct {
		Stages []Stage `json:"stages"`
	}
	if err := yaml.UnmarshalStrict(data, &file); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, stage := range file.Stages {
		if stage.Name == "" || names[stage.Name] {
			return nil, fmt.Errorf("stage name %q is empty or used twice", stage.Name)
		}
		names[stage.Name] = true
	}
	for _, stage := range file.Stages {
		for _, required := range stage.Requires {
			if !names[required] {
				return nil, fmt.Errorf("stage %s requires %s, which is not a stage", stage.Name, required)
			}
		}
		for _, condition := range slices.Concat(stage.Gate, stage.Check) {
			if _, known := Checks[condition.Check]; !known && condition.Manual == "" {
				return nil, fmt.Errorf("stage %s names %q, which is neither a check nor manual", stage.Name, condition.Check)
			}
		}
	}

	ordered := make([]Stage, 0, len(file.Stages))
	placed := map[string]bool{}
	for len(ordered) < len(file.Stages) {
		before := len(ordered)
		for _, stage := range file.Stages {
			if !placed[stage.Name] && !slices.ContainsFunc(stage.Requires, func(required string) bool { return !placed[required] }) {
				placed[stage.Name] = true
				ordered = append(ordered, stage)
			}
		}
		if len(ordered) == before {
			var waiting []string
			for _, stage := range file.Stages {
				if !placed[stage.Name] {
					waiting = append(waiting, stage.Name)
				}
			}
			return nil, errors.New("stages require each other in a cycle: " + strings.Join(waiting, ", "))
		}
	}
	return ordered, nil
}
