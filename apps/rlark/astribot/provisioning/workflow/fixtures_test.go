package workflow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const authSubjectKey = "actor"

func testHTTP() HTTP {
	return HTTP{
		RespondError: func(c *gin.Context, status int, code, message string) {
			c.AbortWithStatusJSON(status, gin.H{"code": code, "message": message})
		},
		RespondKubeError: func(c *gin.Context, message string, err error) {
			status := http.StatusBadGateway
			switch {
			case apierrors.IsNotFound(err):
				status = http.StatusNotFound
			case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
				status = http.StatusConflict
			}
			c.AbortWithStatusJSON(status, gin.H{"message": message})
		},
		DecodeOneJSON: func(c *gin.Context, limit int64, into any, emptyAllowed bool) bool {
			decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
			decoder.DisallowUnknownFields()
			err := decoder.Decode(into)
			if err == io.EOF && emptyAllowed {
				return true
			}
			var extra any
			if err != nil || decoder.Decode(&extra) != io.EOF {
				c.AbortWithStatus(http.StatusBadRequest)
				return false
			}
			return true
		},
		Actor: func(c *gin.Context) string { return c.GetString(authSubjectKey) },
		StationNamespace: func(c *gin.Context, id string) (string, error) {
			if id == "cluster-a" {
				return "agent-a", nil
			}
			c.AbortWithStatus(http.StatusNotFound)
			return "", apierrors.NewBadRequest("unknown fixture cluster")
		},
		CodeInvalidRequestBody:    "INVALID_REQUEST_BODY",
		CodeInvalidRequest:        "INVALID_REQUEST",
		CodeResourceConflict:      "RESOURCE_CONFLICT",
		CodeResourceAlreadyExists: "RESOURCE_ALREADY_EXISTS",
		CodeNotConfigured:         "NOT_CONFIGURED",
	}
}

type fakeStationBootstrap struct{ paths []string }

func (f *fakeStationBootstrap) Address() string { return "10.11.14.128:18443" }

func (f *fakeStationBootstrap) TunnelAddress() string { return "10.254.240.1:18443" }

func (f *fakeStationBootstrap) Digest(_ context.Context, path string) (string, error) {
	f.paths = append(f.paths, path)
	if path == "/v1/bootstrap/ca" {
		return "cadigest", nil
	}
	return "uplinkdigest", nil
}

func (f *fakeStationBootstrap) Open(_ context.Context, path string) (io.ReadCloser, int64, error) {
	f.paths = append(f.paths, path)
	return io.NopCloser(strings.NewReader(fakeInstaller)), int64(len(fakeInstaller)), nil
}

func (f *fakeStationBootstrap) BundleDigest(context.Context) (string, error) {
	return sha256Hex(fakeInstaller), nil
}

// fakeInstaller is what the fake Station API serves as the installer.
const fakeInstaller = "installer bytes"
