# 容器集群扩展：设计与交付计划

状态：单节点 Raw 预览已部署，并于 2026-10-31 通过 GPU 生命周期和私有云空闲滚动升级验收。独立管理控制台现已提供预览版，详见[容器运行时管理控制台](container-runtime-console.md)。多节点 Ray/RLinf、原生 UI 集成、exec 失败回滚和断电恢复仍未完成。有界鉴权 Raw 日志传输已作为可选预览实现，并于 2026-10-31 通过私有云端到端验收。后续历史证据与当前扩展验收分开记录。

## 已确认架构与执行顺序

先编写并验证本中英文计划，再在现有功能分支和 PR 上实施，不重写已发布历史。新代码放入 `extensions/container-runtime/`，具有独立 Go 模块、API group、UI 包、二进制、测试和构建入口。复用原生 Cluster/Node/Job/Task 作为资源和工作负载模型，不向其目标 schema 添加容器专属字段。已有预览字段在安全迁移证明清理和所有权前仅作兼容数据。

原生 RLark 集成仅限通用的控制器所有权分派、依赖注入、服务/UI 路由和构建接线。容器调度、注册、资源观测、鉴权、版本分发和运行时行为归扩展所有。复用已导出的 Host/Coordinator/Provider/Adapter，不复制执行引擎。通用改动与 Docker/Raw 实现分离，并维护原生接入清单以便跟随上游升级。

### 类型化附属资源与所有权

扩展拥有 `containers.rlark.io/v1alpha1` API group。`ContainerCluster` 按稳定身份和 UID 关联一个原生 Cluster，记录运行时和升级策略。`ContainerNode` 关联原生 Node/Cluster UID，记录容器/Engine 实例、私网地址、能力、环境、授权 GPU UUID 和 Agent 版本。`ContainerAllocation` 保存 Job/Task UID、generation、租户、节点实例、执行标识、设备预留和清理进度。`ContainerJob` 是原生 Job 的类型化控制器绑定，不是第二套工作负载 API。`AgentRelease` 标识不可变产物、SHA-256 摘要和协议兼容范围。

`Cluster.spec.agentNamespace` 仍是成员关系权威来源。租户独立于 namespace 验证，并继承到附属资源。标签/注解只能检索，不能证明所有权。派发前验证类型化绑定、精确 Job UID 和租户；活动 Job 在 Task 或清理完成前不得换控制器。直接 Kubernetes API 和扩展 REST 使用同等准入校验。预览版拒绝 Kubernetes/容器混合 Job，原生 Kubernetes Job 行为保持不变。

对于每个由扩展拥有的原生 Task，扩展会在创建 `ContainerAllocation` 之前把一个规范化调度目标持久化到 `containers.rlark.io/allocation-target` 注解。注解包含 Task UID、原生 Node UID、ContainerNode UID、Cluster UID 和节点实例标识。它是持久化的调度意图，不是预留证明；真正的所有权锁仍由 allocation 提供。重试或第二个控制器只能复用完全相同的目标，在创建或派发结果不确定时绝不能换节点。只有在 allocation 具有匹配的清理证明且 Task 正在以新 UID 替换时，才能清除目标。Task 删除、暂停和配置替换在 Agent 报告清理前都保留目标及 allocation 身份。生命周期从当前状态持续维护：`suspended` 和删除会阻止新启动，心跳和 Agent 结果只是观测，不能单独释放锁。

Raw supervisor 即使尚未启动该 Task，也会在停止时持久化一个仅含身份的取消墓碑。同一 Task UID 的延迟启动必须被拒绝，包括 supervisor 重启后。已有但不确定的进程不能走“未启动”路径，必须在确认清理前保留分配。取消记录持久化失败会返回错误，不能上报清理完成。此预览版保留墓碑，不自动回收。

需显式启用的预览 Job 预留控制器会把 Job 中有界的 `raw.rlark.io/process` v1 模板（1 至 16 个、名称唯一）分别展开并预留为独立原生 Task。在各自派发契约实现前，拒绝 Ray、旧版绑定、显式运行时实例名称、SSH 注入、用户指定的 GPU 可见性环境变量、环境处理器和 Pod 模板。原生 Task 名称确定，严格校验 owner reference、父 UID、模板身份和租户，不接管旧格式数据。节点选择器先校验，再匹配原生 Node 标签。工作负载或重启变更在确认清理后以新 UID 替换 Task，不就地修改运行中的执行。暂停/删除保留 Job 和 Task finalizer，直到每个精确 allocation 清理完成且观察到删除。目标已固定但 allocation 不存在时，除非控制器已保存精确清理回执，否则继续阻塞：仅凭不存在不能排除仍在进行的创建或启动。释放前，控制器在 Task 的 `containers.rlark.io/allocation-cleanup-receipt` 中持久化 allocation UID 和完整目标身份，以便从删除响应丢失中恢复。孤儿 allocation 阻止 Job 删除；含糊数据需要运维显式恢复。调度决策使用非缓存 API client 和乐观并发。控制器现在聚合受支持的独立 Raw Task：当前 `ExecutionKnown=True` 观测显示 Pending/Running，只有当前 Succeeded/Failed 且 `ExecutionCleaned=True`、精确清理证明和 allocation 删除均已观察到后，才发布 Job 终态。`ExecutionObserved` 和 `AllocationReleased` 条件分别展示观测和清理等待。完成的 Task 身份持续受保护，控制器或 Agent 重启不会重复运行；Job 暂停/删除、模板变更和显式重启才会通过既有清理流程退役它。授权日志传输已作为可选预览实现。Ray/RLinf barrier、自动注册/续签和原生 UI 仍未完成；运行时无关的多模板展开已实现，但尚未提供跨成员协调。下文说明已实现的管理员显式接入和按需 Agent 升级；私有云滚动升级验收记录在后文。

独立控制器默认在 `rlark-system` 中启用 Kubernetes Lease 选主（`--leader-elect`、`--leader-election-namespace`）。使用 `config/rbac/container-runtime-controller.yaml`，并配置对应 namespace 和管理 API kubeconfig；namespace 必须事先存在。默认 `--enable-job-reservations=false` 仅安装校验控制器；隔离的预览测试可通过 `--enable-job-reservations=true` 启用预留。请用 `Job.spec.suspended` 暂停整个 Job；此阶段拒绝单独暂停的模板。可选进程内 `ControllerExtension()` 仍仅执行校验。同一节点不能运行旧预览分配写入者。撤销权限或关闭控制器不会清理或释放持久化预留；重新启动后继续调谐。预留测试覆盖恢复、容量不可用、配置变更、删除、租户冲突和 API 响应丢失，不代表 Agent 执行或端到端训练已支持。

控制器拥有分配意图和派发，Agent 拥有运行时观测与清理证据。派发前用乐观并发持久化预留，重启从当前状态恢复，心跳丢失不释放。每个 Raw 容器或 Docker 主机一次只接收一个 Task，当前分配契约要求显式的授权 GPU 白名单。GPU 只使用已授权整卡 UUID，可见性不等于授权。清理未知阻止复用，日志/遥测/审计不写入资源 Status。

### 独立服务与运维界面

已实现的执行预览为独立扩展 API 增加需显式启用的节点限定 Task 通道。经过验证的 Agent 证书是唯一请求身份；读取、元数据认领、状态报告、启动授权和清理报告都重新校验当前凭据、companion/原生 UID 引用、独立租户身份和精确 allocation。Agent 只收到自己的 allocation 和 Task，不持有原生 kubeconfig，也不能执行 discovery、watch、Secret、代理或任意资源操作。Task 认领写入限定为 Host 拥有的注解/finalizer，并要求调用者观测的 resourceVersion；状态写入保留控制器字段，必须匹配当前 Task UID 和 generation。运行时副作用仍通过现有 Host/Coordinator/Raw Provider 和私有 supervisor。通用 Host 的元数据依赖已收窄到实际使用的 Get/Patch 方法，使扩展可注入真实的限定传输，不实现全功能 Kubernetes client。执行 API 仅校验已有 allocation，不创建锁或修复分配目标。Task spec 按 JSON 语义比较，保留精确数值并容忍 API 调整对象字段顺序。

每次进程启动前，API 校验 Job/Task 当前未暂停、generation 与分配一致、唯一的类型化 Job/Cluster 绑定及有效凭据。节点替换、旧预留、禁止调度/排空、心跳过期、版本不匹配或选择器变更均阻止启动。decoder 仅从不可变 allocation 的授权 GPU UUID 注入 `CUDA_VISIBLE_DEVICES`，拒绝用户覆盖 GPU 可见性。清理使用独立身份路径：暂停、删除和心跳过期阻止启动，但不能阻止精确拥有节点的 Agent 报告清理。即使 Task 从未启动，也必须先由 supervisor 持久化取消，再接受清理证明。supervisor 不可用或后代未确认退出都不能推断清理完成。API 暂时故障会从当前状态重试，不因此停止运行中的可信工作负载或释放分配。每个成员仍是独立进程执行；Ray 整组、自动注册/续签、日志、云端升级和 UI 保持各自验收门槛。

构建独立扩展控制器/API、节点 Agent、supervisor 和管理工具。节点显式加入指定容器 Cluster，使用有限 bootstrap 凭证注册，再用节点级认证主动出站；不需要公网 IP 或控制面入站。Raw 在已有容器内执行可信 root 进程，Engine 使用宿主机 Docker socket，绝不 Docker-in-Docker；两类能力分别展示。

提供独立构建的“容器集群”页面，并只向 RLark 接入最小导航入口。展示集群身份/运行时/连接、节点 Ready/可调度/占用、容量/分配/利用率、授权 GPU、预装环境、Job、Agent 版本和升级失败。管理员可创建/加入、生成接入包、禁止调度/排空和发布目标版本；Job 显式选择 Cluster，服务端校验能力。

控制面托管不可变 Agent 产物。节点认证下载，校验摘要、平台和协议，原子切换并保留旧版本。目标版本是持续期望状态，每个 Cluster 同时最多升级一个空闲节点；切换前阻止新分配，健康检查失败回滚。Agent 升级不重启 workload supervisor。固定主机支持 systemd，无 systemd 开发机使用前台 root launcher/updater。

### 有界远程 Raw 日志读取

下一项新增预览提供针对精确活动 allocation 的按需日志快照。管理员请求携带 allocation 的 namespace/name/UID 和 Task UID。API 在排队前与返回日志前重新验证原生/附属 Node、Cluster、租户、Job 绑定及 allocation 所有权。节点通过现有出站 mTLS 连接轮询，不新增节点入站监听器或 Docker socket。禁止调度、暂停或就绪过期本身不撤销精确日志拥有者；凭据撤销、对象替换、allocation 释放或含糊所有权会拒绝读取。

请求是临时只读操作，只保留在 API 进程内，最长 20 秒；每个节点最多一个、全局最多 32 个未完成请求。API 重启会丢失请求，管理员重新读取即可。日志不写入 CRD Status、Event、审计负载或组件诊断。supervisor 只接受已记录的精确 Task 身份，读取不跟随软链接的私有普通日志文件，最多返回保留的 stdout/stderr 合并日志开头 64 KiB。文件仍受配置的保留上限约束。截断标记表示还有未返回的保留字节，达到保留上限标记只表示文件已达到配置上限，不代表已确认丢弃输出。

管理员读取监听器须显式启用在回环地址，独立验证可信 Gateway 的管理员 JWT，独立控制台通过固定只读路由代理。Agent 接口仍必须使用 mTLS。工作负载可能在日志中输出敏感信息，因此只向管理员提供，按纯文本显示，不执行 HTML 或终端控制序列。allocation 释放后的历史日志、归档、tail/since/follow、资源指标和 terminal/exec 仍不支持。远程日志验收需通过本地传输、撤销/替换/竞争、请求限量与恢复测试，以及显式启用的真实 GPU Job 测试。

### 迁移与验收门槛

首个独立控制面阶段使用通用 Job 控制器声明（`rlark.io/controller-owner`），并以类型化 ContainerJob 绑定校验。即使扩展不在线，原生控制器也会对声明外部主人的 Job 拒绝派发。校验控制器检查 UID 与权威 namespace 引用；它不授权工作负载副作用，也不把已登记节点标为可调度。AgentRelease 元数据通过校验不代表产物可用。等价准入安装前，扩展 CRD 仅允许可信管理员写入。在分配器、节点限定 Agent 通道和清理调谐完成前，不把活动工作负载切换到附属 API。

