# Station API 类型（试验性）

`NodeOperation` 是试验性的节点生命周期 API。controller-manager 仅在设置
`--enable-node-operations` 时注册对应控制器，默认部署保持关闭。Gateway 提供
仅管理员可用的创建/查询路由。Stations 界面可提交 Join/Remove 并从操作历史恢复轮询。
Join 可选择标准 containerd 或 Fleet socket profile，会在执行 kubeadm 前检查 socket，
并在核对新 Node UID 后设置 runtime 标签。成员集群必须预先部署匹配的 node-agent
DaemonSet。同名 Station 若仍绑定旧 UID，kubeadm 可先完成，但 RLark readiness 会等待；
新鲜观测到达后提交显式 Rebind，Join 调谐器会继续推进。Offline Station 在存在不同 UID
的新鲜观测时也可进行重绑审查。
Join 已连接试验性 SSH 执行器，依赖 CRD、成员凭证、专用 RBAC 和 RLark readiness
集成。剔除会遍历所有登记成员集群，按 Node UID 移除归属明确的 WireGuard Peer/配置，
验证全局无残留，再等待网络 DaemonSet Pod 停止后 reset worker 并删除精确 Node UID。
所有成员 kubeconfig 和网络 ConfigMap 必须可访问。未标记的旧配置会在排空前阻止，除非
新鲜且绑定 Node UID 的 Station 观测能证明其公钥/地址归属。该功能仍是实验能力，不要在生产
集群启用。内部辅助函数可从经验证的成员 kubeconfig 推导 kubeadm 发现信息，
并为操作创建或撤销短期加入凭证。SSH 辅助函数要求固定主机密钥；排空辅助函数
拒绝控制面节点、未受控 Pod 和使用 `emptyDir` 的 Pod，并通过 Kubernetes Eviction
API 驱逐。2026-11-12 在本地开发集群的 Notebook 上完成了 Join 和 Remove，包含 kubeadm reset 与精确 UID 删除。Join 仍需手动部署 Agent，Drain 也必须先手动删除固定到该节点的测试 Deployment；本次没有验证 WireGuard 接入或干净的一次性集群。完整生命周期仍属试验能力。

标准 containerd node-agent 示例为 `apps/rlark/docs/examples/agent-node-containerd-daemonset.yaml`；
每个成员集群须为实际 runtime 部署匹配 profile。`deploy/controller-node-operations-patch.yaml`
只打开 manager 参数；管理面和成员集群
RBAC 必须分别审查部署，并先在管理 API 安装 CRD。SSH 凭证存放在独立的
`station-operation-credentials` namespace，不在 ClusterRole 的 Secret 权限范围；controller
只能在该 namespace 读/删 Secret。接受操作前须确保 Gateway 可在此创建 Secret。该补丁不是生产部署方案，未在一次性
集群完成端到端测试前，不应应用到承载业务的集群。

需要从非 WireGuard 节点采集 Station 观测时，在成员集群一次性安装
`deploy/station-agent-identity.yaml`。它会排除带有
`station.rlark.io/network=enabled` 标签的节点；这些节点由身份与 WireGuard
双容器 DaemonSet 管理，并引用集群已有的 Station ConfigMap、ServiceAccount 和 CA Secret。

内部加入脚本会校验 kubeadm 参数，默认拒绝已有 kubelet 配置。用户显式选择 reset 后，
controller 会核对主机名、目标集群中无同名 Node 或相同 systemUUID，并拒绝控制面主机，再执行带操作标记的
`kubeadm reset`；中断后结果不明时不会自动重放。SSH sudo 凭证与登录凭证分开，通过标准输入传递。
kubelet 客户端证书校验可验证成员 CA
和节点身份，但不能单独证明 Node UID 的归属，也不能据此自动接管已有节点。
试验调谐器会等待本操作的 kubelet 证书和 Node 注册，并使用乐观锁更新状态。
Join 和 Remove 也已在本地开发集群进行过验证；本次测试不能证明 Agent 会自动安装，或任意绑定到节点的工作负载都可自动剔除。
加入尝试在节点本地写入操作标记，避免结果未知后重复执行。试验性的 SSH 加入
驱动核对 kubelet 证书与本次 bootstrap 身份提交且已批准的 CSR，再观测成员
Node 并撤销 token。加入失败后会保留本次操作标记，不自动重复 kubeadm；须先检查主机状态，再通过明确的恢复操作继续。该路径仍是试验功能：本地测试未覆盖干净的一次性集群、生产授权或 Agent 自动部署。
内部调谐器在操作删除时通过 finalizer 清理归属于该操作的 bootstrap 和 SSH
凭证。删除操作不会撤销已发生的加入、恢复节点调度或回滚部分剔除；发起新操作
前须检查节点实际状态。
试验性的 RLark 验证阶段在匹配成员 Node UID 后启用节点上报，并等待匹配的
Station 绑定和 RLark Node 就绪；已有冲突的启用标签会阻止该阶段。
短期 bootstrap token 作为 kubeadm 参数传入，运行期间目标机器的特权用户
可能看到该参数。

此独立 Go module 定义 `station.rlark.io/v1alpha1` API 类型、生成 CRD、controller 和 controller manager。Station API、controller、真实身份 agent 及双容器网络 DaemonSet 已在本地、机器人和两台裸金属节点运行；WireGuard 由 `extensions/station-gateway` 的网络容器实现。NodeOperation CRD 已装入 KCP root workspace，试验控制器已在本地开发集群完成 Notebook Join/Remove 测试。测试需要手动部署 Agent 并清理固定到节点的测试 Deployment，未验证 WireGuard 接入或空白主机自动接入。Agent 会随心跳从 `/proc/stat` 上报内核启动时间。公网镜像仓库、整机恢复和自动凭据轮换仍未完成。

字段归属、namespace 映射、校验、生命周期语义和待决设计见[API 契约](../../docs/design/station-api-contract.zh-CN.md)。

身份协调器拒绝已取消的获取请求，并在取消与获取竞争时释放锁。该锁仅串行化活动 manager 进程；可协作 leader-only 探针的正常交接已在隔离 kcp 验证，协调过程中的切换和分布式 fencing 尚未验证。

测试和 vet 需关闭仓库 Go workspace：

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

`TestLiveStationReconcileAndRebind` 还需将 `STATION_TEST_ISOLATED_KUBECONFIG` 指向已安装五份 Station 身份 CRD 的一次性 workspace。测试会创建 namespace 和资源，禁止指向生产 workspace；Cluster 注册解析和成员 Node 读取使用测试替身，并在真实 CRD status 和审计资源上测试注入审计故障后的恢复；它不测试 NodeOperation。

用仓库固定版本的生成器更新 deepcopy 代码和 CRD：

```bash
GOWORK=off ../../api/bin/controller-gen-v0.18.0 object paths=./api/v1alpha1/...
GOWORK=off ../../api/bin/controller-gen-v0.18.0 crd:crdVersions=v1 paths=./api/v1alpha1/... output:crd:artifacts:config=./config/crd/bases
```
