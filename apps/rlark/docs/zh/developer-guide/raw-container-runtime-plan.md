# 容器集群扩展：设计与交付计划

状态：预览能力迁移中。本文定义目标架构；后续历史证据不代表独立扩展已经部署。

## 已确认架构与执行顺序

先编写并验证本中英文计划，再在现有功能分支和 PR 上实施，不重写已发布历史。新代码放入 `extensions/container-runtime/`，具有独立 Go 模块、API group、UI 包、二进制、测试和构建入口。复用原生 Cluster/Node/Job/Task 作为资源和工作负载模型，不向其目标 schema 添加容器专属字段。已有预览字段在安全迁移证明清理和所有权前仅作兼容数据。

原生 RLark 集成仅限通用的控制器所有权分派、依赖注入、服务/UI 路由和构建接线。容器调度、注册、资源观测、鉴权、版本分发和运行时行为归扩展所有。复用已导出的 Host/Coordinator/Provider/Adapter，不复制执行引擎。通用改动与 Docker/Raw 实现分离，并维护原生接入清单以便跟随上游升级。

### 类型化附属资源与所有权

扩展拥有 `containers.rlark.io/v1alpha1` API group。`ContainerCluster` 按稳定身份和 UID 关联一个原生 Cluster，记录运行时和升级策略。`ContainerNode` 关联原生 Node/Cluster UID，记录容器/Engine 实例、私网地址、能力、环境、授权 GPU UUID 和 Agent 版本。`ContainerAllocation` 保存 Job/Task UID、generation、租户、节点实例、执行标识、设备预留和清理进度。`ContainerJob` 是原生 Job 的类型化控制器绑定，不是第二套工作负载 API。`AgentRelease` 标识不可变产物、SHA-256 摘要和协议兼容范围。

`Cluster.spec.agentNamespace` 仍是成员关系权威来源。租户独立于 namespace 验证，并继承到附属资源。标签/注解只能检索，不能证明所有权。派发前验证类型化绑定、精确 Job UID 和租户；活动 Job 在 Task 或清理完成前不得换控制器。直接 Kubernetes API 和扩展 REST 使用同等准入校验。预览版拒绝 Kubernetes/容器混合 Job，原生 Kubernetes Job 行为保持不变。

对于每个由扩展拥有的原生 Task，扩展会在创建 `ContainerAllocation` 之前把一个规范化调度目标持久化到 `containers.rlark.io/allocation-target` 注解。注解包含 Task UID、原生 Node UID、ContainerNode UID、Cluster UID 和节点实例标识。它是持久化的调度意图，不是预留证明；真正的所有权锁仍由 allocation 提供。重试或第二个控制器只能复用完全相同的目标，在创建或派发结果不确定时绝不能换节点。只有在 allocation 具有匹配的清理证明且 Task 正在以新 UID 替换时，才能清除目标。Task 删除、暂停和配置替换在 Agent 报告清理前都保留目标及 allocation 身份。生命周期从当前状态持续维护：`suspended` 和删除会阻止新启动，心跳和 Agent 结果只是观测，不能单独释放锁。

Raw supervisor 即使尚未启动该 Task，也会在停止时持久化一个仅含身份的取消墓碑。同一 Task UID 的延迟启动必须被拒绝，包括 supervisor 重启后。已有但不确定的进程不能走“未启动”路径，必须在确认清理前保留分配。取消记录持久化失败会返回错误，不能上报清理完成。此预览版保留墓碑，不自动回收。

需显式启用的预览 Job 预留控制器为每个 Job 创建并预留一个 `raw.rlark.io/process` v1 模板。在各自派发契约实现前，拒绝 Ray、多模板、旧版绑定、显式运行时实例名称、SSH 注入、用户指定的 GPU 可见性环境变量、环境处理器和 Pod 模板。原生 Task 名称确定，严格校验 owner reference、父 UID、模板身份和租户，不接管旧格式数据。节点选择器先校验，再匹配原生 Node 标签。工作负载或重启变更在确认清理后以新 UID 替换 Task，不就地修改运行中的执行。暂停/删除保留 Job 和 Task finalizer，直到每个精确 allocation 清理完成且观察到删除。目标已固定但 allocation 不存在时，除非控制器已保存精确清理回执，否则继续阻塞：仅凭不存在不能排除仍在进行的创建或启动。释放前，控制器在 Task 的 `containers.rlark.io/allocation-cleanup-receipt` 中持久化 allocation UID 和完整目标身份，以便从删除响应丢失中恢复。孤儿 allocation 阻止 Job 删除；含糊数据需要运维显式恢复。调度决策使用非缓存 API client 和乐观并发。Job phase 保持 Pending，`Scheduled` 原因为 `ReservedAwaitingDispatch`；停止的 Job 只在清理后变为 Stopped。控制器负责预留，API 和 Agent 上还需分别显式开启执行。当前可上报 Task 状态，但 Job 运行/结果聚合及终态 allocation 自动释放尚未实现。进程结束后仍保留预留，直到 Job 暂停/删除；不能把 Pending Job phase 当作实时执行结果。