扩展新增了基于 API 的 Node 独占分配原语。它用原生 Node UID 派生的确定性 ContainerAllocation 名称创建锁，并校验权威成员关系、租户、Node/Job/Task UID、Agent generation/就绪、心跳时效、GPU 白名单、私网地址、重复 GPU/地址范围以及原生旧 reservation 是否存在。重复 Acquire 只恢复完全相同 Task 的分配；并发竞争者在 API 创建冲突时失败。Release 必须有针对精确 allocation UID 的 Status 清理证明，并在观察到删除前保留 finalizer。独立 mTLS 服务的 `POST /v1/heartbeat` 仅接受已验证且携带容器 Agent 角色的证书；它会校验签名的 companion UID、租户、凭据、原生绑定、私网地址和授权 GPU 可见性，然后更新有界 ContainerNode 状态。Linux 前台 Agent 二进制现在观测当前 cgroup v2 边界、可见 GPU UUID 和私有 Raw supervisor 占用，并发送显式 `busy` 字段；观测或 API 暂时不可用时使用有界退避重试，supervisor 或 GPU 观测失败时拒绝上报，因此最终表现为心跳过期和调度屏蔽。周期修复保留新鲜 Agent 观测并屏蔽过期心跳；分配检查当前凭据身份和节点忙碌状态。API 二进制和最小权限 `config/rbac/container-runtime-api.yaml` 已提供。管理员显式登记、离线证书/安装包导出、验证后的版本分发和单进程 Job 生命周期已作为显式启用的预览实现，详见下文。自动注册/续签和Ray/RLinf 多节点 Job 生命周期仍未完成；独立 Raw 模板展开已完成。TLS 部署、CA 分发和证书签发/轮换仍由运维方管理。

先复制并测试现有 Docker/Raw Provider、分配、supervision 和测试到独立模块，再以附属资源替换预览绑定/预留，更新 decoder/准入并建立通用 Job 分派。新旧分配写入者不能同时运行。迁移时禁止旧节点调度，等待确认清理，按 UID 建立附属资源，验证后启用新主人；含糊活动数据保持阻塞并展示。

首轮云端验收是一个预置 Raw 节点和一个 GPU 进程 Job：显式选 Cluster、注册、调度、GPU 计算、有界授权日志、成功/失败/停止、后代确认清理、checkpoint 保留及 Agent 升级/回滚。多节点 Raw Ray/RLinf、跨运行时 Job、终端/exec、动态存储和云自动扩容留待后续。Docker 生命周期保持已有验证，但多节点 GPU、logs/stats、GPU UUID 分配必须单独验收后才能宣传。

### 金山云测试开发机

用户指定资源池 `GPU-Pool-Regular-Training` 的 `regular-training` 队列，没有选择 `regular-dev`。公司指南通常要求 0/1 GPU 开发机走 Dev 队列，因此记录为用户明确指定的例外，不静默换队列。创建前重新核对权限、容量、镜像和存储。区域 `cn-northwest-3`，1 张 GM402 GPU、20 CPU、236 GiB，队列可见并自动保存；不分配公网 SSH/EIP 或公网 dashboard。

参考开发机 `rl_dashboard`（`kaic-08ab1432-4d49-4a97-b0d5-f30b00b24113`，SSH `root@36.103.180.181:22`）。复用已授权保存镜像和 `/ks3-regular`、`/kpfs-regular` 挂载。保存镜像不复制未保存改动，也不保证有 `/root/fleet/venv`。不隐式保存/修改源机器，不复制个人文件/密钥，不覆盖 autosave 标签。只安装用户授权公钥，并实时读取新机私网地址和 GPU 授权。

注册前验证 cgroup 配额、平台 GPU 授权和 UUID、Python prefix/依赖、状态/日志目录、存储权限及出站控制面连接。源机器没有 Docker socket 和可用 systemd，首测使用明确的可信 root 进程树后端。缺环境或网络则阻塞 GPU 验收，不静默装依赖、申请 EIP，也不以枚举 GPU 代替计算验收。

2026-10-30 创建的测试开发机 `kaic-6e552a55-4108-4d14-b719-e8a392e193d9` 最初因源机 autosave tag 返回仓库 `NotFound` 而拉镜像失败。经用户同意，停止原机、换为可用的个人镜像 `rlinf-dashboard:1.1`（`0f71ac0b-6dfe-43ce-9e6f-f2fb94b26874`）后重启。当前运行于私网 Pod IP `10.0.3.85`，位于 `regular-training` 队列，配置 1 GPU、20 CPU、236 GiB，无公网 SSH/EIP。参考开发机仅作为 SSH 跳板，没有复制其文件或环境。

默认 Python 是 3.11.11，含 PyTorch 2.6.0+cu124，不含 Ray/RLinf；继续检查发现以下可用的预装环境，本轮没有安装依赖：

| Python 入口 | 观测到的依赖与验证 |
| --- | --- |
| `/root/RLinf/.venv-pi05/bin/python` | Ray 2.56.1、PyTorch 2.11.0+cu126；实际从 `/root/RLinf/rlinf` 导入源码，其 `pyproject.toml` 声明版本 0.4.0（没有已安装分发元数据）。CUDA 张量运算返回 `42.0`。实际 Ray 任务使用 1 CPU、0 GPU，返回 `{"value": 42}`；shutdown 成功，后续未发现 Ray raylet/GCS 进程。 |
| `/root/RLinf/.venv-lumo2/bin/python` | Ray 2.58.0、PyTorch 2.10.0，可发现 RLinf 源码；没有已安装 RLinf 分发元数据。CUDA 张量运算返回 `42.0`。未在该环境运行 Ray 任务。 |
| `/opt/rl_dashboard_bundle/env/bin/python` | Ray 2.56.1、PyTorch 2.7.0+cu128，不含 RLinf 模块。CUDA 张量运算返回 `42.0`。 |

`/root/RLinf` 下两个 venv 的 Python 入口均解析到 `/opt/conda/bin/python3.11`，接入时需保留并验证 venv prefix；supervisor 的规范可执行路径策略本身不能保留该 prefix。上述检查只证明导入和小规模计算，不代表 RLinf 训练或具体工作负载依赖兼容性通过。`/root/fleet/venv` 仍不存在，替代镜像也无法证明包含源机后续环境改动。

实际 cgroup 配额为 20 CPU（`cpu.max = 2000000 100000`）和 236 GiB（`memory.max = 253403070464`）。虽然存在 `cgroup.kill`，cgroup v2 文件系统为只读，不能使用 cgroup supervisor 后端。没有 Docker socket 或可用 systemd。初始预检时状态/工作目录尚未安装，父目录可写；下文验收部署已创建私有状态目录和工作目录。可信 root 进程树后端必须通过自身预检与生命周期测试后再使用，它不提供任务隔离。`/ks3-regular` 在平台本地目录上覆盖了实际读写 `fuse.ks3fs` 挂载，`/kpfs-regular` 为读写 `kingstorage` NAS 挂载；尚未测试跨重启 checkpoint 保留。

资源池清单显示分配一张 GM402，但容器内 `nvidia-smi` 显示一张 H100 80GB；在把 GPU UUID 作为授权容量前需确认平台型号映射。已通过私有跳转路径在该开发机上以 root 运行可选 Agent 观测测试：process-tree supervisor socket、cgroup v2 限制（`20000m`、`253403070464` 字节）和一个 GPU UUID 均成功读到；关闭 supervisor 后观测会 fail closed。这只验证本地观测边界。该预检阶段开发机尚未登记为扩展 Agent；下文后续单节点验收已覆盖显式登记和 GPU Job 派发，授权日志及真实 Agent 升级/回滚仍为独立门槛。本轮未启动 RLinf 训练。

2026-10-30 稍后查询发现开发机已停止，自动保存成功。本轮重新启动时未变更队列、资源规格或公网配置。私网 Pod IP 仍为 `10.0.3.85`，宿主 Node IP 变为 `10.0.2.72`，因此旧宿主地址不能标识执行身份。重启后预装 pi05 Python 仍报告 Ray 2.56.1、PyTorch 2.11.0+cu126。以 root 运行的 supervisor 定向测试通过：启动前取消、取消持久化失败及重试、无效/取消请求，以及真实 supervisor close/open 后恢复取消记录。已移除临时测试二进制。这些测试未启动训练进程，不证明 checkpoint 保留，也未注册 Agent 或派发 Job。

2026-10-31 只读复查平台：同一开发机当前已停止，自动保存成功，已引用更新的个人
自动保存镜像。队列、1 GPU、20 CPU、236 GiB 和仅私网 SSH 配置保留。
此前私网 IP 不代表容器仍运行或旧节点实例可以复用。再次云端验收前，需读取当前
平台状态，并按文档中精确身份恢复流程退役旧节点、重新登记替换实例；不要因为旧
基础镜像曾获批准，就把已保存环境重置回旧镜像。

### 前台 Raw 观测 Agent（preview）

独立 API 使用自身的参数集，避免依赖包全局参数冲突；`--help` 无需 TLS 或管理凭据即可正常退出。未知参数和位置参数会被拒绝。应用 RBAC 前，先创建 `rlark-system`（或配置的选主命名空间）。控制器的命名空间 Role 允许访问 Lease，并创建或 patch 选主 Event；API 没有 Event 写权限。

`make -C extensions/container-runtime build` 在 API/controller 和 `raw-supervisor` 之外构建仅支持 Linux 的 `container-runtime-agent`。该命令默认只上报观测，显式开关可启用下文的限定 Raw 进程执行。它不注册节点、不签发证书或传输日志；升级安装由独立的按需开关和流程控制。先创建并验证原生/companion 绑定，独立安装可信 root supervisor，并由运维配置专用 Agent 证书：签名元数据必须包含精确 ContainerNode namespace/name/UID、原生 Cluster UID、容器实例、租户及当前 `containers.rlark.io/agent-credential` 注解。禁止向 Agent 签发原生管理员证书。专用 CA 私钥保留在 API/Agent 之外，API listener 仅配置 CA 证书。这些步骤仍由运维管理；下文管理员工具可自动完成显式登记和离线安装包导出，尚无自动注册/续签。

运维配置好文件并运行 supervisor 后，支持的观测命令为：

```bash
./extensions/container-runtime/bin/container-runtime-agent \
  --endpoint=https://container-api.example.internal:9444 \
  --tls-cert=/private/rlark/agent.crt \
  --tls-key=/private/rlark/agent.key \
  --server-ca=/private/rlark/server-ca.crt \
  --version=preview-v1 \
  --private-address=10.0.3.85 \
  --supervisor-socket=/var/lib/rlark-supervisor/supervisor.sock
```

端点、身份、版本和路径需替换为真实配置。端点必须是 HTTPS origin，禁止重定向；要求 TLS 1.3 和服务端 CA 校验。独立 API 使用 `--tls-cert`、`--tls-key`、`--agent-ca`、`--kubeconfig` 和 `--addr`（默认 `:9444`），最小权限位于 `extensions/container-runtime/config/rbac/container-runtime-api.yaml`。API 对每次报告执行认证和授权，拒绝未知 JSON 字段或缺失 `busy` 的观测，心跳时间使用服务端时间。证书/凭据轮换后需以新文件重启 Agent；尚未实现自动重载/续签。撤销凭据注解会阻止新报告和分配，但不释放已有 allocation。

默认成功报告间隔为 15 秒（`--interval`，最大 15 秒），故障时指数退避并增加抖动，默认上限两分钟（`--retry-max`，最大五分钟），本地观测期限为十秒。每次重试都重新读取状态。私有 supervisor Unix socket 的 `POST /v1/status` 不接受请求体，仅返回占用；活动或持久化状态不确定的执行均为忙碌，supervisor 已关闭、缺失或协议不兼容时禁止上报。GPU 查询失败同样禁止上报，不会默认空闲或空 GPU。心跳丢失、探测失败均不证明工作负载清理完成。allocator 独立拒绝超过 45 秒的报告；状态每 30 秒修复一次，因此界面 Ready 条件可能比截止时间晚一个修复周期。

