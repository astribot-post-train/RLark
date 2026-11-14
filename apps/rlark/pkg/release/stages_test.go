package release

import (
	"os"
	"regexp"
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
    gate: [{gateways-understand: 3}]
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
	if gate := stages[0].Gate[0]; gate != (Condition{Check: "gateways-understand", Level: 3}) || gate.String() != "gateways-understand: 3" {
		t.Fatalf("gate %+v", gate)
	}
}

func TestLoadStagesRefuses(t *testing.T) {
	for name, c := range map[string]struct{ stages, reason string }{
		"a cycle":                         {"[{name: a, requires: [b]}, {name: b, requires: [a]}, {name: c}]", "cycle: a, b"},
		"an unknown requires":             {"[{name: a, requires: [z]}]", "requires z"},
		"an unknown check":                {"[{name: a, check: [all-gateways-current]}]", `"all-gateways-current"`},
		"an unknown gate":                 {"[{name: a, gate: [all-gateways-current]}]", `"all-gateways-current"`},
		"an empty manual":                 {"[{name: a, gate: [{manual: ''}]}]", "nor manual"},
		"a level check without its level": {"[{name: a, gate: [gateways-understand]}]", `"gateways-understand"`},
		"a level of zero":                 {"[{name: a, gate: [{gateways-understand: 0}]}]", `"gateways-understand"`},
		"a level that is no number":       {"[{name: a, gate: [{gateways-understand: new}]}]", "int32"},
		"a level for a plain check":       {"[{name: a, check: [{nodes-ready: 2}]}]", `"nodes-ready"`},
		"two entries in one":              {"[{name: a, gate: [{gateways-understand: 1, manual: looked}]}]", "not one check"},
		"a name used twice":               {"[{name: a}, {name: a}]", "used twice"},
		"an unknown field":                {"[{name: a, rollout: hubs}]", "rollout"},
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

	// The controller's gate asks for the level of the document the
	// controller of this tree writes. Station is a module this one does not
	// import, so its constant is read from its source.
	source, err := os.ReadFile("../../../../extensions/station/network/compute.go")
	if err != nil {
		t.Fatal(err)
	}
	written := regexp.MustCompile(`(?m)^const DocumentLevel = (\d+)$`).FindSubmatch(source)
	if gate := stages[at["controller"]].Gate; written == nil || len(gate) != 1 || gate[0].String() != "gateways-understand: "+string(written[1]) {
		t.Errorf("the gate of stage controller is %v, the controller writes level %s", gate, written)
	}
}
