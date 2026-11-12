# Station API 契约（试验性，尚未安装）

> **名字（2026-11-12）**：本文的 `Station` 对象在界面和对外接口里叫**场站节点（Site Node）**：一台机器在场站里的长期身份和入网记录。"Station"留给 Fleet 一个 Run 里的 station（论文的定义）。CRD 组 `station.rlark.io`、kind `Station*` 和可执行文件名是沿用的代号，迁移方案和入网设计见 [Station v2](station-v2.zh-CN.md)。

本文定义 `extensions/station` 中已登记的 `station.rlark.io/v1alpha1` 类型和生成 CRD。它仍是试验性扩展 API：CRD 尚未安装到生产 kcp 或成员集群；controller manager 与 Station API 可执行原型及示例 Deployment 均已存在，但尚未部署或达到生产就绪。

## 范围与 namespace 映射

五种资源均为 namespaced。可信注册记录把一个稳定 cluster ID 映射到成员 Kubernetes API 客户端和一个 `Cluster.spec.agentNamespace`。namespace 是资源放置边界，不等于集群身份。两个活动注册不得声明同一个 namespace。稳定 cluster ID 写入 `StationSequence.spec.clusterID` 并由服务端解析；请求 namespace 或标签本身不能证明成员关系。

## 资源与写入方

| 资源 | 写入方 | 契约 |
| --- | --- | --- |
| `Station` | `station-controller` | 长期身份记录。`metadata.name` 是由可信 cluster ID 与已验证 Node UID 派生的确定性 128-bit ID，重复/重试发现会收敛到同一对象；它不是认证凭证。`metadata.uid` 标识 Kubernetes 对象实例。Spec 保存显示名、集群内序号、当前 Node 名称/UID 绑定、绑定版本和不可逆退役期望状态；Status 保存 phase、启动时间、最近上报时间和 conditions。 |
| `StationObservation` | `station-api` | 每个已验证 Node 名称仅保留一份可覆盖的最新快照。API 先创建空 `spec` 对象，再通过 `/status` 写观测数据。服务端 annotation 绑定 Cluster ID 与 Node UID；bootstrap 时还没有 Station 对象 UID，Station 创建后再一次性写入。`status.reportedAt` 是服务端接收时间，不能用客户端时钟判断存活。快照包含当前 Node UID，避免同名重建节点被误认为旧绑定。 |
| `StationSequence` | `station-controller` | 每个已注册集群一个序号分配器。`status.lastAssigned` 使用 resourceVersion 比较并交换递增；允许跳号，永不复用。`spec.clusterID` 不可变。 |
| `StationRebind` | 管理员 Gateway 接口，随后由 `station-controller` 处理 | 不可变的一次性操作，包含稳定 request ID、精确 Station 对象 UID、期望 binding 版本、目标 Node 身份、已认证请求者和原因。仅管理员可调用的 Gateway 接口解析 Cluster namespace、核对页面显示的 Station UID/版本，并派生确定的 operation 名称。相同 request ID/载荷重试返回既有操作；同 ID 不同载荷返回冲突。controller 会从成员 API 独立核验目标 Node UID。更改 Station Spec 前 controller 先记录 `status.previousBinding`，然后 Status 在 `Accepted`、`Applied`、`Rejected` 间流转。操作状态不是审计日志。 |
| `StationAuditEvent` | `station-controller` | 与 Rebind 状态分离的不可变追加式安全记录。名称由操作对象 UID 确定性派生。Create 重试核对已存事件并拒绝冲突载荷；首次服务端时间保留。运行时 RBAC 仅授予 create/get，不授予 update/delete。 |

所有资源使用注册记录指定的 `agentNamespace`，不设 cluster-scoped 资源。Node 名称、systemUUID、标签和源 IP 只能作为辅助证据。首次心跳可在服务端完成 TokenReview 并新鲜读取 Pod、ServiceAccount 和 Node 后接受，即使 Station 尚不存在，以便 controller 创建首个 Station。此 bootstrap Observation 仅归属已验证的 cluster ID 与 Node UID；Station 创建后，challenge 和敏感操作必须同时匹配当前 Station 对象 UID 与 binding version。Node UID 变化不得自动接管。

## 校验与生命周期

生成的 CRD schema 限制字符串和列表长度、要求 binding version 与 Station sequence 为正数、probe RTT 非负，并限定 phase enum。CEL 规则令 Station 显示名和序号不可变；binding 变化必须且只能递增一个版本；sequence 的 cluster ID 不可变、计数不下降；rebind 请求 spec 创建后不可变。API Server 是 schema 校验权威，客户端不得依赖 UI 校验。