CPU 观测取进程 affinity 与当前 cgroup 到 cgroup-v2 挂载点之间所有可读祖先 `cpu.max` 的最小限制；内存取可见层级中最小有限 `memory.max`，全为无限时不报告内存。`--cgroup-mount` 默认 `/sys/fs/cgroup`，经 `/proc/self/cgroup` 解析当前成员路径，拒绝非 v2、缺失或格式错误的边界。它们是可见上限，不代表利用率、空闲量、隐藏祖先限制的保证或任务级限额。可见 GPU 不改变运维授权的 UUID 白名单。前台默认模式仍是 Raw；`--runtime=docker` 现支持无需 supervisor 的 [Docker 主机观测](docker-agent-observation.md)。限定执行尚未实现前，Docker 节点保持不可调度。

本地测试覆盖观测失败、API 故障恢复且不重放旧观测、取消、严格占用响应、祖先配额、无限/无效限制、重复 GPU 输出、mTLS 授权及重定向拒绝。在准备好的可信 root GPU 容器上，可选 `RLARK_TEST_LOCAL_OBSERVATION=1` 测试会运行真实 supervisor socket、cgroup 和 GPU 探测，不启动工作负载。这些检查不代表生产调度门槛完成。

2026-10-30，同一私网开发机通过 `TestScopedExecutionRealSupervisor`：真实 mTLS 请求、Host/Coordinator、私有 supervisor Unix 传输、可信 root 进程启动、Agent/Host 重建、allocation GPU 环境注入及暂停/清理证明。管理 API 为测试替身，工作负载只使用 CPU；注入的 `GPU-a` 是测试标记，不代表授权设备。开发机仍为 `regular-training`、1 GPU、20 CPU、236 GiB、无公网 IP。临时二进制已移除。这是执行组件证据，不代表已接入的生产 Job/GPU 验收。

### 限定 Raw 执行预览

运维手动配置精确原生/companion 绑定、专用节点证书及私有 supervisor 后，在 API 和 Agent 命令上**同时**加入 `--enable-task-execution=true`，控制器仍需 `--enable-job-reservations=true`。在 API 基础 RBAC 之外应用 `extensions/container-runtime/config/rbac/container-runtime-api-execution.yaml`；新增角色只允许原生 Job/Task 读取、Host Task 元数据/状态补丁、companion 绑定/allocation 读取和 allocation 清理状态补丁，不授予 allocation 创建/删除、Secret、exec、签名或 impersonation 权限。执行路由默认不存在，仅观测不需要额外 RBAC。

版本化预览 `POST /v1/task` 通道仅允许针对证书绑定的 allocation 和 Task UID 执行 `assignment`、`get`、`authorize`、`claim`、`status`、`cleanup`。严格且有界的 JSON 拒绝任意资源和字段。认领补丁先在内存中与 Host 允许的精确元数据比对，再由服务端重建带乐观锁的写入。类型化状态仅修改当前 Task 实例的 Host 字段/条件。Agent 不持有管理面 kubeconfig。启动前 API 检查当前 Job 模板、重启标记、凭据、心跳、版本、禁止调度/排空和保留的 allocation；命令或归属变化均拒绝。

暂停/删除是持久化清理请求。Agent 先调用私有 supervisor 的幂等 Stop，包括尚未启动的情况，验证精确身份、持久化取消和后代清理后再报告。API 先移除 Host 拥有的认领元数据，再发布 allocation 清理证明；锁释放和 Task 分配保护归 Job 控制器。认领/清理响应丢失后从当前状态恢复。本地观测失败时只协调清理，不发送伪造心跳或启动任务。API 故障保留工作负载和 allocation。撤销/替换的凭据不能报告清理；需恢复精确所有者身份或走独立可审计的运维恢复，不能直接删除保护。

测试通过真实 mTLS 传输搭配假管理面和 supervisor，覆盖跨节点/租户/UID、旧 generation/心跳/版本、伪造字段/状态、Host 重建、GPU 注入、认领提交后响应丢失、部分清理/响应丢失，以及观测失败时的清理。这些组件测试不代表已部署的云端 Job 验收；下文后续真实集群测试覆盖显式登记和单 GPU 进程。Ray 组、授权日志、自动注册/续签、云端升级、UI 和完整 GPU 训练验收仍属后续工作；运行时无关的多模板调度由控制器测试覆盖。

### 已实现的执行生命周期增量

已实现的预览聚合独立调度的 Raw 成员 Task。Running 需要当前 Task `observedGeneration` 和 `ExecutionKnown=True`；终态结果还需要当前 `ExecutionCleaned=True`。仅有终态 phase、主进程退出、心跳过期或 supervisor 记录缺失均不能作为释放证明。上报当前成功/失败结果后，限定 API 拒绝新启动和冲突的状态回退。Agent 必须先持久化 supervisor Stop/取消，再由已有 cleanup 端点发布 allocation 清理证明。本地观测不新鲜时也必须能执行这一清理路径。

控制器校验精确 allocation 目标/UID/所有者/租户，等待 Agent 清理认领，持久化已有清理回执，并观察 allocation 删除完成后才发布 Job 成功/失败。清理等待状态与观测到的 Task 结果分别展示。完成的原生 Task、目标及 allocation 保护 finalizer 保留为当前结果身份，即使有人请求删除该 Task；仅 Job 暂停/删除、模板变更或显式重启标记才退役它。控制器/API/Agent 重启后不能再次分配或运行同一个 Task。执行变更沿用已有清理流程退役旧 Task，并重置 Job 时间戳。时间戳代表控制面观测时刻，不代表精确进程启动/退出时间。不需要修改 CRD 或原生生命周期。

测试覆盖成功/失败及运行聚合、过期/缺失条件、精确清理证明、延迟删除、回执/删除/状态响应丢失、控制器重建、不重跑/不重分配、完成后 Job 暂停/删除/重启，以及第二个 Job 获取已释放节点。真实 supervisor 测试证明终态清理会阻止释放后延迟到达的 Ensure。2026-10-30，私网金山云开发机通过 `TestScopedTerminalExecutionRealSupervisor`，使用 CPU 进程、真实 mTLS/Unix 传输和假管理面，观察到 Job 成功及 allocation 删除，再拒绝延迟 Ensure。该组件测试本身不代表已接入的云端 GPU Job 验收，后续真实集群结果见下文。这仍为单进程预览；日志、自动注册/续签、云端滚动升级和 UI 仍为独立门槛。

### 管理员显式接入（预览）

已实现独立 `container-runtime-admin` 命令，使用严格 JSON 配置执行 `cluster-create`、`node-register`、`node-package` 三个显式操作。管理 kubeconfig 仅由接入机器上的管理员使用，不下发给 Agent。创建集群会新建原生 Cluster、专用 namespace 及按 UID 固定的 ContainerCluster。登记节点会在选定集群中创建原生 Raw Node 和 ContainerNode，显式配置容器实例、私网地址、预装环境、网络范围及平台授权 GPU UUID。每次创建都带调用者保留的 UUID 和规范请求摘要，创建响应丢失后通过实时读取恢复，并拒绝修改部分创建的意图；既有资源若操作身份、租户或期望内容不符则报冲突，绝不接管。部分创建保留可见供重试。登记不设置 Ready，也不解除禁止调度。

导出包要求精确 ContainerNode UID，重新校验原生及附属 Cluster/Node 实例和租户，并由管理员独立 Agent CA 签发有期限、仅 client-auth 的证书。不修改或轮换当前凭据。仅写入全新的私有目录，包含节点证书/私钥、API 服务端 CA、不可变身份清单、Agent 启动器、独立 supervisor 启动器及可选 systemd 单元，不含 CA 签名私钥、管理 kubeconfig 或复制的用户环境。导出重试可为同一当前凭据签发另一张证书；旧/替换凭据必须拒绝。管理员私下传输并分别启动 supervisor；停止或更新 Agent 不得停止 supervisor。自动注册/续签仍属后续工作；空闲节点滚动选择为下文单独启用的预览功能。

首个安装包支持已选定的 Linux 可信 root process-tree Raw 模式，要求明确的规范可执行/工作/状态/安装路径、Linux Agent/supervisor 文件及 SHA-256 验证，不从可见性推断 GPU 授权，也不安装 Python 依赖。故障测试覆盖外来资源、跨租户/namespace、替换 UID、部分创建/响应丢失、无效 CA/期限、输出私有权限和启动参数转义。真实 mTLS 测试验证签发证书可认证登记节点、凭据轮换后被拒绝。此管理员路径不代表云端已部署接入。Docker 安装包模式现支持连接本地 Unix socket 的只读主机观测；Docker 执行、自动续签和安装编排仍属后续工作。

执行前先安装扩展 CRD、启动独立校验控制器/API，并准备专用 Agent 签名 CA，以及 SAN 匹配私网 API origin 的 HTTPS 服务端证书。接入身份需要原生 Cluster/Node 和附属资源的 list/get/create 权限及 namespace 的 get/create 权限。这是可信管理员路径，不是租户自助 API。签名私钥必须是无 group/other 权限的普通文件。证书期限为 1–720 小时，不能超过签发 CA 的有效期。开发机必须能访问该私网 API，Agent 无需公网地址。

仓库中的 `extensions/container-runtime/config/examples/` 只是模板：从管理面和平台重新读取后替换全部 `REPLACE_...` 值。以下命令都在接入机上使用管理员 kubeconfig 执行；若响应丢失，必须保留并重用同一个 operation UUID：

```bash
./extensions/container-runtime/bin/container-runtime-admin cluster-create \
  --kubeconfig=/private/admin/kubeconfig \
  --config=extensions/container-runtime/config/examples/provision-cluster.json
./extensions/container-runtime/bin/container-runtime-admin node-register \
  --kubeconfig=/private/admin/kubeconfig \
  --config=extensions/container-runtime/config/examples/provision-node.json
./extensions/container-runtime/bin/container-runtime-admin node-package \
  --kubeconfig=/private/admin/kubeconfig \
  --config=extensions/container-runtime/config/examples/provision-package.json
```

把集群结果中的 `spec.cluster.uid` 填入 `clusterUID`，把 ContainerNode 结果中的 `metadata.uid` 填入 `node.uid`；二者属于不同资源实例。`credentialID` 使用当前凭据注解，每个新的创建意图使用新的 operation UUID。Notebook ID 或私网 IP 都不能证明当前容器实例或 GPU 授权。安装包示例默认仅观测（`enableExecution: false`）；启用执行还需要上文 API/controller 的开关及支持的 Job 契约。

将导出的目录私下传到配置的 `installDirectory`，保留目录/可执行文件 0700、数据文件 0600 权限，先执行 `sha256sum -c SHA256SUMS`，再分成两个独立进程启动：先运行 `start-supervisor`，再运行 `start-agent`。示例使用已准备好的 `/opt/conda/bin/python3.11`，不会安装 Ray、RLinf 或 CUDA 依赖。命令创建原生/附属资源和离线目录，不生成压缩包，也不远程安装。启动前先在目标上创建并验证工作目录，工作路径不能与私有安装/状态路径重叠。生成的 systemd 单元可选且不会自动安装，Notebook 使用前台启动器。部分导出会保留 `INCOMPLETE` 并拒绝启动，管理员检查后应导出到新目录，不得直接删除标记绕过失败。登记/导出不会把节点标记为 Ready，也不会派发 Job。写入前后均重新验证凭据和节点期望配置；SHA-256 检测文件损坏，不构成对恶意可信 root 任务的安全边界。仅更新 Agent 时不要重启 supervisor。

本增量本地验证：仓库 `make test-go`、接入/CLI 定向竞态测试、独立模块 `vet`/构建、定向 golangci-lint（零问题）及严格双语 MkDocs 构建通过。仓库 `make lint-go` 仍被原生 Docker/网络及相关预览代码中 30 个已有问题阻塞。本接入增量未执行云端登记或 GPU Job。

### 认证 Agent 制品（预览）

