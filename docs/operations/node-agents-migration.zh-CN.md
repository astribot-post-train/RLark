# 把集群迁到声明式节点 Agent

2026-11-14 在本地 `rs-farm` 集群演练（三个节点，先是与线上相同的逐节点 Deployment，再是 rlarkadm 形态的 DaemonSet）。尚未在线上集群执行。功能说明：[在 Cluster 上声明节点 Agent](../../apps/rlark/docs/zh/admin-guide/agent.md#在-cluster-上声明节点-agent)。[English](node-agents-migration.md)

每个集群执行一次的批处理步骤。RLark 自己从不删除旧的节点 Agent 工作负载：它们是手工或 rlarkadm 创建的，只由批处理工具删除。

## 步骤依赖的事实（实测）

- 同一节点上两个节点 Agent 不冲突。某节点的隧道已存在时，Server 对第二条隧道先拒绝五次（新 Agent 日志里是 `websocket: bad handshake`，每 4–6 s 重试一次），之后接受，两个会话并存。节点心跳 Lease 留在第一条连接上；node server 套接字经稳定符号链接切到最新的 Agent。实测：新 Pod 启动后 25–31 s 隧道被接受；两者并存期间没有管理面 Node 变 Offline。
- DaemonSet Pod 在 node server 起来时就 Ready，早于隧道被接受。要等隧道（见下），不能只看 Ready。
- 删除旧 Agent 的代价与任何一次节点 Agent 重启相同：隧道关闭时 Lease 不释放，最后一次续约后 45 s 过期，另一条连接在下一次 15 s 重试时接手。逐节点实测 Offline 0、5、25 s（手工 Deployment），0–10 s（接管 DaemonSet）。运行中的 Task 不受影响。
- 重启 Server 或集群 Agent 自身有心跳空窗（Cluster Offline 最长约 1 分钟；Server 重启时每个 Node 0–45 s）。这些步骤与节点 Agent 步骤分开做。

## 每个集群的步骤

1. 先管理面：应用新的 `clusters.rlinf.io` CRD，再把 Server、Gateway、controller-manager 升到该版本。集群 Agent 连接时 Server 授予它读取自己 Cluster 的权限；新集群 Agent 连旧 Server 时节点 Agent 控制器无法启动。
2. 检查每个要运行节点 Agent 的 Node：`kubectl get nodes -o custom-columns=NAME:.metadata.name,SOCKET:.metadata.annotations.kubeadm\.alpha\.kubernetes\.io/cri-socket,MANAGED:.metadata.labels.rlark\.io/enabled`。套接字必须在 `/run` 下（没有该注解的 Node 上节点 Agent 不做镜像预拉取），且已有 `rlark.io/enabled=true`。确认集群 Agent 的镜像在这些 Node 上都能用同一个名字拉到：一个 DaemonSet 只有一个镜像。
3. 集群 Agent 的 Deployment（`rlark-agent`）一次改完：`strategy: {type: Recreate}`；镜像为新版本；`--image=<同一镜像>`；downward API 提供 `POD_NAME`、`POD_NAMESPACE`（rlarkadm 已设）；`--mode=cluster`（`--mode=both` 的 Agent 还在自己的节点上跑节点功能，但没有 `/host/run` 挂载，那里没有镜像预拉取；由 DaemonSet 接手）。等 Cluster 重新 Online。
4. 移交 Agent 命名空间里已经叫 `rlark-agent-node` 的对象（只处理存在的）：`kubectl -n rlark-system label sa/rlark-agent-node ds/rlark-agent-node app.kubernetes.io/managed-by=rlark-agent` 和 `kubectl label clusterrole/rlark-agent-node clusterrolebinding/rlark-agent-node app.kubernetes.io/managed-by=rlark-agent`。必须在第 5 步之前：集群 Agent 拒绝没有标签的对象并按退避重试，单独打标签不会唤醒它（实测拒绝 30 s 后打标签，14 s 后接管；可能要几分钟）。
5. 一个 Gateway 请求（管理员）：`PUT /api/v1/clusters/<cluster-id>/node-agents`，请求体 `{"enabled": true}`。`<cluster-id>` 是 Cluster 对象名（`rlark-<agent id>`）。实测 2 s 后三个节点的 DaemonSet Pod Ready。
6. 按旧工作负载逐个处理，先旧后新：等它所在每个节点上的新 Pod 连续 15 s 没有 `tunnel connection error`（`kubectl -n rlark-system logs <pod> --since=20s | grep -c 'tunnel connection error'` 为 0 且 Pod 已超过 40 s），再删旧工作负载。管理面 Node 重新 Online、Lease 换了持有者（`kubectl -n <cluster-id> get lease node-heartbeat-<node>`）后再处理下一个。
7. 验证：`kubectl -n rlark-system get ds rlark-agent-node` 期望数 = 就绪数；管理面 Node 全部 Online；每个节点一个 Job 能拉镜像（其管理面 Node 出现 `status.pullProgress`）。

## 机器人集群（`alexfu` = 3090 控制面，`s1u-01-x86`，`s1u-02-x86`）

现状：每个节点一个手工 Deployment，按主机名固定，各自传 `--containerd-socket`：`rlark-node-agent`（s1u-01-x86）、`rlark-node-agent-alexfu`、`rlark-node-agent-s1u-02`；集群 Agent `rlark-agent` 以 `--mode=both` 运行。预期套接字：`alexfu` 为 `unix:///run/containerd-fleet/containerd.sock`（安装器运行时），机器人为 `unix:///run/containerd-robot/containerd.sock`（第 2 步核对）。

第 1–5 步同上（第 3 步同时把 `rlark-agent` 改为 `--mode=cluster`；第 4 步仅当 `kubectl -n rlark-system get sa,ds rlark-agent-node` 有结果时做）。第 6 步逐节点、先选没有 Run 的机器人：删 `deploy/rlark-node-agent-s1u-02`，再删 `deploy/rlark-node-agent`（s1u-01-x86），最后 `deploy/rlark-node-agent-alexfu`。每台在 RLark 里 Offline 0–25 s；之后入网的机器人由 DaemonSet 自动得到节点 Agent（演练：准入后 70 s Ready，无需其他操作）。

## 办公室集群

现状：DaemonSet `rlark-local-node-agent`（固定在 `cailiang`，`/run/containerd/containerd.sock`，镜像名 `localhost:5555/...`）、`rlark-node-agent` 和 `rlark-node-agent-containerd`（`containerd-fleet` 节点）。没有叫 `rlark-agent-node` 的，第 4 步通常为空。这里第 2 步是关键：`cailiang` 用与其他节点不同的名字拉 Agent 镜像；第 5 步之前要让它的 containerd 认同一个仓库名（仓库信任），否则那里的 DaemonSet Pod 起不来。第 6 步按旧 DaemonSet 逐个删：`ds/rlark-local-node-agent`，再 `ds/rlark-node-agent-containerd`，最后 `ds/rlark-node-agent`，每个都在其节点上的新 Pod 通过隧道检查之后；同一个旧 DaemonSet 的所有节点同时经历 0–25 s 空窗。

## 云端集群

现状：rlarkadm 的 DaemonSet `rlark-agent-node`。第 4 步把它（连同 ServiceAccount、ClusterRole、ClusterRoleBinding）移交；第 5 步后集群 Agent 把它改写为声明的形式，逐节点滚动更新（演练：三个节点 1 分 40 秒，每节点 Offline 0–10 s）。第 6 步无需删除。
