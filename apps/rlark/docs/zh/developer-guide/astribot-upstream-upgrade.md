# Astribot 上游 RLark 升级核对

本次核对范围为 `9ad4a21..d06745e`。功能变更来自 `8c3a72d`，`d06745e` 修正发布流程并补充 v0.1.0 变更日志。Astribot 分支包含 `d06745e`，也包含该区间新增的全部 211 个路径。以下分别记录分支集成和部署环境验收。

| 领域 | 分支集成 | 环境验收 |
| --- | --- | --- |
| 数据面批量升级 | 共用升级页面、Gateway API、两个 CRD、Controller Manager 协调器、Agent 执行器均已保留。Astribot 管理面身份已补齐 Gateway 访问升级任务，以及 Controller Manager 协调和上报升级任务所需权限。数据面 Agent 在目标集群命名空间的管理面角色已包含子资源和状态权限。 | 使用前先应用 CRD 和 RBAC。使用审核通过的 `rlark-upgrade` 镜像在可丢弃的 Kubernetes 测试集群验证并发、超时、失败阈值和回滚。扩展至 Astribot 管理的集群前，核对 Agent 参数、挂载、凭据和拓扑。 |
| 节点生命周期 | 节点管理保留心跳、可调度状态、暂停调度、排空进度、取消排空、恢复和取消纳管。排空及恢复调度的确认框明确了被排空的任务需要显式启动对应 Job。Astribot 的场站节点移除仍走独立流程。 | 在空闲测试节点验证排空完成、恢复调度后旧任务仍停止、显式启动 Job 后任务恢复。另行验证场站节点移除。 |
| 调度和诊断 | 创建任务保留按集群、节点、容量和可调度状态选点，以及 `/dev/shm` 配置。Worker 保留调度、存储和拉取镜像等待原因。Kubernetes 所有权链校验和状态投影也覆盖 Astribot 的 DaemonSet 和逐节点任务实例。 | 用 Kubernetes 任务验证选点、Worker 等待与共享内存挂载，核对 Pod 和 Task 状态与页面显示。验证 DaemonSet 所有者 UID 不匹配和 Pod 替换时不会带入旧状态，且逐节点实例能更新和清除。 |
| Agent、故障恢复与部署 | 启动配置校验、依赖和权限检查、周期状态修复、所有权校验、Helm Chart 和发布流程均已保留。控制面 Chart 内的 CRD 和权威 Schema 一致。 | 在测试集群验证 Agent 重启自愈、磁盘压力恢复和控制器启动。在隔离环境渲染并安装两个 Chart，核对官方部署路径。Astribot 继续使用自己的部署流程。 |

Astribot 标准发布在 `crds-rbac` 阶段应用 CRD 和生成的管理面角色。Astribot 发布镜像清单已用上游构建规则纳入 `rlark-upgrade`。批量升级验收前需确认候选镜像已发布且可按摘要拉取。官方 v0.1.0 已发布镜像列表未包含它。官方升级 Job 会根据 `DeployConfig` 重建 cluster Deployment 和 node DaemonSet。带自定义挂载、启动参数或 embedded 拓扑的 Astribot 部署继续使用各自的更新流程，直到验证过升级和回滚能保留所需配置。上游 Kubernetes 工作负载路径仍处于预览阶段，Docker 和裸机任务执行尚未实现。

## 兼容分支实现进度

机器人操作、场站观测、相机和日志代理、手动扫描及型号目录在 `apps/rlark/astribot/stations/`。入网码签发和撤销、有效期校验、安装命令和记录转换在 `stationidentity/`。成员集群网络规划和机器安装脚本在 `provisioning/`。专用 Addon 目录、Agent 就绪判定、节点服务代理、任务适配器、存储声明渲染和 OCI 清单校验已归入相应的 Astribot 包。控制台扩展和测试分别位于 `apps/rlark-ui/src/astribot/` 和 `apps/rlark-ui/tests/astribot/`。

CRD 字段与生成产物、共享角色、鉴权和错误转换、组件接线及其集成测试随共享契约维护。已有安装通过同名同版本的 `csi-driver-rclone` 使用 Astribot 参数，因此该扩展仍在原 Addon 定义内。依赖共享组件状态的镜像仓库和 Agent／提供商代码保留各自的接入点。

