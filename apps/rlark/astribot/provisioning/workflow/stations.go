package workflow

import (
	"context"

	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const machineNamespace = "station-system"
const maxNodeAdmissionRequestBytes = 16 * 1024

var (
	nodeAdmissionGVR     = stationResource("nodeadmissions")
	nodeClassGVR         = stationResource("nodeclasses")
	siteNetworkGVR       = stationResource("sitenetworks")
	networkEnrollmentGVR = stationResource("networkenrollments")
	machineGVR           = stationResource("machines")
	nodeOperationGVR     = stationResource("nodeoperations")
)

func stationResource(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "station.rlark.io", Version: "v1alpha1", Resource: resource}
}

func (g *Service) admissionService() stationidentity.AdmissionService {
	return stationidentity.AdmissionService{
		SiteNetwork: func(ctx context.Context) (*unstructured.Unstructured, error) {
			return stationidentity.ReadSiteNetwork(ctx, g.stationClient)
		},
		Admissions: func(namespace string) dynamic.ResourceInterface {
			return g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace)
		},
		NodeClasses: g.stationClient.Resource(nodeClassGVR),
		Bootstrap:   g.stationBootstrap,
	}
}