独立控制器默认在 `rlark-system` 中启用 Kubernetes Lease 选主（`--leader-elect`、`--leader-election-namespace`）。使用 `config/rbac/container-runtime-controller.yaml`，并配置对应 namespace 和管理 API kubeconfig；namespace 必须事先存在。默认 `--enable-job-reservations=false` 仅安装校验控制器；隔离的预览测试可通过 `--enable-job-reservations=true` 启用预留。请用 `Job.spec.suspended` 暂停整个 Job；此阶段拒绝单独暂停的模板。可选进程内 `ControllerExtension()` 仍仅执行校验。同一节点不能运行旧预览分配写入者。撤销权限或关闭控制器不会清理或释放持久化预留；重新启动后继续调谐。预留测试覆盖恢复、容量不可用、配置变更、删除、租户冲突和 API 响应丢失，不代表 Agent 执行或端到端训练已支持。

控制器拥有分配意图和派发，Agent 拥有运行时观测与清理证据。派发前用乐观并发持久化预留，重启从当前状态恢复，心跳丢失不释放。每个 Raw 容器或 Docker 主机一次只接收一个 Task，当前分配契约要求显式的授权 GPU 白名单。GPU 只使用已授权整卡 UUID，可见性不等于授权。清理未知阻止复用，日志/遥测/审计不写入资源 Status。

### 独立服务与运维界面

已实现的执行预览为独立扩展 API 增加需显式启用的节点限定 Task 通道。经过验证的 Agent 证书是唯一请求身份；读取、元数据认领、状态报告、启动授权和清理报告都重新校验当前凭据、companion/原生 UID 引用、独立租户身份和精确 allocation。Agent 只收到自己的 allocation 和 Task，不持有原生 kubeconfig，也不能执行 discovery、watch、Secret、代理或任意资源操作。Task 认领写入限定为 Host 拥有的注解/finalizer，并要求调用者观测的 resourceVersion；状态写入保留控制器字段，必须匹配当前 Task UID 和 generation。运行时副作用仍通过现有 Host/Coordinator/Raw Provider 和私有 supervisor。通用 Host 的元数据依赖已收窄到实际使用的 Get/Patch 方法，使扩展可注入真实的限定传输，不实现全功能 Kubernetes client。执行 API 仅校验已有 allocation，不创建锁或修复分配目标。Task spec 按 JSON 语义比较，保留精确数值并容忍 API 调整对象字段顺序。

每次进程启动前，API 校验 Job/Task 当前未暂停、generation 与分配一致、唯一的类型化 Job/Cluster 绑定及有效凭据。节点替换、旧预留、禁止调度/排空、心跳过期、版本不匹配或选择器变更均阻止启动。decoder 仅从不可变 allocation 的授权 GPU UUID 注入 `CUDA_VISIBLE_DEVICES`，拒绝用户覆盖 GPU 可见性。清理使用独立身份路径：暂停、删除和心跳过期阻止启动，但不能阻止精确拥有节点的 Agent 报告清理。即使 Task 从未启动，也必须先由 supervisor 持久化取消，再接受清理证明。supervisor 不可用或后代未确认退出都不能推断清理完成。API 暂时故障会从当前状态重试，不因此停止运行中的可信工作负载或释放分配。这只是单进程执行阶段；Ray 整组、注册、日志、升级和 UI 保持各自验收门槛。

构建独立扩展控制器/API、节点 Agent、supervisor 和管理工具。节点显式加入指定容器 Cluster，使用有限 bootstrap 凭证注册，再用节点级认证主动出站；不需要公网 IP 或控制面入站。Raw 在已有容器内执行可信 root 进程，Engine 使用宿主机 Docker socket，绝不 Docker-in-Docker；两类能力分别展示。

提供独立构建的“容器集群”页面，并只向 RLark 接入最小导航入口。展示集群身份/运行时/连接、节点 Ready/可调度/占用、容量/分配/利用率、授权 GPU、预装环境、Job、Agent 版本和升级失败。管理员可创建/加入、生成接入包、禁止调度/排空和发布目标版本；Job 显式选择 Cluster，服务端校验能力。