兼容分支已纳入 v1.7.3 发布构建使用的 S1U 遥测源码。Gateway 内嵌的硬件能力定义与 S1U 镜像复制的文件相同。使用该分支发布前，在测试环境验收本体状态、两路相机及其延迟指标和手动扫描。

发布工具保留管理面的全站 Lease，同时在写入前按集群顺序获取成员 Kubernetes Lease。遇到冲突会报告持有者并释放本次已经获取的成员锁。使用新版发布工具前，管理员需给每个目标集群应用成员发布 RBAC 并预建 `astribot/release/member-upgrade-lease.yaml`。成员锁到期后仍须确认旧执行者已退出才能接管。旧发布工具只识别全站锁，应避免和新流程同时使用。

升级协调器通过驱动接口读取结果，接受对应升级容器的结构化失败信息，并过滤任意终止文本。原地换镜像执行器和 ConfigMap 检查点已覆盖模板保留、首轮回滚和回滚续跑的本地测试。隔离测试用的 `astribot-cluster-only` 模式已接通 Agent、独立 Job 和执行器。Agent 按成员的本地可信配置固定执行器镜像摘要，以子任务 UID 命名 Job，并挂载不可变配置。Job 使用独立 ServiceAccount，核对自身 Pod 和 Job 的 UID，在成员锁内只写本地 Agent Deployment 的镜像字段。检测到 node Agent DaemonSet 会拒绝执行。直接写入管理 API 的子任务也经过成员策略校验。Gateway 继续拦截标有 Astribot 驱动的批量任务，避免回退到上游重建流程。面向带节点 Agent 的 Astribot 集群，还需接入节点 Agent 镜像声明的受限写入接口和隔离集群完整升级回滚验收。

前端静态热更新使用 `astribot/release/build-ui-static.sh`，传入新的绝对输出目录。此入口与正式镜像使用相同的 IAM 构建设置，产物包含独立的 `login.html` 及其图片。发布后验证未登录跳转和已登录控制台。

要在无 node Agent 的隔离成员运行这条路径，先核对管理面的 `Cluster.spec.nodeAgents` 未开启。应用成员升级 Lease 与 `astribot/release/member-upgrade-cluster-only.yaml`，将 Role 的 Deployment 名称改为该成员实际部署名。成员管理员在 `rlark-system` 建立 `rlark-agent-upgrade-policy` ConfigMap，键为 `mode=astribot-cluster-only`、`deployment` 和 `executorImage`，其中镜像必须按 SHA-256 摘要固定。仅提交该成员的测试用 `ClusterUpgradeJob`，其执行器镜像必须与策略一致，DeployConfig 只允许 `kubernetes.agent-image` 和 `kubernetes.image` 两个摘要镜像字段。这个入口用于隔离验证。

已在隔离 Kind 集群使用真实 Kubernetes Job 和带 Agent 启动参数的测试工作负载验证执行器。正常镜像成功滚动，故障镜像进入 CrashLoopBackOff 后由独立 Job 恢复上一个镜像。无关启动参数保留，成员锁已释放。Agent 创建 Job 和不可变配置另有 Go 测试。

参考文档

- [上游集群升级](../admin-guide/upgrade.md)
- [节点管理](../admin-guide/nodes.md)
- [任务](../user-guide/jobs.md)
- [Agent](../admin-guide/agent.md)

Gateway、管理面控制器和升级后的成员 Agent 都要求集群明确设置 `rlark.io/upgrade-driver: stock` 后才允许使用上游安装器。声明了托管节点 Agent 的集群无法使用该安装器。若成员已有 Astribot 发布租约但未配置执行策略，Agent 同样拒绝启动。隔离测试的仅镜像模式在目标就绪后由 Job 持成员租约写入完成检查点。若本地 Job 消失，Agent 会确认检查点属于本轮子任务 UID，并核验相同 Deployment 的两个目标镜像摘要及实际就绪状态。找不到匹配的检查点时会报告结果无法确认。

