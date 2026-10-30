# Docker 运行时执行计划

本文是独立 `extensions/container-runtime` Docker 路径的实施契约。Docker
继续使用 ContainerCluster、ContainerNode、ContainerJob、ContainerAllocation
附属资源，并将 Docker 实现保留在扩展模块中。

## 目标行为

Docker Cluster 与 Kubernetes Cluster 分开管理。每台登记的 Linux Docker 主机是
一个 ContainerNode。控制面选择整台主机，在派发前持久化唯一的
ContainerAllocation。Agent 只通过 mTLS 接收与 allocation 绑定的 Task 协议，使用
本地 Unix socket 连接宿主 Docker Engine，绝不把 socket 暴露给工作负载容器。

首个可执行契约是每个 Task 一个不可变 Docker 容器：

- 由版本化 Docker carrier 提供 image、命令、entrypoint、环境和工作目录。
- 要求 host network，保证多主机训练使用私网地址通信。
- 一个 allocation 独占本次授权 GPU 集合，Task 不能自行选择其他 UUID。
- provider 使用包含 Task UID、namespace、name、runtime 和 incarnation 的确定性
  labels，只允许接管完全匹配且具有本地创建记录的容器。
- create、start、inspect、stop、remove 都必须幂等；响应丢失时通过确定性名称和
  labels 重新确认。
- 已退出/dead 容器在其所有者证明删除前继续占用；未知容器永不接管或删除。

## Task 与 Ray 派发

现有控制器把 Job 展开为独立 Task。Docker 调度器复用每台主机的持久化独占锁，
只选择 Docker 节点，并在 Agent 启动前写入规范的 Task target 注解和精确
ContainerAllocation，不写旧版 DockerBinding。只有心跳新鲜、Engine ID 和 credential 匹配、目标 release
可用且 allocation UID/Task UID 当前有效时才派发。

Ray/RLinf 属于后续 gang 层。当前执行契约限定每个 Job 一个显式容器 Task，拒绝
Ray 组、`rayPort`、用户 GPU 分配以及受保留的 `RLARK_*` 环境变量。后续 Ray 适配器
必须在同一节点 allocation 中预留主机端口并拒绝冲突。多机 Ray Job 必须等待同一 Job generation 的所有成员
报告当前私网地址后才能通过屏障；在屏障实现前，API 应拒绝 Ray 组派发，不能启动
部分成员。

## Agent 协议与所有权

Agent 只允许 `assignment`、`get`、`authorize`、`claim`、`status`、`cleanup`。
API 对每次操作重新校验原生 Job、Task、Cluster、Node、ContainerNode、allocation、
租户、generation 和 credential。Agent 不持有 Kubernetes 凭据。Docker provider 在
`ContainerCreate` 前通过 admission callback 复用服务端授权结果。

状态分开表达期望、容器观测和清理证明：

- `ExecutionKnown=True` 表示有界的 Docker phase 与退出原因。
- 只有 owner-scoped stop/remove 后再次 inspect/list 证明容器不存在，才能写
  `ExecutionCleaned=True`。
- 只有 allocation 仍然有效时，才能为精确容器读取日志和 stats；不能把日志写入
  resource Status。
- Engine 故障保持 allocation 保护，不发送虚假 heartbeat 或 cleanup proof。

## 升级与发布

Docker 和 Raw 共用证书与 release 校验，但运行时参数互斥。Docker 安装包排在
`docker.service` 之后启动。Docker 升级执行尚未实现；后续选择器只能选择空闲 Docker 节点，并保留原有 Engine
socket 和节点 incarnation。目标 release 会阻止新分配；安装本身是带精确 allocation
和重启回执的 operation。

## 验收门槛

1. Fake Engine 覆盖所有权、幂等 create/recover、不可变配置、GPU 请求、host
   network、端口冲突、stop/remove 与故障恢复。
2. Scoped protocol 覆盖 Docker 节点在无 Kubernetes 凭据下授权和 claim，同时保证
   Raw 路径兼容。
3. 官方 SDK Unix socket 测试证明 Agent 连接本地 daemon。
4. 真实私网 Docker GPU 主机需通过 image pull/create/start/inspect、logs/stats、
   stop/remove 和重启恢复。当前金山云 Raw 测试机没有 Docker socket，不能替代该门槛。
5. 多机 Ray/RLinf、gang 屏障、端口预留和滚动升级在单 Task 生命周期稳定后单独验收。

## 本地主机取消屏障

