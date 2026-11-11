# GPU 集群接入

## 概述

接入 Kubernetes 数据面需要 Agent 证书及供 `rlarkadm` 使用的 `DeployConfig`。当前管理表单用于签发证书，不要求填写集群类型或区域，也不会生成 Shell 安装命令。

## 通过 UI 获取证书

### 第一步：签发集群证书

1. 登录管理平台 `http://<host>:5173/admin`。
2. 打开**集群管理** → **创建集群**。
3. 只需输入集群名称，例如 `my-cluster-01`。
4. 选择**签发证书**。

> **截图说明：** 截图来自示例环境，资源名称和数据仅供说明，实际环境会有所不同。

![创建集群](../../images/ui/admin-create-cluster.jpg)

签发后，页面会显示集群名称、Server 地址和完整部署 YAML，并将名称加入**已签发集群**；之后仍可展开该记录并再次复制 YAML。

管理员可在**系统配置 > 部署配置**中设置生成 YAML 的默认值，这些字段沿用 `rlarkadm` 的 `DeployConfig` 结构。已配置的控制面与 SSH 地址会覆盖生成值；TLS 验证、Kubeconfig、Agent 镜像、共享 RLark 镜像、镜像拉取策略、镜像拉取 Secret 和 containerd socket 也会在有值时写入 YAML。展开历史签发记录时会使用当前默认值重新生成 YAML。

!!! warning "保护 YAML"
    页面显示的 `agent-key` 是私钥。请安全保存复制的 YAML，且不要在其他集群复用。

### 第二步：完善部署 YAML

UI 输出与 `rlarkadm` 接受的 `DeployConfig` 一致：

```yaml
apiVersion: rlark.io/v1alpha1
kind: DeployConfig
plane: data
control-plane-address: https://<control-plane>:8443

cert:
  ca-cert: |
    -----BEGIN CERTIFICATE-----
    <CA 证书>
    -----END CERTIFICATE-----
  agent-cert: |
    -----BEGIN CERTIFICATE-----
    <Agent 证书>
    -----END CERTIFICATE-----
  agent-key: |
    -----BEGIN PRIVATE KEY-----
    <Agent 私钥>
    -----END PRIVATE KEY-----

kubernetes:
  kubeconfig: ~/.kube/config
  agent-image: rlark:latest
```

将 `kubernetes.kubeconfig` 替换为可向目标集群部署资源的 kubeconfig，并设置可用的 Agent 镜像。仅在启用需要共享 RLark 镜像的组件时添加可选的 `kubernetes.image`。所有可用字段参见[配置参考](../reference/configuration.md)。

### 第三步：安装数据面

将完善后的 YAML 保存为 `deploy-data-plane.yaml`，再从受信任的管理主机运行：

```bash
rlarkadm install -f deploy-data-plane.yaml
```

UI 不会生成此命令，也不会代为执行。

### 第四步：验证注册

1. 返回**集群管理** → **集群列表**或**节点管理**。
2. 等待 Agent 连接并同步节点。
3. 确认集群在线，且预期 Worker 节点已经出现。

![验证集群节点](../../images/ui/admin-clusters-nodes.jpg)

## 通过 CLI 获取证书

自动化场景可签发 Agent 证书并使用仓库维护的部署示例：

```bash
rlarkctl sign \
  --role=agent \
  --client-id=agent-my-cluster-1 \
  --output=/tmp/agent-certs

rlarkadm install -f apps/rlark/docs/examples/deploy-data-plane.yaml
```

安装前请更新示例中的 Server 地址、证书内容或路径、kubeconfig 和镜像。

## 添加调度元数据

使用节点管理的批量编辑器设置城市、一个或多个云算力/端算力/真机分类、GPU 型号或设备型号，也可批量 Cordon/Uncordon。字段存储在控制面的 Node CR 上，并会在 Agent 刷新 Kubernetes 发现状态时保留。

## 运行冒烟测试

