package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestRawNodeStatusOwnershipAndAuthorization(t *testing.T) {
	for _, mode := range []string{"valid", "stale", "no-version", "phase", "reservation", "heartbeat", "metadata", "gpu", "environment", "ip", "negative", "capacity", "unknown", "peer", "active-change"} {
		t.Run(mode, func(t *testing.T) {
			f := newRawFixture(t)
			ctx := context.Background()
			nodes := f.server.rlarkClient.RlinfV1alpha1().Nodes(f.identity.Namespace)
			node, err := nodes.Get(ctx, f.identity.Node, metav1.GetOptions{})
			require.NoError(t, err)
			capability := &api.RawNodeStatus{Incarnation: f.identity.Incarnation, TrainingIP: "10.0.0.1", NetworkScope: "vpc", Environment: "prepared", AuthorizedGPUUUIDs: []string{"GPU-authorized"}}
			authorization, err := json.Marshal(capability)
			require.NoError(t, err)
			node.Annotations[rawnode.CapabilityAnnotation] = string(authorization)
			if mode != "active-change" {
				node.Status.Raw = capability
			}
			_, err = nodes.Update(ctx, node, metav1.UpdateOptions{})
			require.NoError(t, err)
			status := map[string]any{"raw": capability, "capacity": map[string]string{"nvidia.com/gpu": "1"}, "allocatable": map[string]string{"nvidia.com/gpu": "1"}}
			metadata := map[string]any{"resourceVersion": node.ResourceVersion}
			patch := map[string]any{"metadata": metadata, "status": status}
			switch mode {
			case "stale":
				metadata["resourceVersion"] = "old"
			case "no-version":
				delete(metadata, "resourceVersion")
			case "phase":
				status["phase"] = "Online"
			case "reservation":
				status["reservation"] = nil
			case "heartbeat":
				status["lastHeartbeatTime"] = "2026-01-01T00:00:00Z"
			case "metadata":
				metadata["annotations"] = map[string]string{"injected": "true"}
			case "gpu":
				copy := *capability
				copy.AuthorizedGPUUUIDs = []string{"GPU-other"}
				status["raw"] = &copy
			case "environment":
				copy := *capability
				copy.Environment = "other"
				status["raw"] = &copy
			case "ip":
				copy := *capability
				copy.TrainingIP = "8.8.8.8"
				status["raw"] = &copy
			case "negative":
				status["used"] = map[string]string{"nvidia.com/gpu": "-1"}
			case "capacity":
				status["capacity"] = map[string]string{"nvidia.com/gpu": "2"}
			case "unknown":
				status["unexpected"] = true
			}
			path := rawAPIPath("/namespaces/agents/nodes/node-a/status")
			if mode == "peer" {
				path = rawAPIPath("/namespaces/agents/nodes/node-b/status")
			}
			body, err := json.Marshal(patch)
			require.NoError(t, err)
			response := f.ownRequest(t, http.MethodPatch, path, string(body), string(types.MergePatchType))
			if mode == "valid" {
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				updated, err := nodes.Get(ctx, f.identity.Node, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, node.Status.Reservation, updated.Status.Reservation)
				require.Equal(t, node.Status.LastHeartbeatTime, updated.Status.LastHeartbeatTime)
				require.Equal(t, capability, updated.Status.Raw)
				gpu := updated.Status.Capacity[corev1.ResourceName("nvidia.com/gpu")]
				require.Equal(t, 0, gpu.Cmp(resource.MustParse("1")))
			} else if mode == "stale" || mode == "no-version" {
				require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
			} else {
				require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			}
		})
	}
}
