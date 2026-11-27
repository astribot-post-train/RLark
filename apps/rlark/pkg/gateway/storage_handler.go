package gateway

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rlinf/rlark/apps/rlark/astribot/storageconfig"
	"github.com/rlinf/rlark/apps/rlark/pkg/gateway/storage"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	"k8s.io/apimachinery/pkg/api/errors"
)

// StorageClassData 定义StorageClass的响应数据结构.
type StorageClassData struct {
	Name             string   `json:"name"`
	StorageClassName string   `json:"storageClassName"`
	Clusters         []string `json:"clusters"`
	Description      string   `json:"description"`
	Bucket           string   `json:"bucket"`
	Provider         string   `json:"provider"`
	Endpoint         string   `json:"endpoint"`
	Region           string   `json:"region"`
	PathStyle        bool     `json:"pathStyle"`
	AccessKeyId      string   `json:"accessKeyId"`
	TenantID         string   `json:"tenantID"`
	CreatedBy        string   `json:"createdBy,omitempty"`
	CreatedAt        string   `json:"createdAt"`
	storageconfig.SpecialClassView
}

// Provider 定义存储提供商信息.
type Provider struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// listStorageClass reads StorageClass declarations from the management plane.
// The distribution controllers own delivery to the data plane, so listing does
// not need to contact cluster agents.
func (g *Gateway) listStorageClass(c *gin.Context) {
	secrets, err := storageconfig.ListClassSecrets(c.Request.Context(), g.rawClient, g.managementNamespace())
	if err != nil {
		respondKubeError(c, fmt.Sprintf("list storage class replications: %v", err), err)
		return
	}
	requestedClusters := parseStorageClassClusters(c.Query("clusters"))
	data := make(map[string]*StorageClassData, len(secrets))
	for i := range secrets {
		id, stored, err := storageconfig.ClassFromSecret(&secrets[i])
		if err != nil {
			log.FromContext(c.Request.Context()).Error(err, "invalid storage class replication", "secret", secrets[i].Name)
			respondError(c, http.StatusInternalServerError, CodeInternalError, "invalid storage class data")
			return
		}
		clusters := stored.Clusters
		if len(requestedClusters) != 0 {
			clusters = intersectStorageClassClusters(clusters, requestedClusters)
			if len(clusters) == 0 {
				continue
			}
		}
		view, err := storageconfig.DescribeSpecialClass(id, stored)
		if err != nil {
			respondError(c, http.StatusInternalServerError, CodeInternalError, "invalid storage mount policy")
			return
		}
		accessKeyID := ""
		if stored.Type == storageconfig.TypeS3 {
			accessKeyID = storageconfig.MaskAccessKeyID(stored.AccessKeyID)
		}
		data[stored.Name] = &StorageClassData{
			Name: stored.Name, Description: stored.Description, StorageClassName: id, Clusters: clusters,
			Provider: stored.Provider, Endpoint: stored.Endpoint, Bucket: stored.Bucket, Region: stored.Region, PathStyle: stored.PathStyle,
			AccessKeyId: accessKeyID, TenantID: tenant.IDOf(&secrets[i]), CreatedBy: secrets[i].Annotations[tenant.CreatedByAnnotation], CreatedAt: secrets[i].CreationTimestamp.UTC().Format(time.RFC3339),
			SpecialClassView: view,
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "success": true})
}

func parseStorageClassClusters(value string) map[string]struct{} {
	clusters := map[string]struct{}{}
	for _, cluster := range strings.Split(value, ",") {
		cluster = strings.TrimSpace(cluster)
		if cluster != "" {
			clusters[cluster] = struct{}{}
		}
	}
	return clusters
}

func intersectStorageClassClusters(clusters []string, requested map[string]struct{}) []string {
	filtered := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		if _, ok := requested[cluster]; ok {
			filtered = append(filtered, cluster)
		}
	}
	return filtered
}

// listProvider 列出支持的存储提供商. 目录由 storage.Providers 单一维护,
// 与创建/更新时的 provider 校验共用同一份数据, 避免客户端提交 rclone 无法
// 识别的取值.
func (g *Gateway) listProvider(c *gin.Context) {
	providers := make([]Provider, 0, len(storage.Providers))
	for _, provider := range storage.Providers {
		providers = append(providers, Provider{Name: provider.DisplayName, Value: provider.Value})
	}

	c.JSON(http.StatusOK, gin.H{
		"data":    providers,
		"success": true,
	})
}

const CodeStorageClassRequiredFields = "STORAGE_CLASS_REQUIRED_FIELDS"

type storedStorageClass = storageconfig.StoredClass

type CreateStorageClassRequest = storageconfig.ClassRequest