独立模块为 API 和控制器提供可选的 `--artifact-directory`。二者必须挂载同一份由运维管理、只读的制品目录，使用规范绝对路径。AgentRelease Spec 保持不可变；`spec.artifactPath` 是目录内的规范相对路径。拒绝绝对路径、目录逃逸、符号链接、非普通文件、不兼容平台/协议、超过 256 MiB 的文件和 SHA-256 不匹配。控制器周期检查实际字节后才能设置当前 generation 的 `Available=True`；文件丢失或损坏撤销可用性，但不释放 allocation。未配置制品目录的控制器保持不可用。

预览 `POST /v1/release` 仅在配置制品目录时提供。经过验证的节点证书从权威 ContainerCluster 选择当前目标，body 不能指定其他 namespace、租户、版本记录或文件路径。严格 JSON 接受携带 Agent OS/architecture 的 `action: manifest`，或额外携带此前返回的 release UID 与 SHA-256 的 `action: download`。清单仅返回版本身份、版本号、平台、协议、摘要和字节数，不泄漏服务端文件路径。没有目标时返回 HTTP 204；下载只返回经过核验的精确字节。制品 I/O 后重新读取节点凭据、原生/附属绑定及目标身份；凭据轮换、删除、目标替换、租户不符或平台不兼容均拒绝。心跳过期或工作负载占用不阻止下载，但不能授权安装或释放预留。

Agent client 复用 TLS 1.3 通道，拒绝重定向并限制下载大小；返回给后续 updater 前再次验证清单、字节数、SHA-256 和可执行平台。该客户端边界不选择升级节点、不续签凭据，也不重启 supervisor；下文按需启用的节点更新器已可校验并安装精确二进制。独立开关启用后可逐个选择心跳新鲜的空闲节点；外部 supervisor 恢复仍是独立门槛。制品和 AgentRelease 由可信管理员发布，不新增无认证静态文件服务或上传端点。测试覆盖真实 mTLS、跨租户/凭据拒绝、目标/UID 替换、摘要损坏、文件丢失和恢复、路径逃逸、取消及下载边界。

发布由管理员操作。两个组件均使用 `--artifact-directory=/private/agent-artifacts`，并只读挂载同一份目录树。复制 `extensions/container-runtime/config/examples/agent-release.yaml`，将占位符替换为 ContainerCluster 的 namespace、租户、实际版本和小写 SHA-256，并选择匹配的 `amd64` 或 `arm64` 架构。创建 AgentRelease 前，将构建出的 Linux `container-runtime-agent` 二进制放到该目录下的 `releases/agent-v1`。现有 API RBAC 已允许读取版本；控制器 RBAC 允许读取及修补版本 status。Agent 不获得 Kubernetes 权限；不要通过公共静态服务暴露目录。

等待 `status.available: true` 且 `status.observedGeneration == metadata.generation`，再由管理员将目标 ContainerCluster 的 `spec.targetAgentRelease` 设为 `agent-v1`。新版本使用新的记录和路径，不覆盖不可变字节。未提供 CLI/UI 发布器；按需启用的 Linux Agent updater 使用 `TargetRelease` 和 `DownloadRelease`，目标版本由管理员选择，空闲节点由独立启用的滚动控制器选择。改变目标后，在该版本完成安装并由新观测确认就绪前，调度可能被阻止；目标配置自身不授权安装二进制、不停止运行中的 Task，也不释放容量。安装必须有节点限定升级操作及 allocation。文件缺失/损坏会产生 `Available=False` 和有界 API 错误；恢复精确字节或发布新版本，由周期检查恢复。不要修改 status 绕过验证。重新指向目标属于期望配置，不等于自动回滚。运维手动更新 Agent 时也应保持独立 supervisor 持续运行。

### 升级预留（preview）

启用二进制安装前，控制面必须将升级与工作负载分配及其他升级串行化。`AgentUpgrade` 是不可变、具有独立身份的一次操作，固定到原生 Cluster、ContainerCluster、ContainerNode、容器实例、登记凭据及 AgentRelease UID/摘要。其名称由原生 Cluster UID 确定，位于权威 Agent namespace，因此一个集群同时最多存在一个操作。控制器/管理员必须显式创建操作；创建或标记 Reserved 不会安装二进制。

升级预留与 Task 使用完全相同的确定性 `ContainerAllocation` 名称。类型化 `upgrade` 字段区分升级所有权，工作负载 Job/Task 字段缺省。Task 分配和升级预留竞争同一次 API Create，因此最多一方成功。Task 通道对升级占有的锁返回无工作负载；任务清理路径和心跳丢失都不能释放升级锁。节点必须具有当前已验证的原生/附属成员身份、最新认证空闲心跳、显式 GPU 白名单、无旧版预留，以及可用兼容目标。创建预留后再次验证授权，部分写入或响应丢失通过精确读取恢复。

Agent 领取 Applying 前，操作必须记录已观察到的 allocation UID。节点限定升级协议必须携带精确的操作和 allocation UID。成功或已验证回滚需要结果版本的新心跳以及显式空闲完成报告；安装状态不确定的失败保留两个锁。目标变更、删除或撤销证书会拒绝新的应用授权，但不能释放进行中的操作。Pending/Reserved 操作可在任何应用授权之前通过乐观并发状态变更取消；Applying 操作只能由持有它的 updater 完成或回滚。完成操作在其精确 allocation 获得终态证明且确认删除前保持可见。手动操作由管理员保留。独立启用的滚动控制器仅在 updater 确认终态 journal 已持久化且精确 allocation 已确认删除后，才能回收它创建的成功操作；下文节点本地安装器为预览功能。

节点 updater 只替换 Agent 进程，保留并验证上一版二进制，持久化暂存/切换/回滚意图，并保持独立工作负载 supervisor 运行。仅本地 `Busy=false` 永远不足以授权切换；节点限定授权和精确持久尝试均为必需条件。成功 exec 后的健康超时自动回滚尚未实现。

先安装全部扩展 CRD（含 `agentupgrades`）和更新后的控制器 RBAC，再使用 `--enable-agent-upgrade-reservations=true` 重启控制器；该开关默认关闭。控制器需要 allocation status patch 权限以持久化取消证明；Agent 不会获得额外 Kubernetes 或升级权限。发布可用目标版本后，在 `config/examples/provision-upgrade.json` 填入精确的登记节点 UID/凭据、release UID/摘要及观测到的原 Agent 版本。重试时保留相同操作 UUID：

```bash
./extensions/container-runtime/bin/container-runtime-admin upgrade-create \
  --kubeconfig=/private/admin/kubeconfig \
  --config=extensions/container-runtime/config/examples/provision-upgrade.json
kubectl --kubeconfig=/private/admin/kubeconfig -n REPLACE_AGENT_NAMESPACE \
  get agentupgrades.containers.rlark.io -o yaml
```

`upgrade-create` 仅记录意图，只有按需启用的控制器才能将其推进到 `Reserved`。检查 `status.phase`、`status.allocation` 和 `Reserved` condition。锁不可用时保持 Pending 并重试。目标/凭据/身份变化会拒绝新预留，同时保留已有见证的锁。删除精确观测到的 Pending/Reserved 对象可请求取消；控制器先持久化取消，再证明 allocation 清理并确认删除后移除保护。阻塞或 Applying 操作需要持有者 updater 恢复，不能通过移除 finalizer 或修改 status 绕过。自动选择操作仍未实现；节点限定协议和按需启用的本地安装器见下文。测试覆盖工作负载/升级竞争、创建/状态响应丢失、过期与外来身份、延迟删除、删除保护及拒绝任务路径清理升级锁。

在批准的私网测试机替换镜像后，重新核对确认 `/root/RLinf/.venv-pi05` 仍可导入 RLinf 源码，报告 Ray 2.56.1/PyTorch 2.11.0+cu126，且在一张 H100 上执行 CUDA 张量返回 `42.0`。默认 conda Python 不含 Ray/RLinf，需显式选择预置环境。这是直接环境检查，不是已登记 RLark GPU Job 或升级验收。

### 交付门槛

#### 节点限定升级应用协议（预览）

专用 mTLS API 已实现按需启用的 `POST /v1/upgrade`，包含 `assignment`、`get`、`claim` 和 `complete` 操作。身份只来自已验证节点证书。发现接口只返回该节点的当前操作、精确 allocation UID 和不含服务端路径的版本身份，不授权安装。其他操作必须回传操作和 allocation 引用；claim 还需调用者保留的尝试 UUID。API 不创建升级操作或 allocation，也不提供通用 Kubernetes 访问。

claim 通过乐观并发将 Reserved 转为 Applying，记录尝试 UUID 和服务端开始时间。重新验证原生/附属成员关系、租户、凭据、容器实例、allocation 所有权、最新空闲原版本心跳、当前可用目标及文件完整性。其他尝试不能接管 Applying。已提交但响应丢失时只有同一尝试可恢复；目标变更或凭据撤销拒绝新授权，并保留不确定状态和两个锁。取消和 claim 竞争同一操作 resourceVersion；GET/发现不会授权或重做切换。

完成只接受固定目标版本的 Succeeded 或记录原版本的 RolledBack，并要求可信本地 updater 显式证明空闲。必须有授权开始时间之后、经过当前认证、空闲且版本匹配的心跳。API 在操作 status 保存有界完成凭据，不能移除 allocation finalizer。完成以保留身份单独验证所有权，不依赖目标就绪，因此删除或目标变化不会阻止仍获授权的精确持有者报告恢复；撤销凭据仍会拒绝报告。冲突结果和尝试被拒绝，失败、超时、未知状态及心跳丢失都不代表回滚或清理完成。

控制器消费该精确完成凭据，写 allocation 清理证明并确认删除后，才将操作设为终态。完成、状态、删除或 finalizer 响应丢失后会收敛，不能再次安装。完成操作在管理员显式删除前保留；没有完成凭据的 Applying 操作继续受保护。预览 API 使用 `--enable-agent-upgrades` 开关和 `config/rbac/container-runtime-api-upgrades.yaml`，与 Task 执行独立。二进制安装/切换须单独启用节点 updater，自动节点选择须单独启用滚动控制器；UI 尚未实现。测试覆盖真实 mTLS、错误节点/租户/UID/尝试、claim/取消竞争、已提交响应丢失、过期/忙碌/版本不匹配心跳、目标/凭据变化、精确成功/回滚凭据、延迟删除和控制器重建。

API 使用 `--enable-agent-upgrades=true` 启用，并在基础 API 角色之外应用额外升级 RBAC。启用组件前先安装重新生成的 AgentUpgrade CRD。Linux Agent 仅在自身另行设置 `--enable-agent-upgrades=true` 时轮询此端点。`Client.Upgrade` 接受严格的、最多 16 KiB 的请求并校验有界响应。`assignment` 不接受身份字段；`get` 要求两个精确引用；`claim` 还要求发现结果的 resourceVersion 和规范的非零尝试 UUID，调用方应在发送前持久化。完成响应在锁删除前仍为 Applying；`get` 响应可省略已删除的 allocation，但请求必须保留原记录引用。HTTP 400 表示无效输入，403 表示身份拒绝，409 表示竞争或过期操作，503 表示前提暂不可用。请求不能覆盖证书身份。

#### Agent 二进制更新器（预览实现）

Linux Agent 设置 `--enable-agent-upgrades=true` 后，会在串行心跳循环内以有界周期轮询 `assignment`，只领取 `Reserved` 操作，通过认证的 release 端点获取精确版本清单和字节，并在任何改变前校验不可变 UID、版本、平台、大小、摘要和可执行格式。新二进制写入私有 state 目录的暂存文件，执行 fsync 及目录 fsync，然后原子替换配置的 Agent 安装路径并保留一个保持权限的备份。整个过程不会停止 supervisor。进程替换前写入有界持久回执，记录精确 operation/allocation/attempt 和预期结果；启动时读取并校验回执，为运行中的版本上报新鲜空闲心跳，再提交 `complete`。`exec` 失败时恢复备份，且只有观察到旧版本空闲后才提交 `RolledBack`。更新器自身不会删除或释放 allocation。

更新器复用心跳的 mTLS 客户端和证书身份。它拒绝相对路径或共享的安装/state 路径、符号链接目标、并发尝试、变更的 release 身份、缺失 allocation、过期源心跳及无法证明精确性的响应。它不提供沙箱，也不安装 Python 等依赖。前台启动器和生成的 systemd 单元运行同一个 Agent 进程；systemd 可重启失败的替换，而无 systemd 的开发机依赖持久回执和运维重启。宿主机断电后的自动回滚在完成外部 supervisor 重启测试前不宣称支持。