机器列表和入网资格共用 `stations/machine_view.go` 中的转换逻辑。集群创建和 SSH 节点入网共用 `provisioning/bundle.go` 中的安装包传输、摘要验证和进度上报。Astribot 的 S3 挂载选项和端点校验归入 `storageconfig/mount_request.go`，复用上游端点标准化逻辑。Gateway 沿用现有授权和响应契约，再调用 Astribot 工作流。 升级目标被拒绝时的诊断信息，以及节点加入状态的注解转换，也由 Astribot 扩展负责，沿用现有 API 字段。

执行器将托管节点 Agent 镜像作为第三个独立目标写入本轮检查点。开放托管升级前，仍需接通管理面通道，仅允许对已核验的集群和任务比较并变更 `Cluster.spec.nodeAgents.image`，并核验成员集群的实际滚动结果。通过混合双集群验收后再开放 Astribot 批量入口。

### 托管节点 Agent 的升级边界

发布工具以当前阶段读取的状态为基线更新已启用的节点 Agent 镜像声明。它核对 Cluster UID，保留资源版本做条件写入，并在变更镜像前复核成员租约。镜像被改为其他值、Cluster 被替换或正在删除、或者声明已被禁用时，停止镜像变更。重试发现镜像已是目标值时，核对身份和租约后即视为成功。节点选择器、滚动规则及其他 Cluster 字段保留。首次启用声明仍由发布初始化流程负责。

仅集群 Agent 模式的 Job 使用本轮检查点执行仅镜像升级。Agent 校验本地 Job 名称、子任务 UID 标签和执行器 ServiceAccount 后才读取结果。上游安装器继续使用 `rlark-state` 作为回滚基线。仅镜像模式的 Agent 不会在 Job 释放租约后写入这份共享记录。

托管模式的管理面授权和成员滚动必须能在集群 Agent 重启或故障时继续。继续用 UpgradeJob 和 ClusterUpgradeJob 作为运维入口。管理面授权绑定 Cluster UID、子任务 UID 和已批准的目标镜像摘要，并在首次写入前由管理面记录回滚基线。成员 Job 使用绑定本轮任务的凭据。管理面独立核验成员 Job 和当前成员租约，仅开放受限的镜像操作，授权和凭据留在成员不可写的位置。成员 Job 不挂载通用 Cluster 写权限或全站发布凭据。节点 Agent DaemonSet 仍由集群 Agent 独占调谐。声明变更后，必须核验同一工作负载实例、目标镜像和本轮滚动，包括替换后的 Pod，才能判定成功。节点 Agent 镜像独立于集群 Agent 和工具镜像。

独立代理和 Job 适配器已可用于托管模式的隔离验证。在一次性成员集群中安装现有的执行器 ServiceAccount、检查点和租约，并应用 `astribot/release/member-upgrade-verifier.yaml`。验证身份能发起 TokenReview，并读取成员的执行器 Pod、Job、Lease 和节点 Agent DaemonSet。凭据只挂载给代理。运行 `rlark-upgrade` 时，把 `RLARK_NODE_IMAGE_BROKER_CONFIG` 指向一份 JSON 配置，其中包含 `listen`、`tlsCert`、`tlsKey`、`managementKubeconfig` 和 `members`。后者按管理面 Cluster 名称索引，每项填写 `clusterUID` 和该成员验证身份的 `kubeconfig`。代理的管理面身份只授予已批准的 Cluster 名称上的读取和更新权限。代理监听指定的内网地址并要求直连 TLS。

隔离子任务的本地 `rlark-agent-upgrade-policy` ConfigMap 设置 `mode=astribot-managed-node`、`deployment`、摘要固定的 `executorImage`、HTTPS `brokerOrigin` 和可信 PEM `brokerCA`。托管模式的 DeployConfig 要求 `kubernetes.agent-image`、`kubernetes.image` 和 `kubernetes.node-agent-image` 都固定到镜像摘要。可信管理面运维人员在启动该子任务前为 Cluster 写入 `rlark.io/node-image-upgrade-grant`。这份 JSON 授权包含 `ClusterUID`、`DaemonSetUID`、`ChildUID`、`Before`（已观测且固定摘要的节点 Agent 镜像）、`Target`（节点 Agent 目标）、`Deployment`（批准的 `Agent` 和 `Tools` 摘要）、`ExecutorImage`、`Expires`（RFC 3339，晚于 Job 截止时间以覆盖回滚）和初始值为 `Approved` 的 `Phase`。不要替换仍在使用的授权。Job 使用绑定 Pod、可轮换的专用受众令牌；代理通过同一次 Cluster 资源版本检查提交镜像和授权阶段，迟到的正向请求无法撤销回滚。代理核验同一托管 DaemonSet UID、预期模板镜像、已观测代际、已更新节点数和可用节点数。隔离模式要求所有已调度节点收敛。

