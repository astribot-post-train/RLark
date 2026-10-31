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

控制器把 Job 展开为稳定 Task。普通 Docker Job 使用一个 Task 和一台主机的持久化
独占锁；Docker Ray Job 为每个副本展开一个 Task（一个 head，最多 16 个成员），先
计算完整落点再取得锁，并在 ContainerJob Status 持久化类型化整组计划。每个成员都有
不可变 ContainerAllocation，携带同一组身份、摘要、network scope 和端口块。只选择
Docker 节点，不写旧版 DockerBinding。

只有心跳、Engine ID、credential、目标 release、租户、Job/Task generation 和精确
allocation UID 均有效时才派发。组阶段为 `Allocating`、`Preparing`、`Starting`、
`Stopping`、`Succeeded`、`Failed`。Agent 先创建停止状态容器并写精确回执；全部回执
匹配前任何成员都不能启动。每次启动都会重新授权整组。准备容器消失后不能替换，
该成员会使整组进入清理。容器 Running 不等于 Ray 就绪，内置 launcher 会在 head
训练前验证精确私网地址集合。

内部端口块为 TCP 24000..24264（9 个 Ray 服务端口和 256 个 worker 端口）。create/
start 前检查 IPv4/IPv6 监听，但启动前释放检查 socket，因此不是内核级预留。NCCL/
训练端口仍由管理员网络策略提供。一台主机独占一个 RLark 组成员，因此同一 Cluster
中不同 Job 可在不重叠主机上并行。

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
5. 真实双机 Docker GPU/RLinf、整组失败/恢复、端口块和滚动升级在预览组生命周期
   稳定后单独验收。

## 本地主机取消屏障

执行开关要求 root 私有的持久化状态目录。通过独占进程锁与支持上下文取消的进程内串行化保护所有 Task UID 的 create/start、
stop/remove，开始清理前先 fsync 持久化取消记录。
旧 Agent、重复请求、进程重启或停止先于创建都不能重启该 Task。取消记录及追加式索引不随
allocation 释放删除；记录损坏、丢失或目录权限变化时停止执行并由管理员恢复。整个
目录丢失要求先隔离并退役旧节点，不能为旧凭据重新创建空 journal。
首个 carrier 使用 `docker.rlark.io/container` v1 的显式参数，拒绝用户设置
GPU UUID、受保留的所有权 labels、Pod 模板及未支持的 Raw Ray 字段；Docker Ray
整组契约单独携带内部组材料，allocation 独立注入 GPU UUID，Docker 使用 NVIDIA DeviceIDs。

## 交付顺序与剩余验收

1. 把 Docker admission 接入 scoped Agent Task loop，并增加 provider 生命周期/状态测试。
2. 增加精确 Docker logs/stats API action 和有界控制台路由。
3. 增加调度端口材料并拒绝不完整 Ray 组。整组屏障已在扩展实现；真实主机端口/GPU 验收仍未完成。
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
GPU 验收。有界 Docker 诊断和 Docker Ray 端口/整组编排已作为显式启用的预览实现；
滚动升级和控制台 Docker Job 提交仍为后续步骤。已有 Raw 测试及云验收范围保持原有含义。

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


## Ray/RLinf 实施契约（2026-10-31）

本节描述需显式开启的 Docker Ray 整组预览。scoped Agent 仅在 Docker 执行开启时
声明整组能力；只观测 Agent 不能派发。真实双机 Docker GPU/RLinf 验收仍是独立
门槛。改动保留在独立扩展内，不修改原生 Cluster/Node/Job/Task API。

### 端口与主机所有权

每个成员独占一台主机及其授权整卡集合。一台主机同时最多运行一个 RLark 成员，
同一个逻辑 Cluster 则可以让多个 Job 使用互不重叠的主机并行运行。host network
不意味着整个逻辑集群只能运行一个 Job；同主机混部和 GPU 切分不在本契约内。

用显式、版本化的端口块替换 head 名称哈希。每次执行只选择一次，并将同一计划
复制到所有成员的不可变 allocation 中。首版端口块包含九个服务端口和 1..256 个 worker
端口，覆盖 GCS、node manager、object manager、Ray client、dashboard HTTP、
dashboard agent HTTP/gRPC、runtime-environment agent 与 metrics；launcher 显式
固定所有服务及 worker 范围。非 Ray 的训练流量（如 NCCL 和用户 rendezvous 端口）
需要管理员配置私网策略；Ray 端口块不能当成完整的训练防火墙白名单。