如需在 Kubernetes 控制面节点运行工作负载，应显式设置 `rlark.io/enabled=true` 以启用 RLark 上报，确认节点 Agent 可以运行，并在工作负载 Pod 模板中配置所需的 Kubernetes 污点容忍。需要 GPU 时，安装对应运行时和设备插件并核验可分配 GPU 资源。为控制面和网络网关预留资源；启用或显示节点不会绕过污点，也不保证任务可以调度。

从业务平台创建一个小型 Job，为 Worker 选择刚接入的集群并填写可用镜像后提交。确认 Worker 进入 Running、日志可查看；如果镜像提供 Shell，再确认 WebTerminal 可以打开。

## 故障排查

### NVIDIA 在设备插件启动前创建容器失败

同时检查运行时 stderr 与 kubelet/containerd 日志；仅凭 OCI runtime 退出码无法判断是驱动、镜像还是网络故障。实测 H100 主机（驱动 565.57.01、NVIDIA Container Toolkit 1.20.2、containerd 2.2.1）中，`mode = "auto"` 选择 `jit-cdi`，生成 CDI 配置时在 `nvSandboxUtilsShutdown()` 中触发 `SIGSEGV`。在 `/etc/nvidia-container-runtime/config.toml` 显式设置 `mode = "legacy"` 绕过该路径后，两台均通过容器 GPU 枚举及申请一张 GPU 的 Kubernetes Job。这是该组合下验证过的规避措施，不是通用要求，也未证明底层库缺陷的具体原因。

修改前备份配置。containerd 配置版本为 3 时，可在已导入的运行时片段中增加以下内容（例如 `/etc/containerd/conf.d/99-nvidia.toml`，前提是主配置确实导入该目录）：

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia]
  runtime_type = 'io.containerd.runc.v2'
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia.options]
  BinaryName = '/usr/bin/nvidia-container-runtime'
  SystemdCgroup = true
```

默认运行时保留 `runc`。先用 `containerd config dump` 验证，再逐台重启 containerd 并检查节点及已有工作负载。创建名为 `nvidia`、`handler: nvidia` 的 `node.k8s.io/v1` RuntimeClass。本方案的 NVIDIA 设备插件 DaemonSet 和 GPU 工作负载 Pod 模板均需设置 `runtimeClassName: nvidia`，GPU 工作负载还须申请 `nvidia.com/gpu`。设备插件需要访问 `/var/lib/kubelet/device-plugins`；控制面节点同时提供 GPU 时，应配置对应污点容忍。核验可分配 GPU 数、插件注册日志，并在每台节点通过调度器运行一卡 Job。枚举成功不代表 CUDA 训练或多节点 NCCL 通信已验证。

离线镜像导入只填充当前主机缓存。部署前应在所有目标节点预加载并使用 `IfNotPresent`，或配置所有节点均能访问的仓库；镜像引用中的 `localhost` 不代表每台主机都运行仓库。

回滚时先停止受影响的 GPU 工作负载，确认消费者退出后只移除本方案创建的插件及 RuntimeClass 资源，删除新增运行时片段，恢复备份的 NVIDIA 配置，再逐台重启 containerd。重新检查 Kubernetes、Station 网关和 Agent。清理临时诊断包装脚本前保留日志。验证 containerd 重启不等于验证整机重启恢复。

| 症状 | 排查方向 |
|------|---------|
| Agent 无法连接 | Server 地址、CA/Agent 证书是否匹配、出站网络 |
| 集群显示离线 | Agent Deployment 日志和证书有效期 |
| 节点未出现 | Agent 模式、本地 RBAC、节点控制器日志 |
| Job 无法调度 | 所选集群、节点选择器、资源申请、镜像拉取状态 |

## 注册管理

- 每个数据面集群分别签发 Agent 证书。
- 复制的部署 YAML 含有 `agent-key`，必须按 Secret 保护。
- 使用 `rlarkctl sign` 轮换证书，并重新部署受影响的 Agent。
