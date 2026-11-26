package gateway

// What this site's storages add to the official storage handlers (storage_handler.go): the fields of the three
// storage types, and the functions the official flow calls at the places it has to branch.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/astribot/storageconfig"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
)

// borrowStorageCredentials gives a new storage the provider, endpoint, region
// and access key of the storage its credentials_from names.
func (g *Gateway) borrowStorageCredentials(c *gin.Context, req *CreateStorageClassRequest) bool {
	if req.CredentialsFrom == "" {
		return true
	}
	secret, err := storageconfig.ClassSecretByName(c.Request.Context(), g.rawClient, g.managementNamespace(), req.CredentialsFrom)
	if err != nil {
		respondKubeError(c, fmt.Sprintf("get storage class %q: %v", req.CredentialsFrom, err), err)
		return false
	}
	_, source, err := storageconfig.ClassFromSecret(secret)
	if err != nil {
		respondKubeError(c, fmt.Sprintf("decode storage class replication: %v", err), err)
		return false
	}
	req.CopyCredentials(source)
	return true
}

// storageNameFree answers 409 when a storage already has the name: volumes
// name a storage by its name.
func (g *Gateway) storageNameFree(c *gin.Context, name string) bool {
	_, err := storageconfig.ClassSecretByName(c.Request.Context(), g.rawClient, g.managementNamespace(), name)
	if err == nil {
		respondError(c, http.StatusConflict, CodeResourceAlreadyExists, fmt.Sprintf("storage %q already exists", name))
		return false
	}
	if !errors.IsNotFound(err) {
		respondKubeError(c, fmt.Sprintf("get storage class replication: %v", err), err)
		return false
	}
	return true
}

// prepareStorageUpdate reads the declaration an update replaces, once, in
// place of the official name check and access-key lookup: the type cannot
// change; a different name in the body renames the storage (it keeps its id,
// so its StorageClasses keep their names, volumes name it by the new name, and
// Pods that already run keep their mounts); what the body omits keeps its
// stored value. It returns the declaration as it was.
func (g *Gateway) prepareStorageUpdate(c *gin.Context, name string, req *CreateStorageClassRequest) (*v1.Secret, bool) {
	secret, err := storageconfig.ClassSecretByName(c.Request.Context(), g.rawClient, g.managementNamespace(), name)
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, CodeResourceNotFound, "storage class not found")
			return nil, false
		}
		respondKubeError(c, fmt.Sprintf("get storage class replication: %v", err), err)
		return nil, false
	}
	_, existing, err := storageconfig.ClassFromSecret(secret)
	if err != nil {
		respondKubeError(c, fmt.Sprintf("decode storage class replication: %v", err), err)
		return nil, false
	}
	if err := req.ApplyUpdateDefaults(existing); err != nil {
		respondError(c, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return nil, false
	}
	if renamed := strings.TrimSpace(req.Name); renamed != name && !g.storageNameFree(c, renamed) {
		return nil, false
	}
	return secret, true
}

// storageDeclarationWritten follows a created, updated or deleted (stored is
// nil) primary declaration: the object clients cached under names are dropped
// and the storage's endpoint declarations are made the rendered ones, owned as
// identity is.
func (g *Gateway) storageDeclarationWritten(c *gin.Context, identity *v1.Secret, stored *storedStorageClass, names ...string) bool {
	g.storageClientsMu.Lock()
	for _, name := range names {
		delete(g.storageClients, name)
	}
	g.storageClientsMu.Unlock()
	id, _ := storageconfig.ParseClassID(identity.Name)
	if err := storageconfig.SyncEndpointSecrets(c.Request.Context(), g.rawClient, g.managementNamespace(), id, stored, identity); err != nil {
		respondKubeError(c, err.Error(), err)
		return false
	}
	return true
}