Docker provider 在 create 前，以及启动已有 `created` 容器前，检查声明的全部
TCP 端口。端口清单是不可变执行材料，inspect/恢复时必须保留。检查 IPv4 和 IPv6
通配监听冲突，发现冲突即拒绝，不能终止或接管占用进程。检查 socket 必须在 Docker
启动前释放，因此它是预检查，**不是防止外部进程竞态的内核级端口预留**。专用主机
和管理员保留的端口块仍是前提；持久化整机 allocation 只保证 RLark 工作负载之间
互斥。恢复已运行的自有容器时，不能因其占用自身端口而失败。

### 副本展开与持久化组身份

Job 控制器消费 Docker carrier 中的模板副本数，每个副本生成一个原生 Task，且
head 只能有一个副本。规范子名称包含模板及副本索引；rank 固定为 head 在前，再按
模板名称和副本索引排序。创建 Task 前验证完整成员集合，首版最多 16 个成员。
混合 runtime、多个 head、用户指定 head 地址/rank/GPU/端口、未支持字段和所有权
歧义均在派发前拒绝。

持久化带独立身份的执行 operation，绑定精确 binding UID、原生 Job UID/generation
和重启身份，包含不可变成员 Task UID、节点/端口计划，不能用边沿触发的 `start`
布尔值代替。ContainerAllocation 继续作为与升级共享的每主机独占锁，Task target
同时保留 operation、成员和端口身份。API 没有跨对象事务，因此必须允许部分预留，
并在重启后重建。竞争 Job 不能无限累积部分锁；不完整尝试先持久化取消，所有已
暴露成员确认未执行/清理后才能释放。allocation create 响应丢失仍属不确定结果，
不能换主机重试。首版不承诺公平性、抢占或原子同时启动。

### 准备、启动与训练屏障

1. **已分配：** 每个精确成员都有当前 Job generation 的 allocation，租户、逻辑
   Cluster、network scope 一致，主机/IP/GPU 不重复，Agent 能力和认证心跳有效。
2. **已准备：** Agent 拉取/验证镜像、检查端口、创建容器但不启动。每个有界回执
   绑定 operation、Task UID/generation、allocation UID、Engine ID、容器 ID 和
   manifest digest；发布前先写入私有 journal。崩溃/响应丢失只恢复精确容器，不能
   接管同名容器或重建结果不确定的 create。
3. **允许启动：** 全体当前准备回执匹配后，API 才允许成员分别启动。每次 create/
   start 边界重新验证完整组及本地持久化取消状态。这是协调启动，不是分布式原子
   启动。Job 变更/暂停/删除或组所有权丢失会撤销后续启动授权。
4. **Ray 就绪：** head/worker 在工作负载镜像内运行可信 launcher。Ray 启动不能
   用前置 `--block` 阻断用户脚本；等待精确的预期私网节点地址集合后，head 才执行
   训练。worker 持续受监管，成员缺失或超时使启动失败。容器 `Running` 不等于 Ray
   就绪。

首版内部 launcher 要求 Ray CLI/SDK **2.56.1**、Python 3，以及运行显式用户脚本所需的 POSIX shell。
生成的命令使用 argv，保留参数字面量；只有 prepare/run 脚本进入 shell。不能意外
执行镜像默认命令。清理仅针对本容器进程树，不调用主机级 `ray stop`。prepare 在
子 shell 中执行，不能向 launcher 反向导出环境变量。run 与容器 command 互斥；
首版只在 head 执行训练。

### 失败、完成与恢复

停止兄弟成员前，先持久化整组 stopping/failure 观测。任何成员意外退出（包括
worker 以零退出）、无效回执、启动失败或显式停止都阻止后续启动。短暂 API/Engine
故障保留锁；心跳过期不能证明进程已停止或 GPU 已空闲。head 成功作为训练结果，
仍需有序停止 worker 并证明所有精确容器删除后，才能释放整组 allocation。保留
终态执行和 Task 身份，普通 reconcile 不能静默重跑已完成/失败的组。重启使用
显式新执行身份，必须等待旧成员清理证明齐全且 allocation 已观测不存在。

所有协议操作由 mTLS 节点身份及精确 operation/allocation/Task UID 限定；Agent
不持有管理 Kubernetes 凭据。Status 观测不能授权接管替换资源。Engine/节点/凭据
替换、journal 丢失、成员身份缺失或 create 不确定时，需要显式恢复，不能自动强制
释放。

### 交付与验收顺序

