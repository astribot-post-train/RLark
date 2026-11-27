//go:build rejoin_integration

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	rlarkfake "github.com/rlinf/rlark/api/kubeclients/clientset/versioned/fake"
	stationv1alpha1 "github.com/rlinf/rlark/astribot/station/api/v1alpha1"
	"github.com/rlinf/rlark/astribot/station/controller"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	rejoinFixtureDisplayCluster = "深圳公司S1U调试集群"
	rejoinFixtureCluster        = "rlark-robot-s1u-01"
	rejoinFixtureNamespace      = "rlark-robot-s1u-01"
	rejoinFixtureMachine        = "machine-s1u-02"
	rejoinFixtureNode           = "s1u-02-x86"
	rejoinFixtureModel          = "s1u"
	rejoinFixtureSerial         = "fixture-serial-s1u-02"
	rejoinFixtureNodeUID        = "fixture-node-uid-s1u-02"
)

type loopbackClusterNamespaces struct{}

func (loopbackClusterNamespaces) Namespace(_ context.Context, clusterID string) (string, error) {
	if clusterID != rejoinFixtureCluster {
		return "", controller.ErrInvalidRegistration
	}
	return rejoinFixtureNamespace, nil
}

func (loopbackClusterNamespaces) ResolveNamespace(_ context.Context, namespace string) (controller.ClusterRegistration, error) {
	if namespace != rejoinFixtureNamespace {
		return controller.ClusterRegistration{}, controller.ErrInvalidRegistration
	}
	return controller.ClusterRegistration{ClusterID: rejoinFixtureCluster, Namespace: namespace}, nil
}

type loopbackMachineInspector struct{}

func (loopbackMachineInspector) Inspect(context.Context, string, controller.MachineAddress) (controller.MachineInspection, error) {
	return controller.MachineInspection{Serial: rejoinFixtureSerial, NodeHostKey: "SHA256:node"}, nil
}

type loopbackRejoinExecutor struct {
	mode  string
	delay time.Duration
}

func (e loopbackRejoinExecutor) Execute(ctx context.Context, _ *stationv1alpha1.NodeOperation, stage string) (bool, string, error) {
	if e.delay > 0 {
		timer := time.NewTimer(e.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return false, "", ctx.Err()
		}
	}
	if e.mode == "unreachable" && stage == "Preflight" {
		return false, "", fmt.Errorf("fixture robot: %w", controller.ErrSSHTargetUnreachable)
	}
	return true, rejoinFixtureNodeUID, nil
}

func TestRejoinLoopbackFixture(t *testing.T) {
	if os.Getenv("RLARK_REJOIN_LOOPBACK") != "1" {
		t.Skip("set RLARK_REJOIN_LOOPBACK=1 to run the isolated browser fixture")
	}
	mode := os.Getenv("RLARK_REJOIN_FIXTURE_MODE")
	if mode == "" {
		mode = "success"
	}
	if mode != "success" && mode != "unreachable" {
		t.Fatalf("RLARK_REJOIN_FIXTURE_MODE must be success or unreachable, got %q", mode)
	}
	delay := 2 * time.Second
	if value := os.Getenv("RLARK_REJOIN_STEP_DELAY"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed < 0 {
			t.Fatalf("invalid RLARK_REJOIN_STEP_DELAY %q", value)
		}
		delay = parsed
	}

	gin.SetMode(gin.ReleaseMode)
	stationClient, err := loopbackStationClient()
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{astribotState: astribotState{stationClient: stationClient}, kubeClient: rlarkfake.NewSimpleClientset(), jwtSigningKey: []byte(strings.Repeat("r", 32))}
	token, _, err := g.issueJWT("loopback-admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	g.RegisterRoutes(router)
	fixture := loopbackFixtureAuth{next: router, token: token}

	controllerClient, machineReconciler, operationReconciler, err := loopbackReconcilers(mode, delay)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go loopbackReconcile(ctx, stationClient, controllerClient, machineReconciler, operationReconciler)

	address := os.Getenv("RLARK_REJOIN_FIXTURE_ADDR")
	if address == "" {
		address = "127.0.0.1:18080"
	}
	if err := requireLoopbackAddress(address); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: fixture, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	fmt.Printf("\nIsolated real Gateway and Station reconciler fixture listening at http://%s\n", address)
	fmt.Printf("mode=%s step_delay=%s cluster_id=%s display_name=%s node=%s machine=%s model=%s\n", mode, delay, rejoinFixtureCluster, rejoinFixtureDisplayCluster, rejoinFixtureNode, rejoinFixtureMachine, rejoinFixtureModel)
	fmt.Println("Only fake Kubernetes records are used. Press Ctrl-C to stop the fixture.")
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("fixture HTTP server: %v", err)
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		t.Errorf("stop fixture HTTP server: %v", err)
	}
}

func requireLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("fixture address must include a host and port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("fixture must bind a loopback IP, got %q", host)
	}
	return nil
}

func loopbackStationClient() (*dynamicfake.FakeDynamicClient, error) {
	cluster := stationHandlerCluster(rejoinFixtureCluster, rejoinFixtureNamespace)
	_ = unstructured.SetNestedField(cluster.Object, rejoinFixtureDisplayCluster, "spec", "displayName")
	machine := handlerMachine(rejoinFixtureMachine, "Joined", "", rejoinFixtureSerial)
	_ = unstructured.SetNestedField(machine.Object, rejoinFixtureCluster, "status", "clusterID")
	_ = unstructured.SetNestedField(machine.Object, rejoinFixtureNode, "status", "nodeName")
	_ = unstructured.SetNestedField(machine.Object, rejoinFixtureModel, "spec", "admit", "model")
	_ = unstructured.SetNestedField(machine.Object, rejoinFixtureModel, "spec", "admit", "nodeClass")
	class := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeClass", "metadata": map[string]any{"name": rejoinFixtureModel},
		"spec": map[string]any{"kind": "Robot", "model": rejoinFixtureModel,
			"discovery": map[string]any{"hostKeyFingerprints": []any{"SHA256:entry"}},
			"naming":    map[string]any{"nodeName": "s1u-%02d-x86"},
			"access":    map[string]any{"entry": map[string]any{"port": int64(22), "user": "orin"}, "node": map[string]any{"address": "192.168.0.10", "port": int64(22), "user": "robot"}}},
	}}
	admission := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission", "metadata": map[string]any{"name": rejoinFixtureNode, "namespace": rejoinFixtureNamespace, "generation": int64(1)},
		"spec":   map[string]any{"clusterID": rejoinFixtureCluster, "serial": rejoinFixtureSerial, "nodeClass": rejoinFixtureModel, "publicKey": "Rpk1Y6lpJXIGOdNcgLMYs0J6mBG+67d85HVJs+upaR4=", "machine": map[string]any{"category": "Robot", "model": rejoinFixtureModel}},
		"status": map[string]any{"phase": "Joined", "observedGeneration": int64(1)},
	}}
	model := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "RobotModel", "metadata": map[string]any{"name": rejoinFixtureModel},
		"spec": map[string]any{"displayName": "S1U fixture"},
	}}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		stationClusterGVR: "ClusterList", machineGVR: "MachineList", nodeAdmissionGVR: "NodeAdmissionList", nodeClassGVR: "NodeClassList",
		robotModelGVR: "RobotModelList", nodeOperationGVR: "NodeOperationList",
	}, cluster, machine, admission, class, model), nil
}