新导出包在 `provision-package.json` 设置 `enableUpgrades: true`。生成的 launcher 增加 `--agent-install-path=<installDirectory>/container-runtime-agent` 和 `--upgrade-state-directory=<stateDirectory>-agent-upgrades`。启动前以 root 创建独立 updater 目录并设置 0700；安装目录和 updater 目录均应保持私有且位于工作负载路径之外。已有包需重新导出，或在 Agent 启动时显式提供上述三个参数。`--version` 保留为初始版本：切换后通过持久回执将真实 `/proc/self/exe` 摘要映射到 release 版本，服务重启同样适用。正常运维不要删除 journal 或切换摘要文件。journal 绑定端点、客户端证书和安装路径；凭据轮转、目录迁移或未知二进制修改需要显式运维恢复，不会静默接管。

按需启用的包 launcher 校验所有不可变导出文件，Agent 仅可匹配原始摘要或 updater 私有的源/目标摘要文件。更新器写入这些摘要前仍会校验 ELF、平台和完整摘要。备份和暂存各保留一个二进制；中断留下的临时文件只能在 Agent 停止且没有正在应用的操作时清理。claim 和 complete 响应丢失会复用持久 attempt；摘要/身份错误、文件系统失败及不确定状态保留 allocation 并重试。只有进程替换失败会自动恢复旧可执行文件。新二进制启动后无法心跳时尚无自动超时回滚；应保留备份和 journal，由管理员恢复。在宣称支持无人值守滚动升级前，仍须完成 systemd/前台及云端升级验收。

- [x] 中英文文档记录独立扩展、指定队列和验收范围。
- [x] 独立模块/构建，迁移 Provider/supervision/测试；加入原生可选集成接口和所有权校验。
- [x] 类型化附属 schema、单节点预览的 mTLS 授权和管理员接入；直接附属资源写入仍仅限管理员。通用 Job 所有权分派已实现。
- [x] 显式容器集群接入、节点限定身份和整卡分配，已在私网开发机验收。
- [x] 有界授权 Raw 远程日志，已通过真实私有云 GPU Job 验收。
- [ ] 通用准入/迁移和自动注册/续签。
- [x] 认证版本托管和目标驱动的空闲节点滚动选择已作为预览基础设施实现；私有云单节点滚动及 launcher 重启验收已通过。
- [ ] 外部 supervisor 断电恢复和 exec 失败回滚验收。
- [x] 独立容器集群 UI、显式启用的单进程 Raw Job 提交及中英文 API/运维示例；云端浏览器提交/重试/暂停验收通过。Ray/RLinf 多节点提交仍未完成；独立 Raw 模板已由控制器展开。
- [x] 已授权私网 1 GPU 开发机和单进程 Raw GPU Job 成功/复用/失败/停止验收。
- [x] 私有云 Agent 升级和升级后 GPU 生命周期证据。
- [x] 同一运行中容器内，Job 成功/失败/停止和资源删除后的 checkpoint 保留；跨替换存储持久性仍未验证。
- [x] 恢复/竞态测试、扩展 Go 测试/vet/build 和严格双语文档构建。
- [ ] 完整仓库 Go/Web 检查和生成验证。

测试并发预留、旧 UID/generation/实例、跨租户访问、重复注册、部分外部动作、重启、supervisor 丢失、清理失败、心跳丢失、摘要错误、升级竞争、回滚失败、迁移冲突、checkpoint 保留及 Kubernetes 行为不变。不能凭骨架或 mock 勾选门槛。

### 空闲节点自动滚动选择（预览）

扩展实现独立且默认关闭的 `--enable-agent-rollouts=true` 控制器，要求同时启用 `--enable-agent-upgrade-reservations=true`。启用前安装重新生成的 ContainerCluster、AgentUpgrade CRD 及附加的 `config/rbac/container-runtime-controller-rollouts.yaml`。滚动控制器使用不经过缓存的管理客户端，仅拥有 `AgentRolloutReady` condition 和 `status.lastAgentUpgrade`；原集群校验器继续拥有绑定校验及 phase。`spec.targetAgentRelease` 是管理员管理的期望配置。清空它只暂停新节点选择，不撤销 Applying 操作或释放锁。

预览版本串行执行；省略或设为 0 的 `maxUnavailable` 表示最多同时升级一个节点，仅支持 0 和 1。它限制升级并发，不保证其余所有节点可调度：已有目标版本门禁可能阻止仍运行旧版本的节点接新任务。不会抢占运行中的 Job。按稳定名称和 UID 选择节点，要求精确租户及原生/附属绑定、授权 GPU、当前代 Ready/Validated 条件、新鲜的认证空闲心跳，以及显式启用的 `agent-upgrade-v1` 和匹配的 `agent-platform/<os>/<architecture>` 能力。升级选择不使用 `status.schedulable`，因为目标版本门禁本来就会令旧版本不可调度。工作负载或升级 allocation 会排除其节点、地址和 GPU；最终预留仍通过共享节点锁的原子创建与任务竞争。忙碌、离线、心跳过期或未启用更新的节点继续等待。版本相同遵循现有观测契约，不代表远程二进制认证；不同字节必须使用不同版本。

在现有每 Cluster 独占槽中创建不可变、精确 UID 绑定的 AgentUpgrade，初次 Create 同时写入确定性请求 UUID 及显式滚动所有者注解。重复调谐和创建响应丢失可恢复已有操作。不能接管、取消或回收手动/外来操作。目标变更只对本控制器创建、尚无 allocation 观测且没有 attempt 的未分配操作发起携带 UID/resourceVersion 前提的删除。一旦已暴露预留，保留操作并要求 updater/管理员显式取消和恢复，因为可能已有领取前 journal 或响应丢失。Applying 操作绝不自动取消。Applying、失败、已回滚或结果不确定的操作阻止继续选择并保留锁/证明；管理员调查并显式移除已经恢复的失败/回滚操作后才能重试。

认证成功完成后，等待升级控制器写入当前代 `AllocationReleased` 证明，以及持有该操作的 updater 发送 `acknowledge`。updater 先 fsync 终态 journal，再确认精确的操作/allocation/attempt/结果/版本；API 写入 `status.acknowledgedAttemptID`。完成响应丢失时保留操作供恢复；acknowledge 响应丢失可从已保存终态 journal 重试，即使 allocation 已消失。确认仍要求当前认证凭据和新鲜的结果版本空闲心跳。缺少此握手的旧 Agent 不会被自动回收操作。先升级 API，再启用新版 updater 和滚动控制器。先持久保存一个有界的 `status.lastAgentUpgrade` 凭据，绑定操作、节点、版本、摘要和完成时间，再携带 UID/resourceVersion 前提请求删除；确认删除后才能选择下一节点。升级控制器可完成已有终态证明的操作删除，不触碰同一节点键上后来的工作负载 allocation。finalizer 延迟、状态冲突、响应丢失或控制器重启不能跳过顺序。只保存最近一次成功凭据；需要完整历史时向外部归档。所有当前成员均有新鲜目标版本观测时才能设置 `AgentRolloutReady=True`；空集群是等待，不算完成。私有云单节点滚动升级验收已通过；多节点滚动、外部 supervisor 断电恢复、exec 失败回滚和健康超时回滚仍不支持。UI 集成仍未完成。

2026-10-31，从当前自动保存镜像重启现有私网开发机，未改变 `regular-training`、1 GPU、20 CPU、236 GiB 或公网设置。私网 SSH 再次确认一张 H100、PyTorch 2.11.0+cu126、Ray 2.56.1 及 CUDA 张量结果 `42.0`。`TestCloudRawObservation` 在真实 cgroup/GPU/Unix-supervisor 边界通过：`cpu=20000m`、`memory=253403070464`、一张可见 GPU、空闲状态。测试还验证默认不声明升级能力，仅启用 updater 时声明协议及平台。仍没有 Docker socket 或 systemd。该观测运行本身未完成云端 GPU Job 或真实 Agent 滚动升级验收；后续生命周期和滚动升级结果见下文，覆盖单节点 GPU 路径。

### 真实单节点 Raw 验收（显式启用）

2026-10-31，私网 H100 开发机已显式登记到独立 `ksyun-containers` Cluster（`ksyun-container-agents` namespace、`team-a` 租户），并启动导出的 root supervisor/Agent 包。新鲜 mTLS 心跳显示 Ready、可调度、20 CPU、236 GiB 和一张授权 GPU。对真实 KCP 管理 API 运行 `TestCloudRawLifecycle`，四个用例全部通过，总耗时 170 秒：CUDA 成功（`42.0`）、第二个 Job 复用同一 GPU、退出码 7 失败、停止运行中的进程。各用例确认 allocation 不存在，删除自身 Job/binding 并验证 Task 消失；最终节点恢复空闲可调度，allocation 无残留。真实部署还暴露并修复 API 重复注册 `--kubeconfig` 的 panic、选主 Event RBAC 缺失和控制器 logger 未初始化。测试入口依赖 SSH 反向隧道和短期凭据。这证明单节点单进程预览验收，不代表 Ray/RLinf 整组、历史 logs/stats、原生 UI 集成或真实 exec 失败回滚已完成；后文分别记录任务清理后的 checkpoint 保留、有界远程日志和空闲 Agent 滚动升级验收。原生 Cluster/Node 汇总 UI 仍使用自身基于 Lease 的状态；companion Ready 观测不等同于原生 Node Lease，不得据此宣称 UI 集成完成。

启用独立扩展控制器前，必须将部署中的原生 Job/Task/Node CRD **以及**原生 controller-manager 更新到仓库当前版本。旧 Job schema 会拒绝 `placement.environment`；旧原生控制器仍可能用旧 Raw 策略处理扩展 Job，写入冲突的 condition。当前通用 `rlark.io/controller-owner` 所有权边界是必要前提。不要通过删除所有权注解或关闭校验绕过版本不匹配。扩展控制器会初始化 RLark 结构化 logger，以保留调谐和依赖错误。

准备独立 Agent CA、服务端 TLS 证书和受限 API/controller ServiceAccount。签名私钥留在制包机上。用管理员 CLI 创建集群、登记节点并导出，只传输节点包。HTTPS 端点必须可达；隔离测试可使用只监听回环地址的 SSH 反向隧道承载 mTLS。该隧道是测试依赖，不代表稳定的生产入口。Agent 凭据和本地测试 ServiceAccount token 分别到期；本次使用 24 小时凭据，不会自动续期。

节点上的实际可执行文件必须存在，且为无符号链接的规范路径。pi05 验收环境中的 venv `python` 链接到 Conda；直接解析链接会丢失 venv prefix。验收时在同一 venv 的 `bin` 中新建同一 Python 可执行文件的普通副本，验证 `sys.prefix`、PyTorch 导入与 CUDA 后，将 `/root/RLinf/.venv-pi05/bin/python-rlark` 加入白名单。这是验证过的测试环境适配，不是自动 Python 安装或通用 venv 迁移方案。路径变化时应向新的私有目录重新导出包，不得修改导出脚本/配置或绕过校验和。启动 supervisor 前须创建实际工作目录。

以下命令只用于已接入、空闲、仅有一个节点和一张授权 GPU 的专用测试集群。API/Agent 执行开关和 controller reservation 都须启用。每个环境变量均应换成部署中的权威资源与路径。kubeconfig 只留在测试运行机，不会发送给 Agent。

```bash
cd extensions/container-runtime
RLARK_TEST_RAW_KUBECONFIG=/private/management.kubeconfig \
RLARK_TEST_RAW_NAMESPACE=ksyun-container-agents \
RLARK_TEST_RAW_CLUSTER=ksyun-containers \
RLARK_TEST_RAW_NODE=ksyun-test-1 \
RLARK_TEST_RAW_TENANT=team-a \
RLARK_TEST_RAW_EXECUTABLE=/root/RLinf/.venv-pi05/bin/python-rlark \
RLARK_TEST_RAW_DIRECTORY=/root/rlark-smoke \
GOWORK=off go test -v ./pkg/integration -run '^TestCloudRawLifecycle$' -count=1 -timeout=15m
```

