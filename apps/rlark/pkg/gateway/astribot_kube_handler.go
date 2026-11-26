package gateway

import (
	"github.com/rlinf/rlark/apps/rlark/astribot/stations"
	"net/http"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rlarkiov1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

// A Node's robot power, its robot claim and its display name are set through their own endpoints
// (the Astribot station routes and node-display-name handler), which
// check and audit them. The generic Node update and patch may not change them; each answers the request when it
// refuses.

// nodeUpdateAllowed: a Node's new object keeps the stored power, claim and display name.
func nodeUpdateAllowed(c *gin.Context, a *resourceAccessor, namespace, name string, body []byte) bool {
	existing, err := a.get(c.Request.Context(), namespace, name, metav1.GetOptions{})
	if err != nil {
		respondKubeError(c, err.Error(), err)
		return false
	}
	if reason := stations.ControlUpdateConflict(existing.(metav1.Object), body); reason != "" {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, reason)
		return false
	}
	if value, _ := displayNameIn(body); value != existing.(*rlarkiov1alpha1.Node).Spec.DisplayName {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, nodeDisplayNameRefused)
		return false
	}
	return true
}

// nodePatchAllowed: a Node's patch names neither power nor claim, and names the display name only as it is.
func nodePatchAllowed(c *gin.Context, a *resourceAccessor, namespace, name string, body []byte) bool {
	if reason := stations.ControlPatchConflict(body); reason != "" {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, reason)
		return false
	}
	value, named := displayNameIn(body)
	if !named {
		return true
	}
	existing, err := a.get(c.Request.Context(), namespace, name, metav1.GetOptions{})
	if err != nil {
		respondKubeError(c, err.Error(), err)
		return false
	}
	if value != existing.(*rlarkiov1alpha1.Node).Spec.DisplayName {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, nodeDisplayNameRefused)
		return false
	}
	return true
}
