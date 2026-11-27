package gateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
)

const (
	roleOperator       = "operator"
	CodeRobotClaimBusy = "ROBOT_CLAIM_BUSY"
)

func isOperator(c *gin.Context) bool {
	role := c.GetString(authRoleKey)
	return role == roleAdmin || role == roleOperator
}

func requireOperator() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isOperator(c) {
			respondError(c, http.StatusForbidden, CodeForbidden, "administrator or operator access required")
			return
		}
		c.Next()
	}
}

func (g *Gateway) registerRobotRoutes(nodes gin.IRouter) {
	g.robotControlRoutes().Register(nodes, requireOperator())
	g.registerStationCameraRoutes(nodes)
}

func (g *Gateway) robotControlRoutes() stations.ControlRoutes {
	nodes := g.accessors["nodes"]
	return stations.ControlRoutes{
		GetNode: func(ctx context.Context, ns, name string) (*rlarkv1alpha1.Node, error) {
			result, err := nodes.get(ctx, ns, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return result.(*rlarkv1alpha1.Node), nil
		},
		ListNodes: func(ctx context.Context, ns string, opts metav1.ListOptions) (*rlarkv1alpha1.NodeList, error) {
			result, err := nodes.list(ctx, ns, opts)
			if err != nil {
				return nil, err
			}
			return result.(*rlarkv1alpha1.NodeList), nil
		},
		PatchNode: func(ctx context.Context, ns, name string, patch []byte) (*rlarkv1alpha1.Node, error) {
			result, err := nodes.patch(ctx, ns, name, patch)
			if result == nil || err != nil {
				return nil, err
			}
			return result.(*rlarkv1alpha1.Node), nil
		},
		Admissions: func(ns string) dynamic.ResourceInterface {
			return g.stationClient.Resource(nodeAdmissionGVR).Namespace(ns)
		},
		AuthorizeNode: func(c *gin.Context, ns, name string) bool {
			return g.authorizeTenantObjectByName(c, "nodes", ns, name)
		},
		Actor: func(c *gin.Context) string {
			return c.GetString(authSubjectKey)
		},
		IsOperator:              isOperator,
		RespondError:            respondStationError,
		RespondErrorWithDetails: respondStationErrorWithDetails,
		RespondKubeError:        respondKubeError,
		RespondWriteError:       respondCreateError,
		ForwardPose:             g.forwardRobotPose,
	}
}

func (g *Gateway) forwardRobotPose(ctx context.Context, agent, node, actor string, body []byte) (*http.Response, error) {
	address := fmt.Sprintf("%s/api/nodeproxy/%s/%s/api/astribot/services/s1u-power/pose",
		strings.TrimSuffix(g.config.ServerAddress, "/"), url.PathEscape(agent), url.PathEscape(node))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Remote-User", actor)
	return (&http.Client{Transport: g.serverTransport, Timeout: 5 * time.Second}).Do(req)
}