未设置 kubeconfig 变量时测试跳过；设置后其余参数全部必填。测试在提交工作前验证精确 Raw Cluster/Node/tenant 绑定、新鲜空闲心跳和 allocation 为空。用例执行真实 CUDA tensor 计算、用第二个 Job 身份复用 GPU、以退出码 7 失败，以及暂停长时间运行的 GPU 进程。每个用例等待当前 Job 结果和 allocation 已不存在后，才使用精确 UID/resourceVersion 删除自身 Job 和 binding，并等待所属 Task 消失。失败时保留尚存对象供检查，不继续派发下一用例。如果 Job 仍存在，需要停止时应检查该精确实例并通过正常调谐暂停，不能强删不确定的 allocation/finalizer。supervisor 回执与日志保留供本地管理员查看；可选远程日志验收见下文，垃圾回收仍不支持。

完成操作的删除兼容 KCP 在删除时增加一次 generation 的行为。只有 Spec 不可变、原观测代上具有完整认证完成回执和控制器持久化的精确 allocation 释放条件的操作，才能跨过这一次增量完成 finalizer 清理。attempt 错误、缺少释放证明或非预期代际变化均保留保护。控制器不改写 status 来伪造新鲜证明，也不检查或修改之后的工作负载 allocation。

### 有界日志验收（已验证）

测试运行器需要有权创建/删除原生 Job 和附属 binding 的管理 kubeconfig；controller/API ServiceAccount 按设计不具有这些管理员权限。要启用可选远程日志检查，需启用 API 任务执行与回环管理员监听器，导出节点包时设置 `enableExecution: true` 和 `enableLogReads: true`，并在上方生命周期命令的参数之外增加：

```bash
export RLARK_TEST_RAW_LOG_ORIGIN=http://127.0.0.1:9446
export RLARK_TEST_RAW_LOG_TOKEN_FILE=/private/gateway-admin-token
```

origin 必须是显式回环 HTTP 地址，可以指向 API 读取监听器，或配置了 `--log-api-origin` 的控制台。token 文件必须是私有普通文件，包含 Gateway 签发的短期管理员 token，不能是签发密钥。文件不得发送到节点，也不把内容放进命令行或日志。输入非法时在创建工作前失败。stop 用例在观测 Running 后限时一分钟等待真实 CUDA/输出就绪，验证未认证返回 401、未知 `path` 返回 400、错误 Task UID 返回 409，再验证恰好 65,536 字节、GPU 标记和截断信息。停止通过正常暂停进行；必须完成释放后，旧身份才返回 409。输出尚未到达时空快照有效（`data: ""`，规范 base64 字符串）。

2026-10-31，四个生命周期用例及直连日志检查在 170 秒内通过；控制台代理的 stop/日志/清理用例另在 28 秒内通过。专用节点恢复空闲可调度，无验收 Job/Task、binding 或 allocation 残留。首次代理测试暴露验收时序竞争（Running 早于 Python/CUDA 输出），精确的保留 Job 已正常暂停并清理后才重试。这只证明有界按需日志，不代表历史归档、follow/tail、统计、terminal/exec 或生产入口完成。

### Checkpoint 保留验收（已验证）

在生命周期与远程日志参数之外设置 `RLARK_TEST_RAW_CHECKPOINTS=true`，即可
验证工作负载文件保留。该模式要求认证日志：stop 用例必须证明 checkpoint
已经写入后才暂停，不能把进程创建视为应用就绪。各用例在配置的工作目录中
以排他创建方式写入唯一命名的 JSON checkpoint 并 fsync。后续 Job 在先前
Job、Task、binding 和 allocation 均消失后，核对所有先前文件的精确内容。
最终验证 Job 检查全部四个 checkpoint，包括被停止进程的文件。文件缺失或
被更改均失败，验证不会重新创建。只有最终成功的验证程序显式删除这些测试
自身的文件；失败保留文件和资源供检查。该验收只证明运行中容器内的生命周期
清理，不证明平台替换后的存储持久性、任意挂载或训练框架 checkpoint 完整性。

2026-10-31，私网 H100 开发机上五个用例在 216 秒内通过：成功、GPU 复用、退出码 7 失败、正常暂停及最终 checkpoint 校验。结束后验收 Job/Task、binding、allocation 和测试文件均不存在，节点空闲且可调度。此前一轮因 Agent 轮询和日志读取分别到达不同 API 进程而超时；确认 allocation/Task 清理后，正常暂停并删除其精确 Job，核对四份测试文件后显式移除。成功的一轮由同一 API 进程处理 Agent 轮询与日志监听。随后平台查询显示开发机于 04:20 停止，镜像自动保存成功。增加显式 checkpoint-ready 日志标记断言后的复测，在创建任务前被过期心跳预检拒绝；该额外断言尚未在此开发机复测。再次启动应重新核对实例身份；如果平台替换了容器，必须显式退役并重新注册，旧 Ready 证据不授权派发。

### 云端空闲滚动升级验收流程（已验证）

该验收门槛已于 2026-10-31 在金山云私网 H100 开发机上通过。以下流程和证据保留用于重复的运维验收。启动已停止的现有开发机前，重新读取平台状态、队列、资源规格和私网配置。平台停止/启动可能替换容器、PID namespace、宿主机和 GPU；将当前实例证据与登记的 `instanceID` 比较，不能复用旧凭据或修改不可变 companion Spec 来接管替代实例。确认旧实例已停止且其 Job/Task、allocation 和升级操作均不存在后，显式退役精确的原生/companion Node 实例，再以新 UID、实例证据和凭据登记替代节点，保留独立 Cluster。保留的 supervisor 记录归旧实例所有，不能据此授权新执行。

对独占空闲单节点测试集群，向新的私有安装/state 路径导出 `enableExecution: true`、`enableUpgrades: true` 的新包。管理员凭据和 CA 签名私钥留在制包机。校验安装包摘要和预装 Python/CUDA 环境，启动独立 supervisor，创建 root 所有且权限 0700 的独立 updater 目录，再启动 Agent。不能修改旧导出 launcher 来启用升级。选择目标前，确认新鲜观测包含 `agent-upgrade-v1` 和 `agent-platform/linux/amd64` 能力。

将独立构建的 Linux Agent 按不可变版本发布，摘要须不同于当前运行二进制。仅重新打包同一源码只能验证切换/传输，不能证明不同源码版本的兼容性。API 和 controller 配置相同只读产物目录，应用额外 API-upgrade 和 controller-rollout RBAC；先启用 API 升级通道，再启用 controller 的升级预留和滚动选择，等待 release 当前代可用。重启控制面进程时保留工作负载预留，组件中断不能作为清理证明。

显式启用的 `TestCloudRawRollout` 复用 GPU 生命周期测试的 `RLARK_TEST_RAW_KUBECONFIG`、`RLARK_TEST_RAW_NAMESPACE`、`RLARK_TEST_RAW_CLUSTER`、`RLARK_TEST_RAW_NODE`、`RLARK_TEST_RAW_TENANT`，另需 `RLARK_TEST_RAW_RELEASE`。版本产物须已发布且可用。测试要求恰好一个新鲜空闲且支持升级的成员、没有 allocation/升级操作、未配置目标版本；以乐观并发选择目标，观测精确节点/release/摘要完成凭据、当前代滚动就绪、新鲜且可调度的目标版本心跳，以及升级操作/allocation 删除。验证期间保留目标配置，随后由运维清空目标，使独占测试集群回到暂停状态。不能在共享或生产集群运行。

```bash
cd extensions/container-runtime
# 先设置已有 RLARK_TEST_RAW_* 身份和 kubeconfig 参数。
RLARK_TEST_RAW_RELEASE=agent-cloud-accept-v2 \
GOWORK=off go test -v ./pkg/integration -run '^TestCloudRawRollout$' -count=1 -timeout=10m
```

第一次滚动升级将 Agent 从 PID 282/启动 ticks 996548774、摘要 `c0a8f4…` 切换到目标摘要 `db5714…`；supervisor PID 246 及摘要保持不变。通过校验有效的 launcher 重启空闲 Agent 后，持久 journal 仍报告已安装的 `cloud-accept-v2`，没有回到 bootstrap 版本。第二次使用不可变 `cloud-accept-v3` 产物，保持 Agent PID 497 不变并将摘要从 `db5714…` 切换为 `606fd3…`，supervisor PID 246 仍不变；`TestCloudRawRollout` 通过，耗时 32 秒；第一次滚动暴露了 KCP 删除代际问题，修复后的控制器已恢复其保留的 finalizer。升级后的 GPU 生命周期复测在 166 秒内通过成功、复用、退出码 7 失败和暂停四种用例。该证据不证明多节点推进、此云镜像上的 exec 失败回滚、宿主机断电恢复、续签或健康超时自动回滚。失败/不确定安装保留 allocation 和 journal，不能强删以让测试通过。

## 历史预览证据

本文后续内容保留原生 Raw/Docker 实验作为迁移证据，不是目标扩展 API，历史多节点范围也不替代上面的单节点验收门槛。

## 已确认范围

多个现有 GPU 容器作为独立 Node 加入同一逻辑 Cluster，每个容器内运行一个 Agent，使用预装环境执行进程。不需要 Docker daemon、socket、创建容器或 Docker-in-Docker。一个容器同时独占运行一个 Task，一个 Job 可以占用多个容器。首版支持同一云容器组内的 Ray/RLinf，不支持云 Raw 与机器人 Kubernetes 混合 Job。容器私网地址已确认互通，训练地址与控制面连接地址分别配置。

## API 与所有权

复用 Cluster/Node/Job/Task，Cluster.spec.agentNamespace 是成员关系的权威来源，标签不是所有权凭据。记录 Node UID、容器实例、执行和分配标识。Agent 写能力/环境/健康观测，控制器写资源预留。新增运行时无关副本字段，兼容无歧义的 Kubernetes 旧字段并拒绝冲突；Raw 副本展开为独立 Task，索引/rank 稳定，同时聚合模板与实例状态。节点级 claimant 不得接管同 namespace 的其他节点 Task；直接创建 Task 也不能绕过预留和租户验证。

## 调度与生命周期

使用 API resourceVersion CAS 实现节点独占预留。逐步取得整组资源，控制器重启后恢复，部分取得失败则回滚并退避。未全部提交禁止启动。断连、心跳过期或清理结果未知均保留预留，只能在确认清理完成后释放。手动重试先清理旧组，再创建新执行标识。

流程：全部预留 -> 全部预检 -> head 启动 -> worker 加入 -> 精确验证分配成员 -> 训练仅启动一次 -> 记录结果 -> 整组清理。准备和就绪超时可配置，错误必须阻止训练。head/worker 确认失败后整组停止；断连代表未知，不等于已经失败或停止。用户停止与失败分开；训练进程退出决定成功，不能用后台 Ray worker 的存活期代替结果。

## 执行框架

在已有 Host/Coordinator 内实现类型化 Raw 进程 Provider 和 Ray-on-Raw Adapter，使用小接口注入进程、文件、时钟、API 依赖，不另建旁路执行控制器。外部动作前持久化执行声明。独立 supervisor 跨 Agent 重启存活并保存结果，重新附着必须验证执行标识及 boot/start-time，不能仅凭 PID。隔离执行目录、进程 session、Ray 临时路径和有界日志；只清理证实属于该执行的后代，禁止全局 ray stop/pkill。所有权不明、supervisor 丢失但可能有后代或 PID 重用时阻止节点复用。容器替换不能静默接管旧执行。

## Ray 与网络

显式配置私网节点 IP，以及同一网络共享范围内不重叠的 Ray 端口段；关闭无用 dashboard/client。验证支持的 Ray 版本、精确成员，传播 readiness 错误，实际 bind 失败必须算执行失败。单容器单 Task 本身不能隔离 host network 端口。RLinf/NCCL 连通性独立验证，不承诺任意框架动态端口均可控制。

## 环境、资源与存储

只使用预装 Python/Ray/RLinf，验证绝对路径、版本和能力，不自动装依赖。资源容量来自 cgroup/affinity 而非物理宿主机总量。GPU 可见不代表已授权，必须有确认 UUID 白名单或平台绑定，范围未知拒绝调度；只支持整卡独占。分配与利用率分开，遥测可选且有界。

