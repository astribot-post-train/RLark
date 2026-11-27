package clusterupgrade

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestStockRecoveryAfterSIGKILL(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for the isolated Kubernetes API test")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	member, err := kubernetes.NewForConfig(config)
	mustStockTest(t, err)
	ctx := t.Context()
	_, err = member.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}, metav1.CreateOptions{})
	mustStockTest(t, err)
	leases := member.CoordinationV1().Leases(Namespace)
	_, err = leases.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName}}, metav1.CreateOptions{})
	mustStockTest(t, err)
	image := "registry.example/upgrade@" + digest
	maps := member.CoreV1().ConfigMaps(Namespace)
	for name, data := range map[string]map[string]string{
		PolicyConfigMap:     {"mode": DriverStock, "executorImage": image},
		StockStateName:      {"image": "previous", "deploy-config": "baseline"},
		"fixture-workloads": {"cluster": "baseline", "node": "baseline"},
	} {
		_, err = maps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: data}, metav1.CreateOptions{})
		mustStockTest(t, err)
	}

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: LockedStockJobName("child")},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, ServiceAccountName: StockJobServiceAccount,
			Containers: []corev1.Container{{Name: "upgrade", Image: image}},
		}}}}
	ConfigureLockedStock(job, "child", false)
	job, err = member.BatchV1().Jobs(Namespace).Create(ctx, job, metav1.CreateOptions{})
	mustStockTest(t, err)

	path := filepath.Join(t.TempDir(), "kubeconfig")
	mustStockTest(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"test": {Server: config.Host, CertificateAuthorityData: config.CAData}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"test": {ClientCertificateData: config.CertData, ClientKeyData: config.KeyData}},
		Contexts:       map[string]*clientcmdapi.Context{"test": {Cluster: "test", AuthInfo: "test"}},
		CurrentContext: "test",
	}, path))
	binary, err := os.Executable()
	mustStockTest(t, err)
	pods := member.CoreV1().Pods(Namespace)
	createPod := func(name string) *corev1.Pod {
		t.Helper()
		pod, err := pods.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
			Spec: *job.Spec.Template.Spec.DeepCopy()}, metav1.CreateOptions{})
		mustStockTest(t, err)
		return pod
	}
	killDuring := func(pod *corev1.Pod, stage string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestStockRecoveryExecutorProcess$")
		cmd.Env = append(os.Environ(), "RLARK_STOCK_TEST_KUBECONFIG="+path,
			"RLARK_STOCK_TEST_POD="+pod.Name, "RLARK_STOCK_TEST_UID="+string(pod.UID), "RLARK_STOCK_TEST_KILL_STAGE="+stage)
		output, err := os.CreateTemp(t.TempDir(), "output")
		mustStockTest(t, err)
		defer func() { _ = output.Close() }()
		cmd.Stdout, cmd.Stderr = output, output
		mustStockTest(t, cmd.Start())
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		err = wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
			live, err := maps.Get(ctx, "fixture-workloads", metav1.GetOptions{})
			return err == nil && live.Data["stage"] == stage, err
		})
		if err != nil {
			log, _ := os.ReadFile(output.Name())
			t.Fatalf("did not reach %s: %v\n%s", stage, err, log)
		}
		mustStockTest(t, cmd.Process.Kill())
		var exit *exec.ExitError
		err = cmd.Wait()
		if !errors.As(err, &exit) {
			t.Fatalf("expected SIGKILL, got %v", err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || status.Signal() != syscall.SIGKILL {
			t.Fatalf("unexpected process exit: %v", err)
		}
		mustStockTest(t, pods.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0)),
			Preconditions: &metav1.Preconditions{UID: &pod.UID}}))
		held, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
		mustStockTest(t, err)
		held.Spec.RenewTime = ptr.To(metav1.NewMicroTime(time.Now().Add(-time.Hour)))
		_, err = leases.Update(ctx, held, metav1.UpdateOptions{})
		mustStockTest(t, err)
		_, _, err = Acquire(ctx, leases, "rlarkctl-release/unrelated", "unrelated", func(ctx context.Context, holder, executor string) (bool, error) {
			return PreviousUpgradeExecutorExited(ctx, member, holder, executor)
		})
		var conflict *LeaseConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("active original Job did not fence unrelated release: %v", err)
		}
	}
	killDuring(createPod("original"), "upgrade")
	killDuring(createPod("first-replacement"), "rollback")
	pod := createPod("second-replacement")
	err = runStockRecoveryFixture(ctx, config, ExecutorIdentity{ChildUID: "child", PodName: pod.Name, PodUID: string(pod.UID)}, "")
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("replacement did not finish rollback: %v", err)
	}
	live, err := maps.Get(ctx, "fixture-workloads", metav1.GetOptions{})
	mustStockTest(t, err)
	state, err := maps.Get(ctx, StockStateName, metav1.GetOptions{})
	mustStockTest(t, err)
	if live.Data["cluster"] != "baseline" || live.Data["node"] != "baseline" || state.Data["image"] != "previous" || state.Data[StockAttemptKey] != "" {
		t.Fatalf("rollback did not restore baseline: workloads=%v state=%v", live.Data, state.Data)
	}
}

func TestStockRecoveryExecutorProcess(t *testing.T) {
	path := os.Getenv("RLARK_STOCK_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("subprocess for the isolated recovery test")
	}
	config, err := clientcmd.BuildConfigFromFlags("", path)
	mustStockTest(t, err)
	err = runStockRecoveryFixture(t.Context(), config, ExecutorIdentity{
		ChildUID: "child", PodName: os.Getenv("RLARK_STOCK_TEST_POD"), PodUID: os.Getenv("RLARK_STOCK_TEST_UID"),
	}, os.Getenv("RLARK_STOCK_TEST_KILL_STAGE"))
	t.Fatalf("expected the test parent to kill the executor, got %v", err)
}

func runStockRecoveryFixture(ctx context.Context, config *rest.Config, identity ExecutorIdentity, killStage string) error {
	member, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	reader, err := client.New(config, client.Options{})
	if err != nil {
		return err
	}
	stock, err := holdStock(ctx, member, reader, identity)
	if err != nil {
		return err
	}
	defer stock.Release()
	return stock.Run("target", false, true, func(target, baseline string) (StockOperations, error) {
		write := func(ctx context.Context, stage, clusterImage, nodeImage string) error {
			if err := stock.Check(ctx); err != nil {
				return err
			}
			maps := member.CoreV1().ConfigMaps(Namespace)
			live, err := maps.Get(ctx, "fixture-workloads", metav1.GetOptions{})
			if err != nil {
				return err
			}
			live.Data = map[string]string{"stage": stage, "cluster": clusterImage, "node": nodeImage}
			if _, err := maps.Update(ctx, live, metav1.UpdateOptions{}); err != nil {
				return err
			}
			if stage == killStage {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}
		return stockTestOperations{
			upgrade: func(ctx context.Context) error {
				if killStage != "upgrade" {
					return errors.New("replacement unexpectedly replayed the forward upgrade")
				}
				return write(ctx, "upgrade", target, baseline)
			},
			rollback: func(ctx context.Context) error {
				if killStage == "rollback" {
					return write(ctx, "rollback", baseline, "partial")
				}
				return write(ctx, "restored", baseline, baseline)
			},
		}, nil
	})
}

func mustStockTest(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