1. 替换未启用 Ray 编译器的哈希端口/shell launcher：增加显式类型化端口/分配输入、
   完整端口参数、受监管的就绪等待及字面 argv；实现 provider 预检查和恢复。测试
   冲突/占用端口、非法输入、取消、worker 失败与重启。这层运行时基础不声明 Ray
   派发能力。
2. 实现副本展开、不可变执行 operation、部分预留取消、精确准备回执和启动授权。
   测试两组竞争、过期 generation、create/回执响应丢失、控制器/Agent 重启、越权
   节点及容量复用前的清理。
3. 接入组失败传播、终态保留和 head/worker 就绪；增加完整成员 mTLS 集成测试，
   覆盖资源不足和外部端口监听。独立 Raw 语义保持不变。
4. 使用相同 Ray 版本和镜像，在两台真实私网 Docker GPU 主机验收：分布式 CUDA/
   RLinf 冒烟、logs/stats、停止、worker 丢失、Agent 重启、清理和第二个 Job 容量
   复用。Raw Notebook 和本地 CPU Ray 测试不能替代此门槛。
5. 在独立控制台展示类型化组阶段、成员落点、预留端口和阻塞原因，再增加提交/注册
   流程。Docker 空闲升级仍是单独验收门槛。


### 已交付的运行时基础

内部编译器现在要求可信类型化 Assignment：精确 Task UID/generation、有序私网
成员地址/rank、GPU UUID 和 PortPlan。空分配或身份不匹配直接拒绝，没有哈希/
名称/hostname 回退。只接受显式支持的 PodTemplate/container 子集；CPU 必须为
整数（已核对的 Ray CLI 只接受整数），GPU 请求必须匹配完整分配集合。脚本和
command 互斥、仅 head 运行训练，绝不继承镜像默认 entrypoint/command。

九个连续服务端口和 worker 范围写入不可变 manifest 的 `rayPorts`，由扩展专用
容器 label 保留，并受 journal digest 保护。公开单容器 Task 校验器拒绝该内部
字段。整组 allocation、Agent 协议和 `docker-ray-group-v1` capability 已接通；公开
单容器 Task 仍拒绝这些内部材料。

`ScopedProvider.Prepare` 记录/恢复精确 created 容器而不启动。后续 `Apply` 在
start 前重新检查端口和当前 admission。准备和清理共用取消 journal/锁；create
响应丢失、已取消身份和不可变端口漂移保持已有失败语义。恢复已运行容器时不检查
其自身占用的端口。create 前端口失败不产生创建结果不确定状态。双协议族检查
要求 IPv6 socket 支持；协议族不可用时拒绝继续。

Python launcher 将 `ray start --block` 作为受监管子进程启动，自身继续执行就绪
等待和训练。使用绑定版本的 GCS protobuf API，每次最多两秒、响应最多 1 MiB，
最多等待 120 秒，验证完整 alive 私网地址集合；就绪检查不创建 Ray driver。
可信私网仍需限制 Ray 访问，该地址检查不提供认证或网络隔离。launcher 保留
prepare/训练退出码，把任何 worker 自发退出视为失败，只向自身子进程组发送
TERM 再 KILL。Docker PID-1 退出会终止更改进程组的后代。此 launcher 用于
Docker PID namespace，不可当作宿主进程后端。

测试覆盖真实 IPv4/IPv6 监听冲突、prepare/重启/start/撤销、create 响应丢失、
取消、端口 label 漂移、严格分配/模板/资源校验，以及十个 Python 进程用例。
进程用例用 fake Ray/GCS 模块驱动真实子进程、信号、字面 argv、成员等待、超时
与清理，不代表真实 Ray 集群/GPU 验收。另在隔离 Python 环境核对了 Ray 2.56.1
CLI help 与全部生成参数。普通 Go 测试在有 `python3` 时调用进程套件，无解释器
时显式跳过该套件。其他 Ray 版本与真实多机训练仍未验收。


### 已实现：持久化 Ray 整组契约

扩展现在在 `ContainerJob.status.group` 持久化类型化整组快照，并在
`ContainerAllocation.spec.group` 保存不可变成员材料。快照绑定 binding、Job UID/
generation/重启标记、有序 Task UID/generation、节点/Engine/credential 身份、租户、
私网作用域和一个端口计划。创建子对象前会严格校验完整 Job。摘要只覆盖不可变
执行材料，可变观测保留在 Status 条件中。准备/启动准入、失败清理扇出和重启恢复已
通过限定 mTLS 协议接通。该功能仍是预览；真实双机 Docker GPU/RLinf 验收门槛尚未完成。