Observation 是最新状态快照，不是历史。API 在写入前须限制请求体大小和速率，并依据新鲜成员集群身份授权。Station 不存在时，只允许普通心跳端点提交已验证 Node 身份，敏感操作不可用；Station 创建后，API 还要求当前 Station UID 和 binding version。Controller 根据当前资源和时间戳计算 `Observed`、`IdentityMatched`、`RLarkLinked`、`NetworkVerified` conditions。Spec 驱动结果才设置 `ObservedGeneration`；外部健康状态不能一概按 generation 不匹配判为陈旧。

退役是持久期望状态，退役 Station 不因心跳而重新激活，CEL 阻止撤销退役。API 不提供硬删除端点。生产安装前仍需决定 Rebind 操作及审计事件保留期、Station 删除/finalizer 策略，以及 Node 改名后的 Observation 清理规则。

## Controller 原型

`StationReconciler` 只从唯一映射的 `Cluster.spec.agentNamespace` 读取当前 Observation；按 cluster ID 与已验证 Node UID 生成确定性 Station 名称；通过 status 比较并交换分配永不复用的序号，并从当前快照刷新 Station conditions。同一 Node UID 多重绑定或 Node 名称冲突会标记 Conflict。同名新 Node 不会更改旧 binding，也不会自动创建替代 Station。90 秒阈值采用严格小于：到达 90 秒整即为 Offline。Observation 缺失时保留 binding 并标记 Offline。状态更新通过新鲜 API reader 并有界重试冲突。

`RebindReconciler` 先把旧 binding 快照写入不可变操作的 status，再向注册成员 API 验证目标 Node，并条件递增 binding version 一次。操作进入终态前会先写入独立 `StationAuditEvent`。并发重复 reconcile 会识别同一目标已应用并收敛到一个审计事件。若进程在 binding 更新后退出或审计存储不可用，操作保持 Accepted 并重试；审计事件 Create 必须幂等。controller 入口和示例 RBAC/Deployment 已加入，但生产 kcp 身份及权限尚未验证。

应用重绑前，controller 会新读注册 namespace 中的 Station 列表；若目标 Node UID 已绑定给其他 Station，或没有先前 operation 快照却请求绑定到当前 Node UID，则拒绝操作。Station 发现和 Rebind 协调器在 leader-elected manager 内共享可取消的串行协调器，因此活动 manager 中的列表/创建/绑定转换不会并发竞争。race 测试让两个独立 Rebind reconciler 和 Station 发现同时争用一个 Node UID，验证最终只有一个 owner。下文记录真实 API 协调和有限的 leader 交接测试；身份变更过程中的 leader 故障仍未测试。

协调器在获取锁之前拒绝已取消的 context，并在拿到锁后再次检查；若取消与获取同时发生，会释放锁并返回取消错误。回归测试覆盖锁被持有和空闲时的已取消请求，并验证其他调用者随后仍能获取锁。获得锁之后的取消由 reconciler 负责，其 API 调用使用同一个 context。该进程内锁不是分布式 fencing 机制。

2026-11-11 在一次性 kcp workspace 安装五份 CRD 后运行可选 controller 集成测试：真实 Observation status 驱动 Station 创建和重复协调；Rebind 将 binding 递增一次、记录旧 binding 与一条 Kubernetes 审计事件，并进入 Applied；Station 可退休且不能撤销退休。首次测试发现 CEL 退休规则在 status 更新时直接读取被省略的可选 `retired` 字段，导致 API 拒绝；现已在类型源中添加字段存在性检查并重新生成 CRD，完整真实 API 测试通过，临时 workspace 已删除。测试直接调用 reconciler，Cluster 注册解析和成员 Node 验证使用替身；未验证 manager leader 选举、成员凭据、生产 RBAC 或部署。

同一真实 API 测试现会在 binding 更新后注入审计故障，验证旧 binding 已保存而操作尚未进入终态；随后重建 reconciler 和协调器，使用真实 Kubernetes 审计 sink 重试。重试没有再次递增 binding 版本，最终仅有一条审计事件且操作进入 Applied，再次重试保持幂等。race 模式下该测试在一次性 kcp workspace 通过，workspace 已删除。重建进程内对象不等同于 manager leader 故障切换测试。

Station 序号、binding 和状态 patch 现显式携带 resourceVersion 前置条件；原先普通 merge patch 实际未实现预期的比较并交换。隔离 kcp 测试验证过期序号更新被拒绝，六个各自使用独立协调器的并发 Station 创建获得不同序号，测试 workspace 已删除。这保护同一对象的并发更新；不同 Station 对象之间的唯一绑定仍依赖活动 manager 协调，不构成分布式 fencing。

另在一次性 kcp workspace 运行两个共享 Lease、使用缩短测试租约的 controller-runtime manager：首个 manager 正常取消后，另一个获得领导权，leader-only 探针未观察到重叠运行。race 测试通过，workspace 已删除。该测试覆盖可协作探针的正常交接，不覆盖进程强杀、网络分区、旧 leader 卡住、生产租约时间或 Rebind/Station 协调过程中的切换。Kubernetes 选主与进程内协调器不构成分布式 fencing 保证。

