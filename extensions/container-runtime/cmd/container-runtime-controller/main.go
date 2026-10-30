// container-runtime-controller runs companion validation and optional preview
// Job reservations. It does not perform host runtime effects.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	containerapi "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	containercontroller "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	nativeapi "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

func main() {
	enableReservations := flag.Bool("enable-job-reservations", false, "Opt in to preview Raw process Task creation and capacity reservation; no Agent dispatch")
	leaderElect := flag.Bool("leader-elect", true, "Use a Kubernetes Lease for controller leadership")
	leaderNamespace := flag.String("leader-election-namespace", "rlark-system", "Namespace for the controller leadership Lease")
	flag.Parse()
	if *leaderNamespace == "" {
		fail(fmt.Errorf("leader-election-namespace is required"))
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nativeapi.AddToScheme(scheme))
	utilruntime.Must(containerapi.AddToScheme(scheme))
	config := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, LeaderElection: *leaderElect, LeaderElectionNamespace: *leaderNamespace, LeaderElectionID: "container-runtime-controller.containers.rlark.io"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := (&containercontroller.ContainerClusterReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		fail(err)
	}
	if err := (&containercontroller.ContainerNodeReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		fail(err)
	}
	if err := (&containercontroller.ContainerJobReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		fail(err)
	}
	if err := (&containercontroller.AgentReleaseReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		fail(err)
	}
	if *enableReservations {
		direct, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			fail(fmt.Errorf("create uncached Job reservation client: %w", err))
		}
		if err := (&containercontroller.JobReservationReconciler{Client: direct}).SetupWithManager(mgr); err != nil {
			fail(err)
		}
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
