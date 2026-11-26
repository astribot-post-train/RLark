package gateway

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/workflow"
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (g *Gateway) registerRobotModelRoutes(admin *gin.RouterGroup) {
	routes := stations.RobotModelRoutes{
		Station: g.stationClient, NodeClasses: nodeClassGVR, Nodes: siteNodeGVR,
		Enrollments: networkEnrollmentGVR, Machines: machineGVR,
		Admissions: nodeAdmissionGVR, MachineNamespace: machineNamespace,
	}
	if g.kubeClient != nil {
		routes.NodeJoinModels = func(ctx context.Context) ([]string, error) {
			joins, err := g.kubeClient.RlinfV1alpha1().NodeJoins().List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			models := make([]string, 0, len(joins.Items))
			for _, join := range joins.Items {
				models = append(models, join.Annotations[workflow.NodeJoinModelAnnotation])
			}
			return models, nil
		}
	}
	routes.Register(admin)
}