目标 Node reader 从管理面 Secret 读取 kubeconfig，Secret 名称为 `MemberKubeconfigSecretName(clusterID)`，且 `station.rlark.io/cluster-id` 注解必须精确匹配。它将 Secret 数据复制到内存，由 client-go 验证 TLS，每次操作重新读取 Node；Secret 内容变化时替换缓存 client。成员主机上不放配置文件。限制 controller 对专用凭据 namespace 的 Secret 读取权限；每份 kubeconfig 需成员端最小权限 RBAC，只允许读取目标 Node。示例 Deployment 从 Pod 内 Secret volume 读取管理面 kubeconfig；清单不含凭据，生产 kcp 身份与权限尚未验证。

## 当前实现证据

`extensions/station/api/v1alpha1` 已包含 Go 类型、scheme 注册和生成的 deep-copy 方法；`extensions/station/config/crd/bases` 有五份使用仓库固定 `controller-gen v0.18.0` 生成的 CRD。2026-11-11 在临时 kcp workspace 安装五份 CRD 均达到 Established。Admission 接受有效 Station、Sequence、Observation、Rebind 和 AuditEvent；Observation status 与 Rebind `previousBinding` status 可写入；CEL 拒绝 Station 显示名修改、binding 错误递增、退役恢复、sequence 倒退、Rebind spec 改写和 AuditEvent spec 修改。临时 workspace 与 kubeconfig 已清理。这不能证明 APIExport/APIBinding 权限、生产 RBAC 或 manager rollout；生产安装前仍需验证。

controller 当前含 Observation 状态协调、Rebind 状态机、独立 Kubernetes AuditEvent sink、唯一 Cluster namespace resolver、RLark Node Ready 查询及成员 kubeconfig Secret reader。RLark identity 包新增动态注册表和 Kubernetes Observation sink：按当前 Cluster 的唯一 `spec.agentNamespace` 解析作用域，从带 cluster-id annotation 的专用 Secret 读取 member kubeconfig 和 station-agent ServiceAccount UID；Secret kubeconfig 变化时刷新内存 client。首次已验证上报可先以 cluster/Node UID 创建 Observation，由 controller 创建 Station；之后的上报通过独立的 resourceVersion 条件 metadata patch 将 Observation 绑定到精确的 Station 对象 UID，再写 status。不同 Cluster/Node UID 或既有 Station UID 不匹配均拒绝接管。`station-api` 可执行原型已加入 HTTPS、PostgreSQL challenge store、Observation 写入、有限内存的逐 Node UID rate limit，以及独立 health/readiness；示例 Deployment 和管理面/成员集群 RBAC 也已加入，API 示例固定单副本。Station binding 创建前 challenge 和敏感操作返回 403。Rebind controller 会先保存旧 binding，再校验目标 Node UID 和期望版本、应用一次版本递增、幂等写入独立不可变审计资源，最后才结束操作；并发重复请求也收敛。race 测试覆盖并发重复 reconcile、身份冲突、超时恢复、依赖故障恢复、Rebind 版本冲突、Station 对象替换、binding patch 后审计故障恢复、并发幂等及 identity registry/sink 的 bootstrap、归属、轮换和冲突行为；handler 级 fake Kubernetes 串联测试覆盖首次上报、Station 创建、UID 绑定及 challenge 签发。Gateway 管理员重绑接口已实现操作创建/查询，并通过 fake Kubernetes 与 JWT 权限测试；CRD/controller 未部署，尚未进行现场 operation。manager 与 API 均未部署；生产 kcp 身份、准确 RBAC、凭据 Secret 轮换、对外 TLS/网络入口、真实 CRD status 写入、敏感操作授权和审计查询仍需验证。

试验性 `StationReconciler` 只消费可信注册 namespace 中当前的服务端验证 Observation。Station 名称由 cluster ID 与 Node UID 确定性派生，重复和并发首次通知收敛到同一对象；如果进程在预留序号后崩溃或并发创建输掉竞争，序号允许留空洞。状态更新会基于 API 新读对象并有界重试冲突；90 秒是心跳有效窗口，同名 Node UID 替换只进入 Conflict，不会改绑定。`RebindReconciler` 和 Kubernetes AuditEvent sink 已有原型。race 测试覆盖重复/并发通知、UID 冲突、精确超时及恢复、Observation 缺失、退役、序号归属冲突、readiness 依赖恢复、Rebind 版本/对象 UID 冲突、审计故障恢复和并发重复 Rebind。manager 可执行入口及示例容器/RBAC manifest 已加入，但尚未部署；生产 Node/read API 权限和 kcp 认证方式仍待验证。

在 `extensions/station` 目录运行 `GOWORK=off go test ./...` 和 `GOWORK=off go vet ./...`。重新生成时使用仓库固定版本，不能换 controller-tools 版本。
