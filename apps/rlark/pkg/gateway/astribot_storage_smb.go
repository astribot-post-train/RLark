package gateway

import (
	stderrors "errors"
	"github.com/gin-gonic/gin"
	"net/http"
)

// errStorageBrowsingUnsupported is returned for storage types without an
// object API in the gateway (SMB is only mounted by the CSI driver).
var errStorageBrowsingUnsupported = stderrors.New("storage class type does not support file browsing")

func respondStorageClientError(c *gin.Context, err error) {
	if stderrors.Is(err, errStorageBrowsingUnsupported) {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	respondError(c, http.StatusInternalServerError, CodeUpstreamUnavailable, err.Error())
}
