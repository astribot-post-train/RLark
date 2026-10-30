// container-runtime-controller runs validation-only companion reconcilers. It
// does not schedule Tasks or perform host runtime effects.
package main

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	containerapi "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	containercontroller "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	nativeapi "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nativeapi.AddToScheme(scheme))
	utilruntime.Must(containerapi.AddToScheme(scheme))
	config := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme})
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
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