控制面托管不可变 Agent 产物。节点认证下载，校验摘要、平台和协议，原子切换并保留旧版本。目标版本是持续期望状态，每个 Cluster 同时最多升级一个空闲节点；切换前阻止新分配，健康检查失败回滚。Agent 升级不重启 workload supervisor。固定主机支持 systemd，无 systemd 开发机使用前台 root launcher/updater。

### 迁移与验收门槛

首个独立控制面阶段使用通用 Job 控制器声明（`rlark.io/controller-owner`），并以类型化 ContainerJob 绑定校验。即使扩展不在线，原生控制器也会对声明外部主人的 Job 拒绝派发。校验控制器检查 UID 与权威 namespace 引用；它不授权工作负载副作用，也不把已登记节点标为可调度。AgentRelease 元数据通过校验不代表产物可用。等价准入安装前，扩展 CRD 仅允许可信管理员写入。在分配器、节点限定 Agent 通道和清理调谐完成前，不把活动工作负载切换到附属 API。

扩展新增了基于 API 的 Node 独占分配原语。它用原生 Node UID 派生的确定性 ContainerAllocation 名称创建锁，并校验权威成员关系、租户、Node/Job/Task UID、Agent generation/就绪、心跳时效、GPU 白名单、私网地址、重复 GPU/地址范围以及原生旧 reservation 是否存在。重复 Acquire 只恢复完全相同 Task 的分配；并发竞争者在 API 创建冲突时失败。Release 必须有针对精确 allocation UID 的 Status 清理证明，并在观察到删除前保留 finalizer。独立 mTLS 服务的 `POST /v1/heartbeat` 仅接受已验证且携带容器 Agent 角色的证书；它会校验签名的 companion UID、租户、凭据、原生绑定、私网地址和授权 GPU 可见性，然后更新有界 ContainerNode 状态。Linux 前台 Agent 二进制现在观测当前 cgroup v2 边界、可见 GPU UUID 和私有 Raw supervisor 占用，并发送显式 `busy` 字段；观测或 API 暂时不可用时使用有界退避重试，supervisor 或 GPU 观测失败时拒绝上报，因此最终表现为心跳过期和调度屏蔽。周期修复保留新鲜 Agent 观测并屏蔽过期心跳；分配检查当前凭据身份和节点忙碌状态。API 二进制和最小权限 `config/rbac/container-runtime-api.yaml` 已提供。注册、证书签发、版本分发和完整 Job 生命周期仍未完成；单进程派发仅支持显式开启的预览。TLS 部署、CA 分发和证书签发/轮换仍由运维方管理。

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

实际 cgroup 配额为 20 CPU（`cpu.max = 2000000 100000`）和 236 GiB（`memory.max = 253403070464`）。虽然存在 `cgroup.kill`，cgroup v2 文件系统为只读，不能使用 cgroup supervisor 后端。没有 Docker socket 或可用 systemd。状态/工作目录尚未安装，父目录可写。可信 root 进程树后端必须通过自身预检与生命周期测试后再使用，它不提供任务隔离。`/ks3-regular` 在平台本地目录上覆盖了实际读写 `fuse.ks3fs` 挂载，`/kpfs-regular` 为读写 `kingstorage` NAS 挂载；尚未测试跨重启 checkpoint 保留。

资源池清单显示分配一张 GM402，但容器内 `nvidia-smi` 显示一张 H100 80GB；在把 GPU UUID 作为授权容量前需确认平台型号映射。已通过私有跳转路径在该开发机上以 root 运行可选 Agent 观测测试：process-tree supervisor socket、cgroup v2 限制（`20000m`、`253403070464` 字节）和一个 GPU UUID 均成功读到；关闭 supervisor 后观测会 fail closed。这只验证本地观测边界。该开发机尚未登记为扩展 Agent。证书/接入、Job 派发、授权日志和 Agent 升级/回滚仍需实现并完成端到端测试。本轮未启动 RLinf 训练。

2026-10-30 稍后查询发现开发机已停止，自动保存成功。本轮重新启动时未变更队列、资源规格或公网配置。私网 Pod IP 仍为 `10.0.3.85`，宿主 Node IP 变为 `10.0.2.72`，因此旧宿主地址不能标识执行身份。重启后预装 pi05 Python 仍报告 Ray 2.56.1、PyTorch 2.11.0+cu126。以 root 运行的 supervisor 定向测试通过：启动前取消、取消持久化失败及重试、无效/取消请求，以及真实 supervisor close/open 后恢复取消记录。已移除临时测试二进制。这些测试未启动训练进程，不证明 checkpoint 保留，也未注册 Agent 或派发 Job。

