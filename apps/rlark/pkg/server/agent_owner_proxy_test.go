package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	clientset "github.com/rlinf/rlark/api/kubeclients/clientset/versioned/fake"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/apis"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestAgentOwnerProxyRestrictsClusterAndJobReads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const agentID = "cluster-a"
	namespace := apis.RLarkAgentNamespacePrefix + agentID
	cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", UID: types.UID("cluster-uid")}, Spec: api.ClusterSpec{AgentNamespace: namespace}}
	otherCluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: types.UID("other-uid")}, Spec: api.ClusterSpec{AgentNamespace: "other-agent"}}
	job := &api.Job{ObjectMeta: metav1.ObjectMeta{Name: "train", UID: types.UID("job-uid")}}
	controller := true
	task := &api.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "train-head", Namespace: namespace, UID: types.UID("task-uid"),
		Annotations:     map[string]string{utils.ParentUIDAnnotation: string(job.UID)},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}},
	}}
	task.Spec.Runtime.Kind = api.RuntimeKind("Docker")
	task.Spec.Placement.DockerBinding = &api.DockerTaskBinding{TaskUID: string(task.UID), JobUID: string(job.UID), ClusterUID: string(cluster.UID)}
	foreign := task.DeepCopy()
	foreign.Name, foreign.Namespace, foreign.UID = "other-task", "other-agent", "foreign-task-uid"
	server := &Server{rlarkClient: clientset.NewSimpleClientset(cluster, otherCluster, job, task, foreign)}
	meta := map[string]string{apis.MetaCertRole: "agent", apis.MetaAgentID: agentID}

	call := func(method, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/api/kubernetes/"+path, nil)
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = request
		ctx.Params = gin.Params{{Key: "path", Value: "/" + path}}
		require.True(t, server.handleAgentOwnerRead(ctx, meta))
		return recorder
	}

	clusterResponse := call(http.MethodGet, "apis/rlinf.io/v1alpha1/clusters")
	require.Equal(t, http.StatusOK, clusterResponse.Code)
	var clusterList api.ClusterList
	require.NoError(t, json.Unmarshal(clusterResponse.Body.Bytes(), &clusterList))
	require.Len(t, clusterList.Items, 1)
	require.Equal(t, cluster.Name, clusterList.Items[0].Name)

	jobResponse := call(http.MethodGet, "apis/rlinf.io/v1alpha1/jobs/train")
	require.Equal(t, http.StatusOK, jobResponse.Code)
	var gotJob api.Job
	require.NoError(t, json.Unmarshal(jobResponse.Body.Bytes(), &gotJob))
	require.Equal(t, job.UID, gotJob.UID)

	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "unbound job", method: http.MethodGet, path: "apis/rlinf.io/v1alpha1/jobs/other"},
		{name: "cluster outside agent namespace", method: http.MethodGet, path: "apis/rlinf.io/v1alpha1/clusters/other"},
		{name: "mutation", method: http.MethodDelete, path: "apis/rlinf.io/v1alpha1/jobs/train"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := call(test.method, test.path)
			require.Equal(t, http.StatusForbidden, response.Code)
		})
	}
}
