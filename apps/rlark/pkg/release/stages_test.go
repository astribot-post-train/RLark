package release

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadStagesOrdersByRequires(t *testing.T) {
	stages, err := LoadStages([]byte(`
stages:
  - name: c
    requires: [b]
  - name: a
    check: [links-connected, {manual: someone looked}]
  - name: b
    requires: [a]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := stages[0].Name + stages[1].Name + stages[2].Name; got != "abc" {
		t.Fatalf("order %s", got)
	}
	if want := []Condition{{Check: "links-connected"}, {Manual: "someone looked"}}; !slices.Equal(stages[0].Check, want) {
		t.Fatalf("conditions %+v", stages[0].Check)
	}
}

func TestLoadStagesRefuses(t *testing.T) {
	for name, c := range map[string]struct{ stages, reason string }{
		"a cycle":             {"[{name: a, requires: [b]}, {name: b, requires: [a]}, {name: c}]", "cycle: a, b"},
		"an unknown requires": {"[{name: a, requires: [z]}]", "requires z"},
		"an unknown check":    {"[{name: a, check: [all-gateways-current]}]", `"all-gateways-current"`},
		"an unknown gate":     {"[{name: a, gate: [all-gateways-current]}]", `"all-gateways-current"`},
		"an empty manual":     {"[{name: a, gate: [{manual: ''}]}]", "neither a check nor manual"},
		"a name used twice":   {"[{name: a}, {name: a}]", "used twice"},
		"an unknown field":    {"[{name: a, rollout: hubs}]", "rollout"},
	} {
		if _, err := LoadStages([]byte("stages: " + c.stages)); err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestShippedStages(t *testing.T) {
	stages, err := LoadStages(StagesYAML)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	for i, stage := range stages {
		at[stage.Name] = i
	}
	// The constraints the order exists for.
	for _, pair := range [][2]string{
		{"prewarm", "crds-rbac"}, {"crds-rbac", "station-api"}, {"crds-rbac", "controller"},
		{"prewarm", "cluster-agents"}, {"station-agent", "gateway"}, {"cluster-agents", "gateway-api"}, {"gateway-api", "console"},
	} {
		if at[pair[0]] >= at[pair[1]] {
			t.Errorf("%s is not before %s", pair[0], pair[1])
		}
	}
	if len(stages) != 9 {
		t.Errorf("%d stages", len(stages))
	}
}