### 前台 Raw 观测 Agent（preview）

`make -C extensions/container-runtime build` 在 API/controller 和 `raw-supervisor` 之外构建仅支持 Linux 的 `container-runtime-agent`。该命令默认只上报观测，显式开关可启用下文的限定 Raw 进程执行。它不注册节点、不签发证书、不传输日志，也不安装升级。先创建并验证原生/companion 绑定，独立安装可信 root supervisor，并由运维配置专用 Agent 证书：签名元数据必须包含精确 ContainerNode namespace/name/UID、原生 Cluster UID、容器实例、租户及当前 `containers.rlark.io/agent-credential` 注解。禁止向 Agent 签发原生管理员证书。专用 CA 私钥保留在 API/Agent 之外，API listener 仅配置 CA 证书。这些接入步骤仍由运维管理，尚无一键接入。

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

CPU 观测取进程 affinity 与当前 cgroup 到 cgroup-v2 挂载点之间所有可读祖先 `cpu.max` 的最小限制；内存取可见层级中最小有限 `memory.max`，全为无限时不报告内存。`--cgroup-mount` 默认 `/sys/fs/cgroup`，经 `/proc/self/cgroup` 解析当前成员路径，拒绝非 v2、缺失或格式错误的边界。它们是可见上限，不代表利用率、空闲量、隐藏祖先限制的保证或任务级限额。可见 GPU 不改变运维授权的 UUID 白名单。该前台命令当前仅观测 Raw；复制出的 Docker Engine observer 尚未接入该命令。

本地测试覆盖观测失败、API 故障恢复且不重放旧观测、取消、严格占用响应、祖先配额、无限/无效限制、重复 GPU 输出、mTLS 授权及重定向拒绝。在准备好的可信 root GPU 容器上，可选 `RLARK_TEST_LOCAL_OBSERVATION=1` 测试会运行真实 supervisor socket、cgroup 和 GPU 探测，不启动工作负载。这些检查不代表生产调度门槛完成。

2026-10-30，同一私网开发机通过 `TestScopedExecutionRealSupervisor`：真实 mTLS 请求、Host/Coordinator、私有 supervisor Unix 传输、可信 root 进程启动、Agent/Host 重建、allocation GPU 环境注入及暂停/清理证明。管理 API 为测试替身，工作负载只使用 CPU；注入的 `GPU-a` 是测试标记，不代表授权设备。开发机仍为 `regular-training`、1 GPU、20 CPU、236 GiB、无公网 IP。临时二进制已移除。这是执行组件证据，不代表已接入的生产 Job/GPU 验收。

### 限定 Raw 执行预览

运维手动配置精确原生/companion 绑定、专用节点证书及私有 supervisor 后，在 API 和 Agent 命令上**同时**加入 `--enable-task-execution=true`，控制器仍需 `--enable-job-reservations=true`。在 API 基础 RBAC 之外应用 `extensions/container-runtime/config/rbac/container-runtime-api-execution.yaml`；新增角色只允许原生 Job/Task 读取、Host Task 元数据/状态补丁、companion 绑定/allocation 读取和 allocation 清理状态补丁，不授予 allocation 创建/删除、Secret、exec、签名或 impersonation 权限。执行路由默认不存在，仅观测不需要额外 RBAC。

版本化预览 `POST /v1/task` 通道仅允许针对证书绑定的 allocation 和 Task UID 执行 `assignment`、`get`、`authorize`、`claim`、`status`、`cleanup`。严格且有界的 JSON 拒绝任意资源和字段。认领补丁先在内存中与 Host 允许的精确元数据比对，再由服务端重建带乐观锁的写入。类型化状态仅修改当前 Task 实例的 Host 字段/条件。Agent 不持有管理面 kubeconfig。启动前 API 检查当前 Job 模板、重启标记、凭据、心跳、版本、禁止调度/排空和保留的 allocation；命令或归属变化均拒绝。

暂停/删除是持久化清理请求。Agent 先调用私有 supervisor 的幂等 Stop，包括尚未启动的情况，验证精确身份、持久化取消和后代清理后再报告。API 先移除 Host 拥有的认领元数据，再发布 allocation 清理证明；锁释放和 Task 分配保护归 Job 控制器。认领/清理响应丢失后从当前状态恢复。本地观测失败时只协调清理，不发送伪造心跳或启动任务。API 故障保留工作负载和 allocation。撤销/替换的凭据不能报告清理；需恢复精确所有者身份或走独立可审计的运维恢复，不能直接删除保护。

