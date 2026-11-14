package rlarkctl

import (
	"bytes"
	"strings"
	"testing"
	"time"

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
		"     gate     gateways-understand: 1\n",
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
	conditions := []release.Condition{{Check: "clusters-online"}, {Check: "nodes-ready"}, {Manual: "the registry Pod is back"}, {Check: "gateways-understand", Level: 2}}
	baseline.Enrollments = map[string]release.Enrollment{"office/s1u-01": {Phase: "Enrolled", ConfigHash: "abc"}}
	baseline.Observations = map[string]release.Observation{"office/s1u-01": {GatewayLevel: 2}}
	current.Enrollments = baseline.Enrollments
	current.Observations = map[string]release.Observation{"office/s1u-01": {GatewayLevel: 1}}

	var out bytes.Buffer
	err := verifyConditions(&out, conditions, baseline, current, release.Allow{})
	want := "ok      clusters-online       none worse than before: cloud Online, office Downgraded\n" +
		"FAILED  nodes-ready           node office/s1u-01 was Online, is Offline\n" +
		"manual  the registry Pod is back\n" +
		"FAILED  gateways-understand: 2  office/s1u-01 gateway understands level 1\n"
	if out.String() != want || err == nil || !strings.HasPrefix(err.Error(), "2 checks failed") {
		t.Fatalf("err %v\n%s", err, out.String())
	}

	out.Reset()
	if err := verifyConditions(&out, conditions, baseline, baseline, release.Allow{}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestPrintVersions(t *testing.T) {
	taken := time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := printVersions(&out, release.Snapshot{TakenAt: taken, Observations: map[string]release.Observation{
		"office/s1u-01": {ReportedAt: taken.Add(-20 * time.Second), GatewayLevel: 2, GatewayDigest: strings.Repeat("ab", 32), AgentDigest: strings.Repeat("cd", 32), Pressures: []string{"DiskPressure", "PIDPressure"}},
		"cloud/gpu-1":   {ReportedAt: taken.Add(-5 * time.Second), GatewayLevel: 1, GatewayDigest: strings.Repeat("ef", 32), AgentDigest: strings.Repeat("cd", 32), Pressures: []string{}},
		// An old gateway and agent, in a report an old station-api stored.
		"office/desk-2": {ReportedAt: taken.Add(-9 * time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	want := "NODE           GATEWAY       LEVEL  AGENT         PRESSURES                 REPORTED\n" +
		"cloud/gpu-1    efefefefefef  1      cdcdcdcdcdcd  none                      5s ago\n" +
		"office/desk-2  -             -      -             ?                         9h0m0s ago\n" +
		"office/s1u-01  abababababab  2      cdcdcdcdcdcd  DiskPressure,PIDPressure  20s ago\n"
	if out.String() != want {
		t.Fatalf("\n%s", out.String())
	}
}
