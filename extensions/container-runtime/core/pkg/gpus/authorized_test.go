package gpus

import (
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAuthorizedUsesExplicitAllowlistAndFreshDiscovery(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name string
		node *ext.ContainerNode
		want []string
		ok   bool
	}{
		{name: "explicit allowlist", node: &ext.ContainerNode{Spec: ext.ContainerNodeSpec{AuthorizedGPUUUIDs: []string{"GPU-b", "GPU-a"}}}, want: []string{"GPU-a", "GPU-b"}, ok: true},
		{name: "fresh discovery", node: &ext.ContainerNode{Status: ext.ContainerNodeStatus{LastHeartbeatTime: timePtr(now.Add(-time.Second)), DiscoveredGPUUUIDs: []string{"GPU-a"}}}, want: []string{"GPU-a"}, ok: true},
		{name: "stale discovery", node: &ext.ContainerNode{Status: ext.ContainerNodeStatus{LastHeartbeatTime: timePtr(now.Add(-heartbeatFreshness - time.Second)), DiscoveredGPUUUIDs: []string{"GPU-a"}}}},
		{name: "future discovery", node: &ext.ContainerNode{Status: ext.ContainerNodeStatus{LastHeartbeatTime: timePtr(now.Add(time.Second)), DiscoveredGPUUUIDs: []string{"GPU-a"}}}},
		{name: "missing discovery", node: &ext.ContainerNode{Status: ext.ContainerNodeStatus{LastHeartbeatTime: timePtr(now)}}},
		{name: "empty explicit allowlist", node: &ext.ContainerNode{Spec: ext.ContainerNodeSpec{AuthorizedGPUUUIDs: []string{}}, Status: ext.ContainerNodeStatus{LastHeartbeatTime: timePtr(now), DiscoveredGPUUUIDs: []string{}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.node != nil && len(tt.node.Spec.AuthorizedGPUUUIDs)==0 {
				tt.node.Annotations=map[string]string{ext.AgentCredentialAnnotation:"credential"}
				tt.node.Status.AgentCredentialID="credential"
				tt.node.Status.Conditions=[]metav1.Condition{{Type:"Ready",Status:metav1.ConditionTrue}}
			}
			got, err := Authorized(tt.node, now)
			if tt.ok && err != nil {
				t.Fatal(err)
			}
			if !tt.ok && err == nil {
				t.Fatal("expected fail-closed result")
			}
			if tt.ok && (len(got) != len(tt.want) || got[0] != tt.want[0]) {
				t.Fatalf("Authorized() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDiscoveryFencesCredentialsAndGeneration(t *testing.T) {
	now:=time.Now()
	n:=&ext.ContainerNode{ObjectMeta:metav1.ObjectMeta{Generation:2,Annotations:map[string]string{ext.AgentCredentialAnnotation:"new"}},Status:ext.ContainerNodeStatus{ObservedGeneration:2,AgentCredentialID:"new",LastHeartbeatTime:timePtr(now),DiscoveredGPUUUIDs:[]string{"GPU-a"},Conditions:[]metav1.Condition{{Type:"Ready",Status:metav1.ConditionTrue,ObservedGeneration:2}}}}
	if _,err:=Authorized(n,now);err!=nil {t.Fatal(err)}
	for _,change:=range []func(*ext.ContainerNode){func(n *ext.ContainerNode){n.Status.AgentCredentialID="old"},func(n *ext.ContainerNode){n.Status.ObservedGeneration=1},func(n *ext.ContainerNode){n.Status.Conditions=nil},func(n *ext.ContainerNode){n.Status.Conditions[0].ObservedGeneration=1}} {
		copy:=n.DeepCopy();change(copy);if _,err:=Authorized(copy,now);err==nil {t.Fatal("stale or unauthenticated discovery accepted")}
	}
}

func TestAuthorizedRejectsInvalidOrDuplicateGPUUUIDs(t *testing.T) {
	now := time.Now()
	for _, values := range [][]string{{"MIG-1"}, {"GPU-a", "GPU-a"}, {"GPU-a,b"}, {"GPU-a GPU-b"}} {
		node := &ext.ContainerNode{Spec: ext.ContainerNodeSpec{AuthorizedGPUUUIDs: values}}
		if _, err := Authorized(node, now); err == nil {
			t.Fatalf("Authorized(%v) succeeded", values)
		}
	}
}

func timePtr(value time.Time) *metav1.Time {
	result := metav1.NewTime(value)
	return &result
}
