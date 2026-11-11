package gateway

import (
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestValidExposedPortServiceRequiresCurrentTaskAndRequestedPorts(t *testing.T) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "task-ports", Namespace: "rlark-system", Annotations: map[string]string{"rlark.io/management-task-uid": "uid"}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: map[string]string{"rlark.io/management-task-uid": "uid"}, Ports: []corev1.ServicePort{{Name: "api", Protocol: corev1.ProtocolTCP, Port: 8080, NodePort: 31000}}}}
	declared := []api.ExposedPort{{Name: "api", Protocol: "TCP", TargetPort: 8080}}
	if !validExposedPortService(service, "task", "uid", declared) {
		t.Fatal("valid allocation rejected")
	}
	if validExposedPortService(service, "task", "new-uid", declared) {
		t.Fatal("old Task incarnation accepted")
	}
	declared[0].NodePort = 31001
	if validExposedPortService(service, "task", "uid", declared) {
		t.Fatal("wrong fixed port accepted")
	}
	declared[0].NodePort = 0
	service.Spec.Type = corev1.ServiceTypeClusterIP
	if validExposedPortService(service, "task", "uid", declared) {
		t.Fatal("non-external Service accepted")
	}
}
