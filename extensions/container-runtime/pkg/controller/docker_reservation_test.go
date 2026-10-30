package controller

import (
	"context"
	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

func TestDockerJobPersistsExactCompanionAllocationAndRejectsRayBeforeEffects(t *testing.T) {
	for _, mode := range []string{"single-container", "ray", "multiple"} {
		t.Run(mode, func(t *testing.T) {
			r, req := jobFixture(t)
			ctx := context.Background()
			var job native.Job
			if err := r.Client.Get(ctx, req.NamespacedName, &job); err != nil {
				t.Fatal(err)
			}
			task := &job.Spec.Tasks[0]
			task.Runtime.Kind = "Docker"
			task.Workload.Kind = dockerruntime.CarrierType
			task.Workload.Carrier.Type = dockerruntime.CarrierType
			task.Workload.Parameters.Raw = []byte(`{"image":"image","hostNetwork":true}`)
			if mode == "ray" {
				task.Workload.Kind = "rlinf.io/ray"
			}
			if mode == "multiple" {
				other := *task.DeepCopy()
				other.Name = "second"
				job.Spec.Tasks = append(job.Spec.Tasks, other)
			}
			if err := r.Client.Update(ctx, &job); err != nil {
				t.Fatal(err)
			}
			var binding ext.ContainerJob
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "job"}, &binding); err != nil {
				t.Fatal(err)
			}
			binding.Spec.Runtime = "Docker"
			if err := r.Client.Update(ctx, &binding); err != nil {
				t.Fatal(err)
			}
			var cc ext.ContainerCluster
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "cloud"}, &cc); err != nil {
				t.Fatal(err)
			}
			cc.Spec.Runtime = "Docker"
			if err := r.Client.Update(ctx, &cc); err != nil {
				t.Fatal(err)
			}
			var node ext.ContainerNode
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &node); err != nil {
				t.Fatal(err)
			}
			node.Spec.Runtime = "Docker"
			if err := r.Client.Update(ctx, &node); err != nil {
				t.Fatal(err)
			}
			var nn native.Node
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "worker"}, &nn); err != nil {
				t.Fatal(err)
			}
			nn.Spec.AgentType = native.AgentTypeDocker
			if err := r.Client.Update(ctx, &nn); err != nil {
				t.Fatal(err)
			}
			reconcileJob(t, r, req, 5)
			if mode != "single-container" {
				var tasks native.TaskList
				var allocations ext.ContainerAllocationList
				if err := r.Client.List(ctx, &tasks); err != nil {
					t.Fatal(err)
				}
				if err := r.Client.List(ctx, &allocations); err != nil {
					t.Fatal(err)
				}
				if len(tasks.Items) != 0 || len(allocations.Items) != 0 {
					t.Fatal("unsupported Docker Job caused effects")
				}
				return
			}
			got, a := allocated(t, r)
			if got.Spec.Placement.DockerBinding != nil || got.Spec.Placement.Binding != nil || got.Spec.Runtime.Kind != "Docker" || a.Spec.GPUUUIDs[0] != "GPU-a" {
				t.Fatalf("noncanonical Docker allocation: %+v %+v", got.Spec, a.Spec)
			}
			reconcileJob(t, r, req, 3)
			_, retry := allocated(t, r)
			if retry.UID != a.UID {
				t.Fatal("restart changed allocation")
			}
		})
	}
}