托管 Job 被清理后，恢复流程会核验本轮子任务的完成检查点、实际集群 Agent Deployment、托管节点 DaemonSet 和 Cluster 上已应用的授权。过期授权仍可作为本轮只读证据。证据缺失或发生变化时，报告结果无法核验。开放批量升级前，用标准与托管成员完成混合批次验收，覆盖授权过期、资源替换、成员租约冲突、部分滚动和回滚，并确认成员无法变更其他 Cluster 或已批准镜像以外的字段。

独立 Kind 用例以真实 Kubernetes Job 运行托管执行器，使用投射的 Pod 绑定令牌、独立 HTTPS 代理和受限验证凭据。测试夹具将已授权的声明传给真实 DaemonSet，Job 等待滚动状态。故意停止同步后，滚动超时，执行器恢复原声明、保留节点选择器并释放成员租约。该用例尚未覆盖完整的 Astribot 节点 Agent 调谐器、生产 kcp 接入和混合批次调度。

### 混合批次入口

混合批次继续用共享的 `UpgradeJob`，在 `spec.configPatch` 中填写 Astribot `BatchDeployConfig` YAML，`apiVersion` 为 `astribot.rlark.io/v1alpha1`，`kind` 为 `BatchDeployConfig`。将 `stockDeployConfig` 设为上游 Kubernetes DeployConfig 全文，或显式设为空字符串以复用标准成员保存的配置。标准成员使用 `spec.image`。`managed` 下填写 `executorImage` 和 `deployConfig`，后者为包含三个已批准镜像的 YAML 字符串。控制器为每个集群创建上游子资源，子任务只收到自己驱动的配置和镜像。已回滚的升级也计入失败阈值。旧的标准升级请求继续接受原有 DeployConfig。

托管批量调度要求可信 Cluster 声明 `rlark.io/upgrade-driver: astribot-image-only` 和 `rlark.io/managed-upgrade-batch: enabled`。声明前先升级成员 Agent、配置托管策略、重新接入并确认可访问代理。代理配置设置 `autoIssueBatchGrants: true`，并在每个成员条目固定 `executorImage` 摘要。代理管理面身份还需要读取 UpgradeJob 和已选成员的 ClusterUpgradeJob，并能创建 SubjectAccessReview。首次收到持有成员租约的已认证请求时，代理核验成员有效权限、父子任务归属、配置及目标 Cluster UID，再根据实际已收敛的节点 Agent 基线签发授权。同一个子任务无法重新领授权。成员 Agent 只读子任务 Spec，可写状态；若旧 Role 仍可修改子任务 Spec，自动授权会被拦截。

本次变更没有为生产集群自动开启批量入口。生产启用前仍需核对目标 workspace 和成员 Agent 版本。

Kind 混合批次用例对标准和托管 Cluster 记录运行了真实控制面调谐器，验证子任务收到不同的配置，并使用真实 TokenReview 和 SubjectAccessReview 自动签发授权。托管 Job 完成了夹具 DaemonSet 滚动，并在第二次滚动超时后恢复声明。标准子任务已核验分发，尚未运行标准安装器。完整 Astribot 调谐器和生产 kcp workspace 仍需独立验收。Gateway 的机器准入已将请求校验、型号匹配和持久化委托给 Astribot stations 包。 独立 kcp v0.30.0 workspace 还验证了 SubjectAccessReview 能识别旧 Agent 的子任务 Spec 写权限，并在收紧 Role 后确认写请求无权执行。

### 完整混合批次隔离验证