执行开关要求 root 私有的持久化状态目录。通过独占进程锁与支持上下文取消的进程内串行化保护所有 Task UID 的 create/start、
stop/remove，开始清理前先 fsync 持久化取消记录。
旧 Agent、重复请求、进程重启或停止先于创建都不能重启该 Task。取消记录及追加式索引不随
allocation 释放删除；记录损坏、丢失或目录权限变化时停止执行并由管理员恢复。整个
目录丢失要求先隔离并退役旧节点，不能为旧凭据重新创建空 journal。
首个 carrier 使用 `docker.rlark.io/container` v1 的显式参数，拒绝用户设置
GPU UUID、受保留的所有权 labels、Pod 模板及未支持的 Ray 组；allocation 独立
注入 GPU UUID，Docker 使用 NVIDIA DeviceIDs。

## 实施顺序

1. 把 Docker admission 接入 scoped Agent Task loop，并增加 provider 生命周期/状态测试。
2. 增加精确 Docker logs/stats API action 和有界控制台路由。
3. 增加调度端口预留，并拒绝不完整 Ray 组。
4. 增加 Docker Agent release 滚动选择和真实主机验收测试。
5. 增加 Docker 集群管理 UI，展示 runtime、Engine 身份、节点库存、占用、能力、
   heartbeat、rollout 和阻塞原因。

## 已完成里程碑（2026-10-31）

第一步已作为按需开启的预览实现：单容器适配器/严格契约、伴生调度、mTLS 授权/
claim/status、精确 GPU UUID 注入、私有 journal/索引与取消屏障、root/systemd
安装包参数、终态结果与清理分离、重启恢复。Agent/安装包默认仍只观测，需分别开启
控制器 reservation、API execution 和 Agent execution。使用方式见
[运维指南](docker-agent-observation.md) 以及
`extensions/container-runtime/config/examples/docker-container-*.yaml` 模板。

测试覆盖模拟 daemon 故障/恢复、并发取消、跨进程锁争抢、记录丢失检测、官方 SDK
Unix socket，以及 mTLS 下的成功/失败/暂停和精确分配清理。它们不代表真实 Docker
GPU 验收。有界 Docker 诊断已实现为单独显式启用的预览；端口/gang 编排、滚动升级和控制台
Docker Job 提交仍为后续步骤。已有 Raw 测试及云验收范围保持原有含义。

## 有界诊断契约

Docker 诊断使用独立有界 broker 和私有 `/v1/docker-diagnostics` mTLS
poll/complete 传输，显式指定 `logs` 或 `stats` 类型。Raw 保留现有
`/v1/logs` 协议。
Docker 节点必须同时开启执行和 `--enable-docker-diagnostics=true`，并上报
`docker-diagnostics-v1`。仅观测 Agent 不可读取工作负载输出。安装包对应字段为
`enableDockerDiagnostics`。

管理员 GET 路由为 `/api/v1/container-runtime/logs` 与
`/api/v1/container-runtime/stats`，均只接受 `namespace`、`allocation`、
`allocationUID`、`taskUID` 四个参数。现有回环读取监听器和控制台 origin
仅服务这两个固定路由。每 Docker 节点同时一个请求，全局最多 32 个 Docker 请求，最多等待 20 秒；
返回前重新校验管理员凭据及精确 allocation/Task/Job/集群/节点/租户/credential
所有权。API 重启丢弃请求，可安全重试。已释放或 cleanup-confirmed 的分配、
Task generation 变化、凭据撤销和能力关闭均拒绝读取。禁止调度和暂停本身不撤销
保留所有权，但本地取消记录会停止 Docker 诊断。

本地读取前后均校验 journal 记录的精确容器 ID、Task generation、Engine
incarnation 和 labels；读取 Engine 响应正文时不持有执行锁。独立诊断 worker
每步最多 10 秒，不阻碍 reconcile。日志按 Engine 顺序合并 stdout/stderr，不
follow、不添加流标签，最多返回解码后 64 KiB，并限制 multiplex 帧处理量。
它是当前可用 Engine 日志预览，不是历史归档或 tail API；所有权不明、缺少记录
和响应格式错误都返回不可用。

资源快照提供 UTC 采样时间及可选的规范无符号 64 位十进制字符串：CPU 累计
纳秒、内存使用/上限字节、当前 PID 数、块设备读/写字节。缺失字段保持缺失，
不能补成零。stats JSON 正文最多 1 MiB。单次采样不提供 CPU 百分比；GPU
利用率和 host network 流量归属不可用。日志和快照均为临时数据，不写入资源
Status 或持久化指标。控制台提供手动读取，转义显示输出，并在切换选择或退出
登录后丢弃在途结果。