// createStorageClass stores a Replication declaration. The Replication and
// Delivery controllers apply the StorageClass bundle to selected clusters.
func (g *Gateway) createStorageClass(c *gin.Context) {
	var req CreateStorageClassRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, CodeInvalidRequestBody, err.Error())
		return
	}
	if !g.borrowStorageCredentials(c, &req) {
		return
	}
	stored, err := storageconfig.NormalizeClass(req)
	if err != nil {
		respondStorageClassValidationError(c, err)
		return
	}
	if err := validateStorageClassCredentials(c.Request.Context(), stored); err != nil {
		respondStorageClassCredentialValidationError(c, err)
		return
	}
	if !g.storageNameFree(c, stored.Name) {
		return
	}
	secret, err := storageconfig.CreateClass(c.Request.Context(), g.rawClient, g.managementNamespace(), stored, c.GetString(authSubjectKey))
	if err != nil {
		if stderrors.Is(err, storageconfig.ErrClassIDAllocation) {
			respondError(c, http.StatusInternalServerError, CodeInternalError, err.Error())
		} else {
			respondKubeError(c, fmt.Sprintf("create storage class replication: %v", err), err)
		}
		return
	}
	if !g.storageDeclarationWritten(c, secret, &stored) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// updateStorageClass updates the Replication declaration selected by the legacy
// name route while retaining its stable internal identity.
func (g *Gateway) updateStorageClass(c *gin.Context) {
	name := c.Param("name")
	var req CreateStorageClassRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, CodeInvalidRequestBody, err.Error())
		return
	}
	if req.Name == "" {
		req.Name = name
	}
	identity, ok := g.prepareStorageUpdate(c, name, &req)
	if !ok {
		return
	}
	stored, err := storageconfig.NormalizeClass(req)
	if err != nil {
		respondStorageClassValidationError(c, err)
		return
	}
	if err := validateStorageClassCredentials(c.Request.Context(), stored); err != nil {
		respondStorageClassCredentialValidationError(c, err)
		return
	}
	if err := storageconfig.UpdateClass(c.Request.Context(), g.rawClient, identity, stored); err != nil {
		if errors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, CodeResourceNotFound, "storage class not found")
			return
		}
		respondKubeError(c, fmt.Sprintf("update storage class replication: %v", err), err)
		return
	}
	g.invalidateStorageClients(name)
	if !g.storageDeclarationWritten(c, identity, &stored, name, stored.Name) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// deleteStorageClass deletes the Replication declaration selected by the legacy
// name route. Distribution finalizers clean up Delivery declarations and local objects.
func (g *Gateway) deleteStorageClass(c *gin.Context) {
	name := c.Param("name")
	secret, err := storageconfig.ClassSecretByName(c.Request.Context(), g.rawClient, g.managementNamespace(), name)
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, CodeResourceNotFound, "storage class not found")
			return
		}
		respondKubeError(c, fmt.Sprintf("get storage class replication: %v", err), err)
		return
	}
	if err := storageconfig.DeleteClass(c.Request.Context(), g.rawClient, secret); err != nil {
		if errors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, CodeResourceNotFound, "storage class not found")
			return
		}
		respondKubeError(c, fmt.Sprintf("delete storage class replication: %v", err), err)
		return
	}
	if !g.storageDeclarationWritten(c, secret, nil, name) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func respondStorageClassValidationError(c *gin.Context, err error) {
	var validationErr *storageconfig.RequiredFieldsError
	if stderrors.As(err, &validationErr) {
		respondErrorWithDetails(c, http.StatusBadRequest, CodeStorageClassRequiredFields, validationErr.Error(), map[string]any{"fields": validationErr.Fields})
		return
	}
	respondError(c, http.StatusBadRequest, CodeInvalidRequest, err.Error())
}

var validateStorageClassCredentials = storageconfig.ValidateClassCredentials

func respondStorageClassCredentialValidationError(c *gin.Context, err error) {
	log.FromContext(c.Request.Context()).Error(err, "storage credential validation failed")
	respondError(c, http.StatusBadRequest, CodeInvalidRequest, storageconfig.CredentialErrorMessage(err, storageconfig.SMBPort))
}

// --- Storage class file operation handlers ---

// getStorageClient resolves the storage.Client from the management-plane
// StorageClass declaration rather than reading data-plane resources.
func (g *Gateway) invalidateStorageClients(name string) {
	g.storageClientsMu.Lock()
	defer g.storageClientsMu.Unlock()
	for cacheKey := range g.storageClients {
		if strings.HasSuffix(cacheKey, "/"+name) {
			delete(g.storageClients, cacheKey)
		}
	}
}

