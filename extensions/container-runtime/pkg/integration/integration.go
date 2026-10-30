// Package integration assembles the optional extension into native controllers.
package integration

import (
	containerapi "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	nativecontroller "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager"
	nativejob "github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// ControllerExtension returns explicit optional hooks. Deployments must install
// the extension CRDs before opting in; native binaries remain extension-free.
func ControllerExtension() nativecontroller.Extension {
	return nativecontroller.Extension{
		AddToScheme: containerapi.AddToScheme,
		Setup: func(mgr manager.Manager) error {
			client := mgr.GetClient()
			for _, setup := range []func() error{
				func() error { return (&controller.ContainerClusterReconciler{Client: client}).SetupWithManager(mgr) },
				func() error { return (&controller.ContainerNodeReconciler{Client: client}).SetupWithManager(mgr) },
				func() error { return (&controller.ContainerJobReconciler{Client: client}).SetupWithManager(mgr) },
				func() error { return (&controller.AgentReleaseReconciler{Client: client}).SetupWithManager(mgr) },
			} {
				if err := setup(); err != nil {
					return err
				}
			}
			return nil
		},
		JobOwner: func(c client.Reader) nativejob.OwnerResolver { return controller.JobOwner{Client: c} },
	}
}
