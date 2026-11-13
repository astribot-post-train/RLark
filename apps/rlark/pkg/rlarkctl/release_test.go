package rlarkctl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rlinf/rlark/apps/rlark/pkg/release"
)

func TestReleasePlan(t *testing.T) {
	var out bytes.Buffer
	cmd := ReleaseCommand()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"plan"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"1. prewarm\n",
		"4. controller  (after crds-rbac)\n",
		"     gate     manual: every gateway already understands the documents this controller writes\n",
		"     check    documents-unchanged\n",
		"9. console  (after gateway-api)\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("plan lacks %q", line)
		}
	}
}

func TestVerifyConditions(t *testing.T) {
	baseline := release.Snapshot{
		Clusters: map[string]string{"cloud": "Online", "office": "Downgraded"},
		Nodes:    map[string]string{"office/s1u-01": "Online", "office/desk-2": "Offline"},
	}
	current := release.Snapshot{
		Clusters: map[string]string{"cloud": "Online", "office": "Downgraded"},
		Nodes:    map[string]string{"office/s1u-01": "Offline", "office/desk-2": "Offline"},
	}
	conditions := []release.Condition{{Check: "clusters-online"}, {Check: "nodes-ready"}, {Manual: "the registry Pod is back"}}

	var out bytes.Buffer
	err := verifyConditions(&out, conditions, baseline, current, release.Allow{})
	want := "ok      clusters-online       none worse than before: cloud Online, office Downgraded\n" +
		"FAILED  nodes-ready           node office/s1u-01 was Online, is Offline\n" +
		"manual  the registry Pod is back\n"
	if out.String() != want || err == nil || !strings.HasPrefix(err.Error(), "1 checks failed") {
		t.Fatalf("err %v\n%s", err, out.String())
	}

	out.Reset()
	if err := verifyConditions(&out, conditions, baseline, baseline, release.Allow{}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
}