func loopbackReconcilers(mode string, delay time.Duration) (client.Client, *controller.MachineReconciler, *controller.NodeOperationReconciler, error) {
	scheme := runtime.NewScheme()
	if err := stationv1alpha1.AddToScheme(scheme); err != nil {
		return nil, nil, nil, err
	}
	machine := &stationv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Name: rejoinFixtureMachine, Namespace: machineNamespace, Generation: 1},
		Spec: stationv1alpha1.MachineSpec{Site: "shenzhen", NodeClass: rejoinFixtureModel, Source: "Discovery", Admit: &stationv1alpha1.MachineAdmit{
			ClusterID: rejoinFixtureCluster, Type: stationv1alpha1.MachineTypeRobot, NodeClass: rejoinFixtureModel, Model: rejoinFixtureModel,
			RequestedBy: "fixture-admin", RequestedAt: metav1.Now(),
		}},
		Status: stationv1alpha1.MachineStatus{Phase: stationv1alpha1.MachineJoined, Serial: rejoinFixtureSerial,
			ClusterID: rejoinFixtureCluster, NodeName: rejoinFixtureNode, NodeHostKeyFingerprint: "SHA256:node",
			Entry: stationv1alpha1.MachineEntry{Address: "192.168.31.56", Port: 22, HostKeyFingerprint: "SHA256:entry", HostKeyAlgorithm: "ssh-ed25519"}},
	}
	class := &stationv1alpha1.NodeClass{ObjectMeta: metav1.ObjectMeta{Name: rejoinFixtureModel}, Spec: stationv1alpha1.NodeClassSpec{
		Kind: stationv1alpha1.NodeClassKindRobot, Model: rejoinFixtureModel,
		Naming: &stationv1alpha1.NodeClassNaming{NodeName: "s1u-%02d-x86"},
		Access: &stationv1alpha1.NodeClassAccess{Entry: stationv1alpha1.NodeClassEntry{Port: 22, User: "orin", Password: stationv1alpha1.SecretValue{Secret: "fixture", Key: "entry"}},
			Node: stationv1alpha1.NodeClassHost{Address: "192.168.0.10", Port: 22, User: "robot", Password: stationv1alpha1.SecretValue{Secret: "fixture", Key: "node"}}},
		Discovery: &stationv1alpha1.NodeClassDiscovery{HostKeyFingerprints: []string{"SHA256:entry"}},
	}}
	model := &stationv1alpha1.RobotModel{ObjectMeta: metav1.ObjectMeta{Name: rejoinFixtureModel}, Spec: stationv1alpha1.RobotModelSpec{DisplayName: "S1U fixture"}}
	admission := &stationv1alpha1.NodeAdmission{ObjectMeta: metav1.ObjectMeta{Name: rejoinFixtureNode, Namespace: rejoinFixtureNamespace},
		Spec: stationv1alpha1.NodeAdmissionSpec{ClusterID: rejoinFixtureCluster, Serial: rejoinFixtureSerial, NodeClass: rejoinFixtureModel,
			PublicKey: "Rpk1Y6lpJXIGOdNcgLMYs0J6mBG+67d85HVJs+upaR4=", Machine: &stationv1alpha1.NodeMachine{Category: stationv1alpha1.MachineCategoryRobot, Model: rejoinFixtureModel}},
		Status: stationv1alpha1.NodeAdmissionStatus{Phase: stationv1alpha1.NodeAdmissionJoined, ObservedGeneration: 1}}
	client := controllerfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(machine, &stationv1alpha1.NodeOperation{}).
		WithObjects(machine, class, model, admission).Build()
	namespaces := loopbackClusterNamespaces{}
	machineReconciler := &controller.MachineReconciler{Client: client, Reader: client, Inspector: loopbackMachineInspector{}, Clusters: namespaces, Namespace: machineNamespace}
	operationReconciler := controller.NewNodeOperationReconciler(client, client, namespaces, loopbackRejoinExecutor{mode: mode, delay: delay})
	return client, machineReconciler, operationReconciler, nil
}

func loopbackReconcile(ctx context.Context, stationClient *dynamicfake.FakeDynamicClient, client client.Client,
	machineReconciler *controller.MachineReconciler, operationReconciler *controller.NodeOperationReconciler) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	terminalRequestID := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			terminal, requestID := loopbackReconcileOnce(ctx, stationClient, client, machineReconciler, operationReconciler, terminalRequestID)
			if terminal {
				terminalRequestID = requestID
			}
		}
	}
}