测试通过真实 mTLS 传输搭配假管理面和 supervisor，覆盖跨节点/租户/UID、旧 generation/心跳/版本、伪造字段/状态、Host 重建、GPU 注入、认领提交后响应丢失、部分清理/响应丢失，以及观测失败时的清理。这些测试不代表已部署的云端 Job 验收。Job phase/结果聚合、进程结束后自动释放、日志、接入、升级及 Ray 组仍属后续工作。

### 交付门槛

- [x] 中英文文档记录独立扩展、指定队列和验收范围。
- [x] 独立模块/构建，迁移 Provider/supervision/测试；加入原生可选集成接口和所有权校验。
- [ ] 类型化附属 schema、授权/准入和迁移。通用 Job fail-closed 声明已实现，但还没有生产调度器接管 Job。
- [ ] 显式容器集群接入、节点限定身份、整卡分配和授权日志。
- [ ] 认证版本托管、空闲节点滚动升级及不重启 supervisor 的回滚。
- [ ] 容器集群 UI、能力门控 Job 提交及中英文 API/运维示例。
- [ ] 已授权私网 1 GPU 开发机和完整 Raw GPU Job/升级证据。
- [ ] 恢复/竞态测试、仓库 Go/Web 检查、生成验证和严格双语文档构建。

测试并发预留、旧 UID/generation/实例、跨租户访问、重复注册、部分外部动作、重启、supervisor 丢失、清理失败、心跳丢失、摘要错误、升级竞争、回滚失败、迁移冲突、checkpoint 保留及 Kubernetes 行为不变。不能凭骨架或 mock 勾选门槛。

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

### 按依赖顺序完成接线

1. 完成生产所有权约束：限定 bootstrap/注册、唯一 Node/实例身份持久化、精确状态写权限、Lease、cgroup/affinity 容量，以及跨所有组的权威 GPU 绑定。证明两个 Agent 不能冒充同一 Node。
2. 增加运行时无关副本校验及旧字段冲突处理，将 Raw 模板展开为稳定副本 Task。先创建暂停/未绑定 Task，再分配 UID、持久化全部绑定，最后允许分发；Job 控制器重启从当前状态重建此流程。
3. 通过 Host/Coordinator 组装 Raw decoder、Ray adapter、provider、生命周期和状态映射；拒绝不支持的 Kubernetes environment、carrier 和 Pod 模板功能，并对已有 Kubernetes 路径做回归验证。
4. 实现类型化 Ray 配置、预检/就绪屏障、有界超时、按网络范围预留端口、精确成员验证和训练结果。全部分配成员验证前禁止训练；区分 Unknown 与失败，确认组失败后停止全部成员。
5. 完成清理/重试所有权：Job 删除/重启或部分分发时，所有 supervisor 确认清理前保留预留。为 supervisor 丢失提供可审计运维恢复；心跳消失不释放容量。
6. 完成前台注册/启动包、授权日志路由及有界保留、资源观测、UI 能力开关与副本进度。链路可用后才添加可执行中英文部署/训练示例。
7. 在两个准备好的容器验证显式选择的监督后端、授权 GPU UUID 和支持的 Ray/RLinf 版本；覆盖端口占用、后代清理、Agent 重启、supervisor 丢失、断网、head/worker 失败和 checkpoint 保留。本地尚未执行这些平台测试。

### 本地验证证据

内部组件通过定向竞态测试和仓库 `make test-go` 测试，supervisor 二进制构建成功，API/client/UI 生成模型已重新生成，中英文文档通过严格 MkDocs 构建。尚未完成真实 GPU、委派 cgroup 或双容器 Ray/RLinf 验收。仓库 Go lint 仍报告网络包中三个已有的未检查 channel 接收；新增 Raw 包未报告 lint 问题。这些证据仅验证实验基础组件，不代表上述交付清单完成。

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

Docker 预览路径已用 root Agent、宿主机 Docker socket 和 Docker Engine API 1.44+ 验证。验证覆盖 Agent 启动、Node 容量与 incarnation 观测、持久化预留校验、容器所有权及可重试清理。真实多主机 GPU Ray 训练、Gateway 日志/stats 路由、systemd 安装包生成和按设备 UUID 分配仍不属于已完成契约。

单主机 Ray 冒烟 Job `docker-validation-20261030` 已成功退出（exit code 0），日志包含 `RLARK_DOCKER_SMOKE_OK`。删除 Job 后确认对应 Task、Docker 容器和 Node reservation 均已清理。当前每台 Docker 主机只接收一个 RLark Docker Task，纯 CPU Task 也遵循此约束。
