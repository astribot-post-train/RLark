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

Astribot 专用实现放在 `apps/rlark/astribot/`、`apps/rlark-ui/src/astribot/` 和 `astribot/`。已归拢 Addon 定义、Station API 客户端、型号目录、运行镜像、本体状态、相机和日志代理、手动扫描、Agent 就绪判定、节点服务代理、逐节点任务状态转换，以及存储挂载和编码规则。前端默认测试命令也会运行 `apps/rlark-ui/tests/astribot/` 下的测试。

CRD 字段与生成产物、共享角色、HTTP 鉴权和错误转换、组件接线及其集成测试随共享契约维护。已有安装通过同名同版本的 `csi-driver-rclone` 使用 Astribot 参数，因此该扩展仍在原 Addon 定义内。Gateway 的入网、集群配置、机器人操作、仓库与存储编排，依赖控制器内部状态的 Agent 和 provider 适配，以及剩余页面接入仍待迁移。审查时逐项核对这些例外。

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
