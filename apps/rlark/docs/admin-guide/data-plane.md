# GPU Cluster Onboarding

## Overview

Onboarding a Kubernetes data plane requires an Agent certificate and a `DeployConfig` for `rlarkadm`. The current admin form signs certificates; it does not ask for a cluster type or region and does not generate a shell installation command.

## UI-Based Certificate Flow

### Step 1: Sign the Cluster Certificate

1. Sign in to the administrator console at `http://<host>:5173/admin`.
2. Open **Cluster Management** → **Create Cluster**.
3. Enter only the cluster name, for example `my-cluster-01`.
4. Choose **Sign Certificate**.

> **Screenshot note:** Screenshots are from an example environment. Resource names and data are illustrative; your environment will differ.

![Create Cluster](../images/ui/admin-create-cluster.jpg)

After signing, the page displays the cluster name, Server address, and a complete deploy YAML. It also adds the name to **Signed Clusters**, where the YAML can be opened and copied again.

Administrators can set the generated YAML defaults under **System Configuration > Deployment**. These settings follow the `rlarkadm` `DeployConfig` structure. The configured control-plane and SSH addresses override generated values; TLS verification, kubeconfig, Agent image, shared RLark image, image pull policy, image pull Secrets, and containerd socket are also applied when present. Existing signed-cluster entries use the current defaults whenever their YAML is opened.

!!! warning "Protect the YAML"
    The displayed `agent-key` is a private key. Store the copied YAML securely and never reuse it for another cluster.

### Step 2: Complete the Deploy YAML

The UI output matches the `DeployConfig` accepted by `rlarkadm`:

```yaml
apiVersion: rlark.io/v1alpha1
kind: DeployConfig
plane: data
control-plane-address: https://<control-plane>:8443

cert:
  ca-cert: |
    -----BEGIN CERTIFICATE-----
    <CA certificate>
    -----END CERTIFICATE-----
  agent-cert: |
    -----BEGIN CERTIFICATE-----
    <Agent certificate>
    -----END CERTIFICATE-----
  agent-key: |
    -----BEGIN PRIVATE KEY-----
    <Agent private key>
    -----END PRIVATE KEY-----

kubernetes:
  kubeconfig: ~/.kube/config
  agent-image: rlark:latest
```

Replace `kubernetes.kubeconfig` with a kubeconfig that can deploy to the target cluster and set an available Agent image. Add the optional `kubernetes.image` only when enabling components that require the shared RLark image. See [Configuration Reference](../reference/configuration.md) for all accepted keys.

### Step 3: Install the Data Plane

Save the completed YAML as `deploy-data-plane.yaml`, then run it from a trusted administration host:

```bash
rlarkadm install -f deploy-data-plane.yaml
```

The UI does not generate this command or execute it for you.

### Step 4: Verify Registration

1. Return to **Cluster Management** → **Cluster List** or **Node Management**.
2. Wait for the Agent connection and node synchronization.
3. Confirm that the cluster appears online and expected Worker nodes are present.

![Verify Cluster Nodes](../images/ui/admin-clusters-nodes.jpg)

## CLI-Based Certificate Flow

For automation, sign an Agent certificate and use the maintained deployment example:

```bash
rlarkctl sign \
  --role=agent \
  --client-id=agent-my-cluster-1 \
  --output=/tmp/agent-certs

rlarkadm install -f apps/rlark/docs/examples/deploy-data-plane.yaml
```

Update the example's Server address, certificate values or paths, kubeconfig, and images before installation.

## Add Scheduling Metadata

Use the Node Management batch editor to set city, one or more cloud/edge/robot categories, GPU model, or device model. You can also cordon or uncordon selected nodes. These fields are stored on the control-plane Node CR and preserved when the Agent refreshes discovered Kubernetes state.

## Run a Smoke Test

To use a Kubernetes control-plane node for workloads, explicitly enable RLark reporting with `rlark.io/enabled=true`, verify the node Agent can run there, and provide appropriate Kubernetes tolerations in the workload Pod template. Install the GPU runtime and device plugin where required, and verify allocatable GPU resources. Reserve capacity for the control plane and any network gateway; enabling a node or displaying it does not bypass taints or guarantee scheduling.

Create a small Job from the platform console, choose the onboarded cluster for its Worker, use an available image, and submit it. Verify that the Worker reaches Running, logs are available, and WebTerminal opens if the image supplies a shell.

## Troubleshooting

### NVIDIA container creation fails before the plugin starts

Inspect the runtime's stderr as well as kubelet/containerd logs. An OCI runtime exit code alone does not identify a driver, image, or networking failure. On the tested H100 hosts (driver 565.57.01, NVIDIA Container Toolkit 1.20.2, containerd 2.2.1), `mode = "auto"` selected `jit-cdi` and container creation crashed with `SIGSEGV` in `nvSandboxUtilsShutdown()` while generating a CDI specification. Explicit `mode = "legacy"` in `/etc/nvidia-container-runtime/config.toml` bypassed that path; both hosts then passed container GPU enumeration and a Kubernetes Job requesting one GPU. This is a tested workaround for that combination, not a general requirement or proof of the underlying library defect.

Back up configuration before changing it. With containerd configuration version 3, add the following to an imported runtime drop-in (for example `/etc/containerd/conf.d/99-nvidia.toml`, only if the main configuration imports that directory):

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia]
  runtime_type = 'io.containerd.runc.v2'
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia.options]
  BinaryName = '/usr/bin/nvidia-container-runtime'
  SystemdCgroup = true
```

Keep the default runtime as `runc`. Validate with `containerd config dump`, restart containerd one host at a time, and check node and existing workload health. Define a `node.k8s.io/v1` RuntimeClass named `nvidia` with `handler: nvidia`. Both the NVIDIA device-plugin DaemonSet and GPU workload Pod templates must set `runtimeClassName: nvidia` for this setup; GPU workloads must request `nvidia.com/gpu`. The device plugin needs access to `/var/lib/kubelet/device-plugins`; give it the appropriate control-plane toleration when that node also provides GPUs. Verify allocatable GPU counts, plugin registration logs, and a scheduled one-GPU Job on each host. Enumeration does not validate CUDA training or multi-node NCCL communication.

Offline image imports only populate the current host's cache. Preload every target node and use `IfNotPresent`, or configure a registry reachable by every node before deployment; a `localhost` image reference does not imply a registry exists on every host.

For rollback, stop affected GPU workloads first, remove only the plugin/RuntimeClass resources created for this setup after their consumers are gone, remove the added drop-in, restore the saved NVIDIA configuration, and restart containerd one host at a time. Recheck Kubernetes, Station gateways, and Agents. Preserve diagnostic logs before removing temporary diagnostic wrappers. A containerd restart check does not establish recovery after a full host reboot.

| Symptom | Check |
|---------|-------|
| Agent not connecting | Server address, CA/Agent certificate pair, outbound connectivity |
| Cluster shows offline | Agent deployment logs and certificate validity |
| Nodes not appearing | Agent mode, local RBAC, and node-controller logs |
| Job does not schedule | Selected cluster, node selectors, resource requests, image pull status |

## Registration Management

- Sign a separate Agent certificate for each data-plane cluster.
- Treat copied deploy YAML as a secret because it contains `agent-key`.
- Rotate certificates with `rlarkctl sign` and redeploy the affected Agent.