只使用已有挂载，每个节点验证目录权限，节点身份/状态与执行/日志目录分离。Job 删除和清理保留 checkpoint。stdout/stderr 本地轮转，经已有授权 Gateway/Server/Agent 路由提供 tail/since/follow，不允许任意路径/PID。凭证按节点限定，不能传入工作负载环境。

## 接入与界面

rlarkadm 生成配置和前台启动包，可接已有 supervisor，不假设容器内有 systemd。展示 Raw 能力、环境、健康、占用；Job 选择节点、环境、入口而非镜像。展示预留、预检、Ray、训练、清理条件，每副本日志以及手动停止/重试。首版不含通用终端/SSH 注入、TensorBoard 代理、动态 PVC 或云平台调度适配器。

## 交付清单

- [x] 从确认的父分支创建功能分支。
- [x] 中英文设计与交付计划。
- [x] 预览契约的类型 API、schema/deepcopy 生成、兼容及所有权测试。
- [x] 一个 Task 独占一个 Node 的持久化预留、回滚和恢复。
- [x] 历史原生预览路径通过 Host/Coordinator 和轮询 Agent 完成单节点 supervisor 执行。
- [ ] 多节点 Ray 屏障及整组生命周期。
- [x] 预览路径的节点注册、限定凭证、GPU 授权和环境校验。
- [ ] rlarkadm 启动包、授权日志、Gateway 和 UI。
- [ ] 中英文示例、能力矩阵、部署和运维文档。
- [ ] 定向及仓库 Go/web/doc 检查，附证据的草稿 PR。

## 验收与故障测试

覆盖并发抢占、部分回滚、控制器重启、外部动作后崩溃、Agent 重新附着、PID 重用、supervisor 丢失、孤儿后代、端口占用、GPU/环境未知、跨节点/租户认领、head/worker 失败、断网、停止与失败区分、checkpoint 保留、Kubernetes 兼容。CPU 假实现和进程集成测试本地运行；真实双容器 Ray/GPU 与最小 RLinf 验证需要准备好环境并确认 GPU 授权，不自动安装或启动云训练。未验证行为明确列出，端到端验收前保持 preview。

## 当前实现证据与剩余接线

历史原生预览路径接入了实验性 Raw Agent 轮询、受限心跳、受限 API、Job 派发和 Host/Coordinator，用于预置 Node 上一个可信 root 进程 Task，不代表生产 Ray/RLinf 或多节点 Job 支持。上文的独立附属路径现在具有单独的预留控制器及可选的限定单进程执行/清理通道。认证派发、group barrier、端口分配和 GPU/平台验收完成前不要提交 Ray/RLinf 训练。生成的 `status.raw`、`status.reservation`、`spec.placement.binding` 和 `spec.placement.environment` 仍是历史预览契约，新附属预留路径不使用它们。

已实现的边界：

- `rawallocation.Manager` 使用实时读取和乐观锁状态补丁，实现整节点预留、逐步提交、相同执行恢复和未提交预留回滚。环境标识固定在预留中。校验检查成员关系、节点/容器身份、心跳、禁止调度/排空、租户、私网范围、不同训练 IP 和 RLark inventory 中 GPU UUID 不重叠；不能证明云平台授权的独占性，也不能阻止 RLark 外部进程。
- `AssignTask` 将已提交预留幂等绑定到唯一 Task UID。仅提交预留不能授权执行；节点准入要求组内每个节点均已分配 Task UID，还实时验证每个同组 Task 存在、未暂停、绑定完全一致，并由同一个权威 Job 实例持有。
- `raw.NodeAdmission` 重新读取 Task、所属 Job 和整组预留，拒绝直接创建的 Task、旧 generation、暂停、容器替换、跨租户或不完整组。受限服务端只允许限定读取、心跳、状态和 claim 写入；discovery、watch、tunnel 和任意资源均拒绝。
- `raw.Provider` 实现已有 Runtime Provider 和 inventory 契约，启动和清理委派给独立 supervisor；未确认清理持续返回处理中。Raw Host claimant 必须精确包含 Node UID，禁止接管旧声明。
- `raw.NewCoordinator` 和 `raw.NewHost` 通过现有框架组装显式 workload adapter、版本化 process carrier 解码、节点级声明、执行和状态。进程 manifest 的 namespace/name/UID 必须与 Task 一致；验证和 lowering 不产生进程副作用。已测试 Host 重建、重复协调、同级节点隔离、暂停清理、Job 替换和所有权冲突。Agent 每五秒轮询以修复丢失通知。尚未注册内置 Ray adapter。Unknown 映射为 `Pending` 并设置 `ExecutionKnown=False`，不能据此认定进程已停止。
- Linux supervisor 在启动前持久化意图，显式支持 cgroup 和可信 root 进程树两种后端，记录结果，通过私有 Unix socket 仅接受 Task 身份。已测试 Agent 客户端重新连接。supervisor 自身重启后将未确认执行标为 Unknown 并阻止复用，不接管 PID。

### 实验 supervisor 前置条件与构建

执行 `make -C apps/rlark raw-supervisor` 构建。该目标按需启用，不表示 Raw Agent 已可用。默认 `cgroup` 后端要求 Linux cgroup v2、`cgroup.kill`，以及内核支持原子 `CLONE_INTO_CGROUP` 放置。运维需提供可写的委派子树和独立非 root 工作负载 UID/GID。工作负载不得迁移到其他 cgroup、写控制文件、读取节点凭证或写 supervisor 状态/socket 路径。普通云容器可能没有所需权限，需先验证委派；不能因此接入 Docker socket，也不应默认要求 privileged 模式。

通过 `bin/raw-supervisor --config /private/supervisor.json` 传入严格 JSON，字段如下：

```json
{
  "backend": "cgroup",
  "stateDirectory": "/var/lib/rlark-supervisor",
  "executables": ["/usr/bin/python3.12"],
  "workDirectories": ["/workspace/training"],
  "logBytes": 10485760,
  "cgroupDirectory": "/sys/fs/cgroup/rlark-workloads",
  "workloadUID": 10001,
  "workloadGID": 10001
}
```

路径必须存在且为规范绝对路径，配置可执行文件前先解析符号链接。状态目录私有（0700），位于工作负载目录之外，跨 Agent 重启保留。授权可执行文件及其父目录必须防止工作负载替换；路径白名单本身不保证文件完整性。训练不继承 Agent/supervisor 环境，仅接收显式环境。cgroup 后端按配置 UID/GID 运行；进程树后端以 root 运行可信任务，私有文件权限不是对这些任务的安全边界。Unix socket 权限 0600，只供可信本地 Agent 身份使用；该 socket 不提供远程日志鉴权。

当前日志只保留每次执行最初 `logBytes` 字节，持续读取并丢弃超额输出；尚未实现轮转、tail、since、follow 或 Gateway 路由。记录和每次执行的 cgroup 目录尚无自动回收。不得删除活动或未知记录来绕过所有权检查。活动期间停止/重启 supervisor 可能留下未知后代；重新创建节点实例前，需取得容器/平台生命周期证据并经过可审计恢复流程。目前故意不提供强制释放/恢复端点。supervisor 不删除私有状态目录之外的 checkpoint。

### 独立进程准入与分布式就绪

独立的 `raw.rlark.io/process` 成员启动前不等待所有兄弟 allocation 到齐。
同一 Job 有两个模板、只有一个合格节点时，可以先完成一个成员，保留其终态 Task
身份并释放精确清理后的 allocation，再在同一节点运行等待的成员。兄弟 allocation
缺失或已经释放不能成为通用 Agent 启动门禁。每次启动仍必须校验当前 Job、Task、
凭据和自身 allocation；整组替换期间，持久化的 `TaskSetRetiring` 条件仍阻止启动。

这与计划中的 Ray/RLinf 适配器不同。该适配器必须在训练前预留并验证全部必要成员，
具有精确的执行/成员身份、端口声明、有界就绪超时及经确认的整组清理。
独立进程的部分准入不能作为分布式训练就绪的证明。

本地回归组合真实预留控制器、mTLS Task API、Host/Coordinator 和模拟 supervisor，
覆盖两个模板共享一台节点、首成员成功或失败、supervisor 不可用时的清理等待、
控制器重建、拒绝旧成员迟到的清理请求，以及两次 allocation 清理后才聚合终态。
这属于本地集成证据，不代表云端 GPU 或分布式训练验收。

### 按依赖顺序完成接线

1. 增加从有效 cgroup 或 affinity 边界测量的 CPU/内存容量、资源观测及运维健康状态。
2. [x] 增加运行时无关的多模板校验及旧字段冲突处理。将 Raw Job 展开为稳定的逐模板 Task，分别分配并聚合清理；Ray/RLinf 屏障留待后续步骤。
3. 定义并校验 Ray/RLinf 类型化工作负载契约：精确执行/成员身份、支持版本、head/worker 角色、网络范围、私网地址及端口要求。在完整适配器接通前继续拒绝该工作负载派发。
4. 组装适配器，提供整组预留、预检/就绪屏障、有界超时、按网络范围预留端口、精确成员验证及唯一训练结果负责者。全部分配成员验证前禁止训练；区分 Unknown 与失败，确认组失败后停止全部成员。
5. 完成清理/重试所有权：包括 Job 删除/重启和部分派发组，所有 supervisor 确认清理前保留预留。为 supervisor 丢失提供可审计运维恢复；心跳消失不释放容量。
6. 增加前台注册/启动包、授权日志路由和有界保留、UI 能力开关及逐副本进度。链路可用后才添加可执行中英文部署/训练示例。
7. 在两个准备好的容器验证显式选择的监督后端、授权 GPU UUID 和支持的 Ray/RLinf 版本；覆盖端口占用、后代清理、Agent 重启、supervisor 丢失、断网、head/worker 失败和 checkpoint 保留。本地尚未执行这些平台测试。

### 本地验证证据

内部组件通过定向竞态测试和仓库 `make test-go` 测试，supervisor 二进制构建成功，API/client/UI 生成模型已重新生成，中英文文档通过严格 MkDocs 构建。下方历史预检早于真实单节点 GPU 验收，当前验收结果已在上文记录。尚未完成委派 cgroup 或双容器 Ray/RLinf 验收。仓库 Go lint 仍报告原生代码基线问题；增量检查未发现扩展改动新增问题。这些证据与当前单节点结果仍不代表上方多节点交付清单全部完成。

### 金山云容器预检（2026-10-30）

通过已有 SSH 连接检查了此前确认的开发容器。仅运行短时 CPU 进程及能力探针，没有 GPU 计算、安装依赖、修改平台任务、写父级 cgroup 或重新挂载文件系统。

| 检查项 | 实测结果 | 影响 |
| --- | --- | --- |
| 内核与 cgroup | Linux 5.15；cgroup v2 为只读挂载 | 创建子 cgroup 返回 `EROFS`，cgroup 后端无法在此容器运行；仅存在 `cgroup.kill` 不足以满足要求。 |
| CPU 与内存边界 | CPU affinity 为 192；`cpu.max` 为 `12000000 100000`；`memory.max` 为 `1523639648256` | 仅按 affinity 会高估 CPU 容量，实际 quota 相当于 120 个 CPU。 |
| GPU 可见性 | `nvidia-smi` 列出六张 H100 80GB | 可以枚举设备，但不代表已获分配授权；没有执行 GPU 任务。 |
| 默认 Python | 所检查解释器中没有 `ray`、`torch`、`rlinf` 发行包；PATH 无 `ray` | 此解释器不是已准备好的训练环境；后续已找到预装环境，见下文。 |
| CPU 子进程 | 短时 Python 子进程正常退出 | 普通进程执行可用。 |
| user/PID namespace | `unshare --user --map-root-user --pid --fork` 成功 | 存在替代隔离边界的可能性，但当前 supervisor 未实现。 |
| namespace 后代清理 | namespace PID 1 退出，其子进程创建新 session，父进程按 PID/start time 验证无存活的非僵尸进程 | 内核清理了本次 `setsid` CPU 后代；仅为可行性探针，不代表 Ray/GPU 验收通过，也不是生产所有权/恢复实现。 |
| 私有 proc 挂载 | user/mount/PID namespace 结合 `--mount-proc` 在更改挂载传播属性时返回 `EPERM` | 不能假定允许挂载私有 `/proc`；namespace 实现需要单独设计和验证。 |