独立 kcp v0.30.0 root workspace 加两个 Kind 成员验证了混合批次。测试在真实 API 上运行现有批量调谐器、两个成员升级调谐器、标准安装器，以及 Astribot 节点 Agent 和滚动调谐器。父任务和两个子任务均为 Succeeded。标准 Job 更新了集群 Agent Deployment 和独立节点 Agent DaemonSet；托管 Job 通过 Cluster 声明更新节点 Agent，保留集群 Agent 原有参数。应用容器使用健康检查夹具，Kubernetes 滚动、代理认证和调谐器行为均为实际执行。该用例未使用生产凭据，也未测试 Agent 到 Server 的隧道。 故意失败的节点 Agent 镜像滚动超时后，实际调谐器将同一 UID 的 DaemonSet 恢复到原镜像。模拟发布占用成员租约时，托管子任务报告了持有者，零失败阈值阻止了排队的标准子任务，镜像声明未变更。

标准 Job 显式选择上游组件清单，预检、升级及回滚都覆盖两个标准数据面工作负载。常规 Astribot 安装仍走托管声明。标准批次校验和安装器共用严格 DeployConfig 解析器，接受文档规定的连字符 YAML 字段。

### Gateway 业务归属和共享接入点

- `astribot/provisioning/workflow/` 负责 SSH 节点入网与集群创建，包括请求校验、阶段状态和恢复、机器安装、隧道建立及清理。它使用经校验的 Station 引导客户端和类型化管理面客户端。保存的阶段无法识别时，重试返回 HTTP 409。入网记录缺少完成时间时，列表会保留该记录。
- `astribot/storageconfig/` 负责存储声明校验、渲染、Secret 持久化、端点协调，以及为新集群接入共享 S3 存储。Gateway 保留现有路由、HTTP 错误映射和对象浏览客户端缓存。集群创建因部分写入而重试时会补齐缺失的端点声明。Astribot 复用上游 `pkg/gateway/storage` 的提供商目录和 S3 客户端。
- Gateway 接入共用的 JWT 和管理员鉴权，提供操作人身份和 Station 命名空间解析，并注入现有证书服务、Kubernetes 代理、镜像仓库凭据、Addon 发布、任务密钥分发及日志访问。这些接入点复用 Gateway 的已认证客户端。CRD 和生成客户端仍归 API 层维护。

集群创建在 Agent 注册前预留管理面 Cluster 并记录其 UID。存在同名 Cluster，但 UID 与原记录不符，或者历史记录缺少绑定 UID 时，清理在执行前返回 HTTP 409。如果原 Cluster 已删除，仍可清理遗留的创建记录。清理只移除能核对原 Secret UID 的凭据，以及能核对创建操作和 SiteNetwork UID 的原声明。场站入网许可和节点登记须各自核实后处理。宿主机回退校验创建操作标记，已安装 Kubernetes 时还会校验原 `kube-system` 命名空间 UID。无法验证宿主机原身份时，先由运维核实并单独处理，再使用 `machine_gone`。

SSH 入网通过安装机上的原安装进程取得原生 Kubernetes Node UID。清理已进入安装阶段的中断记录时，提交绑定该 UID 的 Remove 操作。HTTP 200 表示已提交，具体结果由 Station 的操作状态报告。历史记录或中断记录缺少原生 UID 时返回 HTTP 409，并保留记录供排查。当前同名节点的 UID 不会用于补写旧记录。清理已成功的入网只移除历史记录。先更新 Station Controller 并发布新的 Station 安装包，再启用更新后的 Gateway，使 SSH 移除的各阶段都执行新的身份校验。

### Server 隧道验收

独立验收使用 kcp v0.30.0 root workspace、一个 Kind 成员和本地构建的真实 Server、Cluster Agent、Node Agent。两个 Agent 使用 Server 签发的客户端证书连接，并续约各自的管理面心跳 Lease。通过 `/api/proxy/:agent/api/kubernetes/api/v1/nodes` 和 `/api/nodeproxy/:agent/:node/api/kubernetes/api/v1/nodes/:node` 发起认证请求，均读到了实际成员 API。请求未连接的节点返回 HTTP 502，节点专用路由未降级到集群隧道。Server 拒绝了无客户端证书的请求。重启测试 Server 后，两个 Agent 均自动重连，原请求再次成功。本次覆盖 Agent 反向隧道。机器 SSH 入网、Station WireGuard 链路和生产发布有各自的验收流程。