func (g *Gateway) getStorageClient(ctx context.Context, name string) (*storage.Client, error) {
	cacheKey := name
	g.storageClientsMu.RLock()
	if client, ok := g.storageClients[cacheKey]; ok {
		g.storageClientsMu.RUnlock()
		return client, nil
	}
	g.storageClientsMu.RUnlock()

	secret, err := storageconfig.ClassSecretByName(ctx, g.rawClient, g.managementNamespace(), name)
	if err != nil {
		return nil, fmt.Errorf("get storage class declaration: %w", err)
	}
	_, stored, err := storageconfig.ClassFromSecret(secret)
	if err != nil {
		return nil, fmt.Errorf("decode storage class declaration: %w", err)
	}
	if stored.Type != storageconfig.TypeS3 {
		return nil, errStorageBrowsingUnsupported
	}
	config := storage.DefaultConfig()
	config.Provider = stored.Provider
	config.Endpoint = stored.Endpoint
	config.AccessKeyId = stored.AccessKeyID
	config.SecretAccessKey = stored.AccessKey
	config.Region = stored.Region
	config.UsePathStyle = stored.PathStyle
	config.Bucket = stored.Bucket
	client, err := storage.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("create storage client: %w", err)
	}
	g.storageClientsMu.Lock()
	g.storageClients[cacheKey] = client
	g.storageClientsMu.Unlock()
	return client, nil
}

// listStorageClassFiles handles GET /api/v1/storage/storageclass/:name/list
// with query parameters: prefix, delimiter, marker, maxKeys.
func (g *Gateway) listStorageClassFiles(c *gin.Context) {
	name := c.Param("name")

	client, err := g.getStorageClient(c.Request.Context(), name)
	if err != nil {
		respondStorageClientError(c, err)
		return
	}

	prefix := c.Query("prefix")
	delimiter := c.Query("delimiter")
	marker := c.Query("marker")
	maxKeysStr := c.Query("maxKeys")

	// Default to "/" so folders are grouped as common prefixes. Callers that
	// want a flat listing can explicitly pass an empty delimiter.
	if _, ok := c.GetQuery("delimiter"); !ok {
		delimiter = "/"
	}

	var maxKeys int
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil {
			maxKeys = mk
		}
	}

	options := &storage.ListOptions{
		Prefix:    prefix,
		MaxKeys:   maxKeys,
		Delimiter: delimiter,
		Marker:    marker,
	}

	result, err := client.ListObjectsWithDelimiter(options)
	if err != nil {
		respondError(c, http.StatusInternalServerError, CodeUpstreamUnavailable, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"objects":         result.Objects,
		"common_prefixes": result.CommonPrefixes,
		"is_truncated":    result.IsTruncated,
		"next_marker":     result.NextMarker,
		"max_keys":        result.MaxKeys,
	})
}

// uploadStorageClassFile handles POST /api/v1/storage/storageclass/:name/upload.
func (g *Gateway) uploadStorageClassFile(c *gin.Context) {
	name := c.Param("name")

	client, err := g.getStorageClient(c.Request.Context(), name)
	if err != nil {
		respondStorageClientError(c, err)
		return
	}

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		respondError(c, http.StatusBadRequest, CodeInvalidRequestBody, "No file uploaded")
		return
	}
	defer utils.CloseIO(file)

	objectKey, err := client.UploadFileFromMultipart(header)
	if err != nil {
		respondError(c, http.StatusInternalServerError, CodeUpstreamUnavailable, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": objectKey})
}

// getStorageClassObject handles GET /api/v1/storage/storageclass/:name/object/*key
// It returns a presigned URL for downloading the object.
func (g *Gateway) getStorageClassObject(c *gin.Context) {
	name := c.Param("name")
	// gin's *key catch-all includes the leading slash; strip it so the object
	// key doesn't start with "/", which would produce a "//" in the presigned
	// URL path. Browsers/HTTP clients normalize "//" to "/", breaking the
	// signature and causing SignatureDoesNotMatch from OSS.
	key := strings.TrimPrefix(c.Param("key"), "/")
	if key == "" {
		respondError(c, http.StatusBadRequest, CodeInvalidQueryParameter, "object key is required")
		return
	}

	client, err := g.getStorageClient(c.Request.Context(), name)
	if err != nil {
		respondStorageClientError(c, err)
		return
	}

	expireStr := c.Query("expire")
	var expireSeconds int64 = 3600
	if expireStr != "" {
		if exp, err := strconv.ParseInt(expireStr, 10, 64); err == nil {
			expireSeconds = exp
		}
	}

	url, err := client.GetObjectURL(key, expireSeconds)
	if err != nil {
		respondError(c, http.StatusInternalServerError, CodeUpstreamUnavailable, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"url": url})
}

// deleteStorageClassObject handles DELETE /api/v1/storage/storageclass/:name/object/*key.
func (g *Gateway) deleteStorageClassObject(c *gin.Context) {
	name := c.Param("name")
	key := strings.TrimPrefix(c.Param("key"), "/")
	if key == "" {
		respondError(c, http.StatusBadRequest, CodeInvalidQueryParameter, "object key is required")
		return
	}

	client, err := g.getStorageClient(c.Request.Context(), name)
	if err != nil {
		respondStorageClientError(c, err)
		return
	}

	err = client.DeleteObject(key)
	if err != nil {
		respondError(c, http.StatusInternalServerError, CodeUpstreamUnavailable, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Object deleted successfully"})
}