func loopbackReconcileOnce(ctx context.Context, stationClient *dynamicfake.FakeDynamicClient, controllerClient client.Client,
	machineReconciler *controller.MachineReconciler, operationReconciler *controller.NodeOperationReconciler, terminalRequestID string) (bool, string) {
	dynamicMachine, err := stationClient.Resource(machineGVR).Namespace(machineNamespace).Get(ctx, rejoinFixtureMachine, metav1.GetOptions{})
	if err != nil {
		return false, ""
	}
	rejoinRequestID, _, _ := unstructured.NestedString(dynamicMachine.Object, "spec", "rejoinRequestID")
	if rejoinRequestID == "" {
		return false, ""
	}
	if rejoinRequestID == terminalRequestID {
		return true, terminalRequestID
	}
	typedMachine := &stationv1alpha1.Machine{}
	if err := controllerClient.Get(ctx, types.NamespacedName{Namespace: machineNamespace, Name: rejoinFixtureMachine}, typedMachine); err != nil {
		return false, ""
	}
	typedMachine.Spec.RejoinRequestID = rejoinRequestID
	typedMachine.Spec.RejoinRequestedBy, _, _ = unstructured.NestedString(dynamicMachine.Object, "spec", "rejoinRequestedBy")
	if err := controllerClient.Update(ctx, typedMachine); err != nil {
		return false, ""
	}
	key := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: machineNamespace, Name: rejoinFixtureMachine}}
	if _, err := machineReconciler.Reconcile(ctx, key); err != nil {
		return false, ""
	}
	if err := controllerClient.Get(ctx, types.NamespacedName{Namespace: machineNamespace, Name: rejoinFixtureMachine}, typedMachine); err == nil {
		syncMachineStatus(ctx, stationClient, typedMachine)
	}
	operations := &stationv1alpha1.NodeOperationList{}
	if err := controllerClient.List(ctx, operations, client.InNamespace(rejoinFixtureNamespace)); err != nil {
		return false, ""
	}
	for i := range operations.Items {
		operation := &operations.Items[i]
		syncOperation(ctx, stationClient, operation)
		if operation.Status.Phase != "Succeeded" && operation.Status.Phase != "Failed" && operation.Status.Phase != "Blocked" {
			_, _ = operationReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: operation.Namespace, Name: operation.Name}})
			if err := controllerClient.Get(ctx, types.NamespacedName{Namespace: operation.Namespace, Name: operation.Name}, operation); err == nil {
				syncOperation(ctx, stationClient, operation)
			}
		}
	}
	if err := controllerClient.Get(ctx, types.NamespacedName{Namespace: machineNamespace, Name: rejoinFixtureMachine}, typedMachine); err == nil {
		if _, err := machineReconciler.Reconcile(ctx, key); err == nil {
			if err := controllerClient.Get(ctx, types.NamespacedName{Namespace: machineNamespace, Name: rejoinFixtureMachine}, typedMachine); err == nil {
				syncMachineStatus(ctx, stationClient, typedMachine)
			}
		}
	}
	current, err := stationClient.Resource(nodeOperationGVR).Namespace(rejoinFixtureNamespace).Get(ctx, rejoinOperationName(rejoinFixtureNode, rejoinRequestID), metav1.GetOptions{})
	if err == nil {
		phase, _, _ := unstructured.NestedString(current.Object, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" || phase == "Blocked" {
			return true, rejoinRequestID
		}
	}
	return false, ""
}

func syncMachineStatus(ctx context.Context, stationClient *dynamicfake.FakeDynamicClient, machine *stationv1alpha1.Machine) {
	current, err := stationClient.Resource(machineGVR).Namespace(machineNamespace).Get(ctx, machine.Name, metav1.GetOptions{})
	if err != nil {
		return
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(machine)
	if err != nil {
		return
	}
	status, found, _ := unstructured.NestedMap(encoded, "status")
	if !found {
		return
	}
	current.Object["status"] = status
	_, _ = stationClient.Resource(machineGVR).Namespace(machineNamespace).Update(ctx, current, metav1.UpdateOptions{})
}

func syncOperation(ctx context.Context, stationClient *dynamicfake.FakeDynamicClient, operation *stationv1alpha1.NodeOperation) {
	resource := stationClient.Resource(nodeOperationGVR).Namespace(operation.Namespace)
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(operation)
	if err != nil {
		return
	}
	encoded["apiVersion"], encoded["kind"] = "station.rlark.io/v1alpha1", "NodeOperation"
	current, err := resource.Get(ctx, operation.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, _ = resource.Create(ctx, &unstructured.Unstructured{Object: encoded}, metav1.CreateOptions{})
		return
	}
	if err != nil {
		return
	}
	status, found, _ := unstructured.NestedMap(encoded, "status")
	if found {
		current.Object["status"] = status
		_, _ = resource.Update(ctx, current, metav1.UpdateOptions{})
	}
}

type loopbackFixtureAuth struct {
	next  http.Handler
	token string
}

func (a loopbackFixtureAuth) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+a.token)
	a.next.ServeHTTP(writer, request)
}
