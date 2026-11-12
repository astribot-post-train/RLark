package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	"github.com/rlinf/rlark/extensions/station/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var kubeconfig, credentialsNamespace, metricsAddress, probeAddress, networkNamespace, networkName string
	flag.StringVar(&kubeconfig, "management-kubeconfig", os.Getenv("KUBECONFIG"), "management API kubeconfig; omit to use in-cluster credentials")
	flag.StringVar(&credentialsNamespace, "member-credentials-namespace", "station-system", "namespace containing per-member kubeconfig Secrets")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "Prometheus metrics bind address")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "health and readiness probe bind address")
	flag.StringVar(&networkNamespace, "site-network-namespace", "station-system", "namespace of the SiteNetwork that answers NetworkEnrollments")
	flag.StringVar(&networkName, "site-network-name", "default", "name of the SiteNetwork that answers NetworkEnrollments")
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		ctrl.Log.Error(err, "load management Kubernetes credentials")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "register core API types")
		os.Exit(1)
	}
	if err := stationv1alpha1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "register Station API types")
		os.Exit(1)
	}
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress:  probeAddress,
		LeaderElection:          true,
		LeaderElectionID:        "station-controller.station.rlark.io",
		LeaderElectionNamespace: "station-system",
	})
	if err != nil {
		ctrl.Log.Error(err, "create Station controller manager")
		os.Exit(1)
	}
	registration := controller.ManagementClusterResolver{Reader: manager.GetAPIReader()}
	identityOps := controller.NewSerializedIdentityOperations()
	readiness := controller.ManagementNodeReadiness{Reader: manager.GetAPIReader()}
	memberSource := controller.SecretKubeconfigSource{Reader: manager.GetAPIReader(), Namespace: credentialsNamespace}
	targetReader, err := controller.NewKubeconfigNodeReader(memberSource)
	if err != nil {
		ctrl.Log.Error(err, "create member Node reader")
		os.Exit(1)
	}
	audit, err := controller.NewKubernetesAuditSink(manager.GetClient(), manager.GetAPIReader())
	if err != nil {
		ctrl.Log.Error(err, "create durable Station audit sink")
		os.Exit(1)
	}
	stationReconciler, err := controller.NewStationReconciler(manager.GetClient(), manager.GetAPIReader(), registration, identityOps, readiness, controller.DefaultOfflineAfter, time.Now)
	if err != nil {
		ctrl.Log.Error(err, "create Station reconciler")
		os.Exit(1)
	}
	if err := stationReconciler.SetupWithManager(manager); err != nil {
		ctrl.Log.Error(err, "register Station reconciler")
		os.Exit(1)
	}
	rebindReconciler, err := controller.NewRebindReconciler(manager.GetClient(), manager.GetAPIReader(), registration, identityOps, targetReader, audit, time.Now)
	if err != nil {
		ctrl.Log.Error(err, "create Rebind reconciler")
		os.Exit(1)
	}
	if err := rebindReconciler.SetupWithManager(manager); err != nil {
		ctrl.Log.Error(err, "register Rebind reconciler")
		os.Exit(1)
	}
	networkReconciler, err := controller.NewNetworkReconciler(manager.GetClient(), manager.GetAPIReader(), types.NamespacedName{Namespace: networkNamespace, Name: networkName}, time.Now)
	if err != nil {
		ctrl.Log.Error(err, "create SiteNetwork reconciler")
		os.Exit(1)
	}
	if err := networkReconciler.SetupWithManager(manager); err != nil {
		ctrl.Log.Error(err, "register SiteNetwork reconciler")
		os.Exit(1)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "register health check")
		os.Exit(1)
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "register readiness check")
		os.Exit(1)
	}
	if err := manager.AddReadyzCheck("cache-sync", func(request *http.Request) error {
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		if !manager.GetCache().WaitForCacheSync(ctx) {
			return errors.New("controller caches are not synchronized")
		}
		return nil
	}); err != nil {
		ctrl.Log.Error(err, "register cache readiness check")
		os.Exit(1)
	}
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "run Station controller manager")
		os.Exit(1)
	}
}