当前可查询的开发任务列表没有匹配该 SSH 端点，平台 GPU 分配授权仍未确认，不能用六个可见 UUID 推导授权白名单。当前 cgroup supervisor 在该目标上存在阻塞；需先实现并验证兼容真实容器的执行边界，再继续生产接入。不得静默降级为仅按进程组清理，也不得改成 privileged 模式或重新挂载宿主机 cgroup。

### 可信 root 进程树后端

已确认的云容器模式让 Agent、服务、专用执行 worker 和训练全部使用 root，保留容器原有 PID namespace 和 `/proc`。必须显式选择，cgroup 预检失败不会自动切换。要求 Linux subreaper、pidfd、匹配的 `/proc` 和可信训练代码。不创建额外容器或 mount namespace，不提供 UID 隔离或任务级 CPU/内存硬限制，资源边界仍是外层云容器。一个容器同时独占一个 Task，直到证实清理完成。root 训练可以修改服务文件或凭证，因此这不是沙箱，也不能作为互不信任租户之间的执行安全边界。

构建 `raw-supervisor`，解析安装路径及 Python 符号链接，准备工作目录，传入以下配置（路径需替换为实际规范路径）：

```json
{
  "backend": "process-tree",
  "workerExecutable": "/usr/local/bin/raw-supervisor",
  "stateDirectory": "/var/lib/rlark-supervisor",
  "executables": ["/usr/local/bin/python3.11"],
  "workDirectories": ["/workspace/training"],
  "logBytes": 10485760
}
```

`workloadUID`、`workloadGID` 必须省略或为零，`cgroupDirectory` 必须省略。未知后端、混合凭证/cgroup 配置均拒绝。worker 使用已安装的同一个 `raw-supervisor` 二进制；`--process-tree-worker` 是需要继承协议描述符的内部模式，不是用户训练命令。Agent 重启后连接原服务 socket，不要随 Agent 一起重启服务。

每次执行由服务先持久化意图，再启动唯一专用 worker。worker 在启动训练前启用 subreaper，接收显式环境并保留原 `/proc`。仅内核证明的子进程关系可以授权发送信号。清理时打开 pidfd，通过 `waitid(P_PIDFD)` 再次确认子进程归属，然后反复终止和回收被收养的后代；只有 `ECHILD` 才证明清理完成，进程名/PID 列表为空不够。禁止按 root UID、命令名、端口或进程组批量清理，禁止全局 `ray stop`/`pkill`。主进程正常退出后仍清理后代。停止先向主进程发送 SIGTERM，等待 30 秒后使用 SIGKILL；主进程强制退出确认和后代清理各有 30 秒上限，任一无法确认都阻止复用。

服务必须同时收到 worker 的清理成功结果并观测其正常退出，才持久化 `cleaned=true`。结果缺失/非法、worker 丢失、意图传输中断或持久化不确定均保留占用。服务自身重启后不接管旧 PID，即使 worker 可能仍在运行。未知执行通过已验证的平台容器替换恢复，不提供强制释放端点。宿主机 boot ID 或 Agent 重启都不能单独证明容器已替换。状态目录外的 checkpoint 保留。Agent 失联不会指示 worker 停止训练，但服务自身丢失暂不支持透明恢复。

后续云探针找到 Python 3.11.16 环境：`/root/fleet/venv/bin/python` 包含 Ray 2.59.0、PyTorch 2.10.0、RLinf 0.4.0；`/opt/rl_dashboard_bundle/env/bin/python` 包含 Ray 2.56.1、PyTorch 2.7.0。user/PID namespace 中 CPU Ray 任务虽能完成，但继承的 `/proc` 会引发 psutil PID 错误，`include_dashboard=False` 也会启动部分后台服务。在临时挂载点挂载私有 proc 和 tmpfs 同样失败。选用进程树后端避免该 PID 不匹配问题。已验证 GPU 可见，GPU 授权和真实 GPU/多容器训练尚未验证。授权环境前必须解析 Python 可执行文件符号链接。

root 进程测试覆盖脱离会话后代清理、无关进程保护、重复启动/停止、完成结果恢复和结果缺失后的阻塞恢复。这些测试不代表生产 Agent 注册、限定授权、Job 副本分发、Ray 屏障、授权日志或 UI 已接通；完成这些接线和端到端验收前，交付清单仍未完成。

实际云容器验收：已构建的新 supervisor 以 root 运行进程树后端，通过私有 socket 启动 Ray 2.59.0 CPU 任务（1 CPU、0 GPU），返回 `{value:45, uid:0}`，最终 `Succeeded` 且 `cleaned=true`。重复 Ensure 复用原执行，每次 Observe 使用新连接；确认清理后 Stop 返回删除标记。未安装依赖、未运行 GPU 计算。该测试没有覆盖完整 RLark Job/Agent 接入。

当前只允许规范可执行路径。Python venv 的符号链接解析后可能失去 venv 的 `sys.prefix` 和包搜索路径；本次探针显式设置已准备环境的 `PYTHONPATH` 和 `PATH`，不能据此宣称直接传 venv 链接即可运行。正式环境适配还需验证 Python 前缀和依赖解析。

新增故障测试覆盖运行中 socket 重连、忽略 SIGTERM 后等待 30 秒再强制停止，以及结果持久化失败。结果落盘失败返回 Unknown、`cleaned=false`，恢复磁盘后重复 Stop 也不释放节点；重启只读取原持久化意图并继续阻塞。

## Docker Agent 验证

Docker 预览路径已用 root Agent、宿主机 Docker socket 和 Docker Engine API 1.44+ 验证。验证覆盖 Agent 启动、Node 容量与 incarnation 观测、持久化预留校验、容器所有权及可重试清理。真实多主机 GPU Ray 训练、Gateway 日志/stats 路由、按设备 UUID 分配及限定作用域端到端执行仍不属于独立扩展的已完成契约。本段是原生代码中早期实验记录，不代表新 Docker Agent 路径已接通；只读观测 systemd 安装包生成现见 [Docker 主机观测](docker-agent-observation.md)。

单主机 Ray 冒烟 Job `docker-validation-20261030` 已成功退出（exit code 0），日志包含 `RLARK_DOCKER_SMOKE_OK`。删除 Job 后确认对应 Task、Docker 容器和 Node reservation 均已清理。当前每台 Docker 主机只接收一个 RLark Docker Task，纯 CPU Task 也遵循此约束。

## 2026-10-31 云端 Raw 生命周期复测

开发机从当前 autosave 镜像重启，该镜像继承此前获准的 `rlinf-dashboard:1.1` 替代环境。使用新的 ContainerNode UID、credential 以及 PID namespace/start-time 实例身份重新登记，将 commit `4b8258c` 的 package 放入新的私有安装/状态目录。由于开发机没有公网 IP，重新建立了临时 SSH 反向隧道；这只是运维测试路径，不是生产入口。开发机仍位于 `regular-training`，报告一张 H100/授权 GPU UUID、20 CPU、236 GiB，且没有 Docker socket。保留了已保存环境，没有重置基础镜像或安装依赖。

使用预置 `/root/RLinf/.venv-pi05/bin/python-rlark`（Python 3.11.11、PyTorch 2.11.0+cu126、Ray 2.56.1、RLinf 源码）运行 `TestCloudRawLifecycle`，165.10 秒内全部通过：

1. CUDA 张量计算返回 `42.0`，Job 成功。
2. 独立的第二个 Job 复用已释放 GPU 并成功。
3. 进程以 exit code 7 退出，Job 正确失败且释放 allocation。
4. 运行中进程被 suspend，Job 停止并完成清理。

最终节点心跳新鲜，`Ready=True`、`Schedulable=True`，容量为 `20000m` CPU / `253403070464` 字节内存 / 1 GPU，且没有遗留 allocation、Task、ContainerJob 或 AgentUpgrade。这证明重启后的单节点、单进程 Raw 路径仍可用；不证明 Docker Engine 接入、多节点 Ray/RLinf 屏障、checkpoint 保留或生产入口。

## 已完成增量：控制台节点登记

独立控制台现在提供显式启用的管理员 Raw 节点登记，复用接入操作及精确原生/companion
UID 所有权。证书/安装包导出留在管理员制包机。API/UI 测试已覆盖响应丢失/部分创建、
外来身份、输入校验、能力禁用及鉴权。云端接入与安装包安装仍是独立运维门槛，详见
[控制台约定](container-runtime-console.md)。

## 当前增量：独立 Raw 模板展开

预留控制器接受每个外部拥有 Job 中 1 至 16 个名称唯一的独立
`raw.rlark.io/process` v1 模板。每个模板生成稳定的原生 Task
（`<job>-<template>`）和精确 allocation。一个容器 Node 同时最多运行一个成员；
不同成员可以使用不同的预装环境或 Node selector。控制器和 Agent 共享校验契约，
拒绝空名称、重复名称、`Head`、下游边、SSH、Pod 模板、旧绑定及 GPU 可见性覆盖。

这是部分准入：可用成员可以先运行，其他成员等待容量；只有前一成员 allocation
完成精确清理并观察到删除后，后一成员才能复用节点。此契约适用于独立进程，
不保证同时启动，不提供 Ray head/worker barrier、端口预留、RLinf/NCCL 成员关系，
也不会因一个成员失败而自动取消其他成员。不要用它启动彼此依赖的分布式训练。

没有成员运行时，创建、容量等待及观测/清理等待均为 Job `Pending`；至少一个成员
当前运行时为 `Running`。`status.tasks` 展示各成员当前结果，过期观测显示 Pending。
Job 终态要求每个成员都有当前 Succeeded/Failed 结果、known/cleaned 条件、精确清理
证明，且观察到全部 allocation 删除：任一失败则 `Failed`，否则 `Succeeded`。
失败成员不停止独立的其他成员。未知成员保留锁，Job 不能进入终态。
完成的 Task 身份持续受保护以防重跑。

`Scheduled=True` 表示本次调谐成功预留所有未完成成员；已完成释放的成员不再分配。
`ExecutionObserved` 表示至少一个成员有当前观测；`AllocationReleased=True` 要求整组
完成清理。暂停/删除先请求停止每个成员，再等待各自清理。模板、重启标记或已分配
Job generation 变化会退役整个旧集合。控制器先持久化 `TaskSetRetiring=True`，
确认旧集合全部消失后才允许替换；重启或部分删除不能绕过此屏障。此恢复标记跨 Job generation 有效，也会阻止 Agent 新启动。

先升级扩展 API，再升级控制器；旧 API 仍拒绝多模板。原生 schema 无需改变，
单模板提交保持兼容。控制台提交 API/UI 仍为单进程；管理员可通过管理 API 测试
多个独立模板：

1. 复制 `extensions/container-runtime/config/examples/raw-processes-job.yaml`，
   替换租户、预装环境、规范 Python 可执行路径和授权工作目录，使用
   `kubectl create -f` 创建暂停的 Job。
2. 从权威 API 读取该 Job UID 和已有原生 Cluster UID。将这些 UID、租户、Cluster
   名称及 agent namespace 填入
   `extensions/container-runtime/config/examples/raw-processes-binding.yaml`，
   使用 `kubectl create -f` 创建绑定，不能复用其他 Job UID。
3. 使用 `kubectl patch jobs.rlinf.io independent-raw-processes
   --type=merge -p '{"spec":{"suspended":false}}'` 激活；观察 Job、Task 和 agent
   namespace 中的 `containerallocations.containers.rlark.io`。
4. 停止时将 Job `spec.suspended` 改为 true，等待 `Stopped`、精确 allocation 和
   Task 消失，再删除绑定和 Job。清理未知时保留身份用于恢复，不能强制移除锁。

控制器测试覆盖独立节点锁、部分容量、前一成员阻塞时后一成员的清理、过期结果、
全部成员终态聚合、子资源创建响应丢失、控制器重建、整组替换及终态身份保留。
Agent 测试验证非首个模板的精确授权，并拒绝非法、缺失或含糊成员。
本增量只有组件测试，不声称通过多节点云端 GPU 或 Ray/RLinf 验收。
