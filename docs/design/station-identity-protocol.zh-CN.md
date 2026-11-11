# Station 节点身份协议与验收设计（试验性原型，未部署）

## 范围与信任边界

本协议用于 station-api 接收节点观测和授权敏感配置；现有 WireGuard 网关以管理员维护的 ConfigMap 运行，并不使用本协议。集群 API 凭证仅定位一个管理员注册的成员集群，不能单独证明节点身份。节点 UID 是 Kubernetes 对象实例标识，不能作为凭证；节点删除重建时 UID 改变必须触发冲突，不能根据同名自动接管。每次绑定以成员集群的稳定 ID 与 Kubernetes Node UID 共同标识；显示名、systemUUID、标签和客户端地址都不能替代二者。

信任成员集群控制面、其管理员和站点节点 root；它们被攻陷时无法由该协议隔离。防止一般工作负载冒充 agent 必须同时限制谁可在 agent 命名空间创建 Pod、使用该 ServiceAccount、修改 DaemonSet 和绑定角色；仅验证 Pod 的 ownerReference 或标签不足以证明其由 DaemonSet 创建。集群准入策略、RBAC 和命名空间隔离应作为部署前置并进行否定测试。

## 请求验证顺序

1. 服务端通过受控的集群注册记录选择对应 Kubernetes API、TLS 信任根和 TokenReview 凭证；集群 ID 来自服务端注册，不能信任上报的 API 地址。连接不通、证书错误、注册不存在或撤销时均拒绝。禁止在日志记录 token、私钥和完整 Authorization 头。
2. 向选定的成员 API 提交 TokenReview，指定唯一 station-api audience；必须同时检查 `authenticated`、服务端返回的 audience、ServiceAccount 的 namespace/name/UID，以及认证结果中 Pod name/UID。缺少所需字段时拒绝，不能退回到自报身份。TokenReview 返回 error 或超时也不能当作授权成功。节点来源集群必须匹配注册记录，不能接受另一成员集群的同名 ServiceAccount。
3. 用受限只读凭证读取该 namespace/name 的 Pod，要求 UID 与 TokenReview 完全相同、Pod 未删除、ServiceAccount 一致、`spec.nodeName` 非空；读取对应 Node，要求未删除，并以实际 Node UID 作为主体。客户端声明的节点名或 UID 与服务器读取结果不同则拒绝。必须重新读取对象，不能在缓存过期时继续授权敏感配置。验证配置对象所属的成员集群、Station binding 版本和当前 Node UID，冲突或退役时拒绝。
4. 服务端生成具有加密随机性的一次性 challenge，绑定集群 ID、Pod UID、Node UID、Station UID、binding 版本、操作类型、请求摘要及短有效期。challenge 的创建、过期和消费必须保存在多副本共享的持久存储中；使用事务条件更新确保并发最多一次成功。敏感请求重新执行上述身份校验，并在下发前原子消费匹配 challenge；错误主体、不同摘要、过期、重放均拒绝。消费成功但响应丢失时只能以新 challenge 重新读取当前期望状态；不能把敏感状态变更设计成可重复执行的边沿命令。挑战存储不可用时拒绝敏感请求。
5. 普通观测写入以前述主体为来源，服务端生成接收时间并限定大小、速率和字段；客户端时钟只作观测值。使用对象 UID、resourceVersion 和写入方权限避免覆盖其他 Station 或旧实例。具体配置下发还须明确密钥材料的加密传输、生命周期及授权撤销机制，不能把 ConfigMap 中的公钥当成身份凭证。

TokenReview 是某一时刻的认证结果，无法替代后续读取和授权，也不能防止已经获得合法 token 的受信任节点在有效期内滥用；短期 token、一次性 challenge、最小权限和成员集群准入分别解决不同问题。缓存策略如被引入，必须明确撤销延迟和最大陈旧时间，敏感操作在故障时拒绝。

## 2026-11-11 纯校验库进展

`apps/rlark/pkg/stationidentity` 已新增纯校验层：它要求注册集群、ServiceAccount、Pod UID、Node UID、Station UID、TokenReview audience 和 binding generation 同时匹配；Pod/Node 删除、跨集群同名对象、错误 audience 和旧 UID 均拒绝。它还生成绑定请求摘要、主体和 generation 的短期 challenge，并拒绝主体、摘要、操作、generation 或过期时间不匹配。

纯校验层本身只验证输入证据；库中后续增加 Kubernetes 读取函数、服务层、HTTP handler、动态注册表和 Observation sink（见下文）。HTTP 原型要求 `SensitiveAuthorizer`，签发和消费 challenge 时均调用授权器；未配置授权器或授权依赖故障时 fail closed，明确拒绝只返回通用 403。接口已落实 fail-closed 边界，但生产授权策略适配器尚未实现。可执行 station-api 和 Deployment 示例现已存在，尚未部署；Go 测试通过不能据此声称 Station 身份协议已上线。

2026-11-11 增加 `ChallengeStore` 存储边界和并发安全的进程内测试实现：服务端以加密随机 ID 登记 challenge，16 个并发消费者中只有一次成功；未登记 ID、重放、不同请求摘要和到期时刻均拒绝，错误请求不会消耗 challenge。内存实现只用于单进程测试；生产必须使用共享持久存储和事务条件消费。当时尚无可执行 station-api，当前接入状态见本节末。

2026-11-11 进一步收紧纯证据校验：必须显式确认 TokenReview authenticated，匹配注册的 ServiceAccount UID、Pod 名称与 UID、Pod 使用的 ServiceAccount、Pod 所在节点名称、注册的 Node UID、Station UID 和 binding generation；无效绑定及空 audience 拒绝。race 测试覆盖这些失败路径。校验器仍依赖调用方提供可信且新鲜的服务端读取结果，本身不执行 TokenReview 或 Kubernetes 对象读取。

2026-11-11 新增可注入 Kubernetes 客户端的身份读取函数：使用服务端注册的 audience 发起 TokenReview，再读取绑定 Pod、ServiceAccount 和 Node 并交给纯校验器；多节点模式通过服务端 `BindingResolver` 按实际 Node 名称和 UID 解析 Station UID/generation，避免把集群注册错误固定到单一节点；TokenReview 失败、错误 audience、ServiceAccount/Pod UID 不匹配、节点重建及缺失身份均拒绝。client-go 假 API 的 race 测试通过。该函数要求调用方提供可信注册记录、验证 TLS 的集群客户端和新鲜 binding；尚未实现注册管理、station-api、共享 challenge 或生产 RBAC，也未替代现场端到端验收。

2026-11-11 新增可选真实 Kubernetes 集成测试 `TestLiveKubernetesIdentity`。在本地 cailiang 集群创建临时命名空间、ServiceAccount 和绑定 Pod 的短期 token，调用新读取函数验证成功、错误 audience 拒绝、删除 Pod 后 token 被拒绝；race 测试通过，临时命名空间已清理。运行需显式设置 `STATION_TEST_KUBECONFIG` 和 `STATION_TEST_NODE`，测试会创建并删除临时命名空间；默认跳过。此验证不覆盖真实注册管理、共享 challenge、授权策略或其他站点的该代码路径。

2026-11-11 跨站真实集成复测：在机器人集群（API 隧道 `10.254.240.3:6443`）和金山云集群（`10.254.240.2:6443`）分别创建临时 namespace、ServiceAccount、Pod 和 Pod-bound token，验证正确 token、错误 audience 以及删除 Pod 后 token 失效；两处 race 测试均通过，临时资源和机器人临时 kubeconfig 已清理。该测试仍使用开发机直接调用库，不是已部署 station-api，也未覆盖生产注册、RBAC 和 challenge 持久化。

2026-11-11 新增 `PostgresChallengeStore`，使用独立 `station_api.challenges` 表；challenge ID 使用 256-bit 随机数，消费通过单条条件 UPDATE 原子检查主体、generation、摘要、数据库时钟有效期和 `consumed_at IS NULL`。隔离 PostgreSQL 15 race 测试用两个独立 store 并发消费，恰好一个成功；错误摘要不消耗记录，过期 challenge 拒绝，测试容器已删除。schema SQL 位于 `apps/rlark/config/station-api-challenges.sql`，运行账号应为非 owner 并仅授予需要的 SELECT/INSERT/UPDATE；过期清理使用独立 janitor DELETE 权限，在线运行账号不获 DELETE。PostgreSQL 集成测试现覆盖过期批量清理及重复清理，测试容器已删除。该 store 已由可执行 station-api 构造，但未配置生产授权器的 challenge/sensitive 路由仍 fail closed；权限审计、清理调度、保留期限、备份恢复和生产部署仍未实现。

2026-11-11 在 `pkg/stationidentity` 新增 HTTP API 原型：`GET /healthz`、`GET /readyz`、需身份认证的 `POST /v1/observations`、`POST /v1/challenges` 和 `POST /v1/sensitive/{operation}`。调用方必须由服务端集群注册表解析；接口限制并严格解码 JSON，请求观测数据会关联新鲜验证证据；敏感操作执行前消费一次性 challenge。操作失败后 challenge 仍保持已消费。新增必需的 `SensitiveAuthorizer` 接口，在签发和消费时都授权；缺少授权器或依赖故障时 fail closed，拒绝响应不泄露内部错误。测试验证授权拒绝或暂时不可用时不先消费 challenge，授权恢复后同一 challenge 可成功一次。handler 仍是库级原型；另有独立可执行包装器提供 TLS 1.2+ HTTPS、依赖 readiness、有限超时和优雅关闭。默认 Deployment 未配置生产授权器和动作 adapter，因此敏感路由保持 fail closed。进程内 challenge store 仅用于测试；可执行服务配置 PostgreSQL store。race 测试覆盖授权拒绝/不可用、身份失败、格式错误/超限输入、challenge 重放、操作失败和 readiness。

2026-11-11 新增 `KubernetesObservationSink` 和 `KubernetesClusterNamespaceResolver`。Resolver 每次读取当前 RLark Cluster，只从唯一的 `spec.agentNamespace` 确定管理面作用域；未知 Cluster 或 namespace 重复归属均 fail closed。Sink 先创建空 `StationObservation`，再用 status 子资源写入服务端接收时间和已验证 Node UID；只修改 status，拒绝 cluster 或 Station UID annotation 不匹配的既有对象，清除最新快照中已缺失的可选字段，并对 Create 竞争和 resourceVersion 冲突做有界重试。race 测试覆盖这些行为。该 sink 现已接入 station-api，但尚未对真实 CRD 执行 status 写入集成测试。

2026-11-11 当前可执行状态：`cmd/station-api` 使用管理面 Cluster API 与专用 credential Secret 动态解析注册，每次身份验证前读取当前 Station binding；member kubeconfig 仅接受 HTTPS、内联 CA/凭据及无 exec/auth plugin 的形式，Secret 内容变化会刷新 client。Secret 必须位于 `station-member-credentials`，annotation `station.rlark.io/cluster-id` 与 Cluster 对象名完全一致，data keys 为 `kubeconfig`、`service-account-namespace`、`service-account-name`、`service-account-uid`、`audience`。Observation rate limiter 按已验证 cluster+Node UID 限制到 1 request/s、burst 5，内存桶最多 10000 个并在空闲 10 分钟后清除。API 示例固定单副本，因为 limiter 状态仍在进程内存；横向扩展前须实现共享 limiter。PostgreSQL DSN 必须使用 `sslmode=verify-full` 和容器内 `sslrootcert`，禁止降级到明文或不验证主机名。`make -C apps/rlark station-api` 构建通过；identity race tests、vet 通过。部署和 RBAC YAML 通过 kubectl client-side dry-run，未应用。`station-api.yaml` 当前仅创建 ClusterIP Service，跨站访问地址及 TLS SAN 仍未确定；管理身份与 member reader/agent 权限须分别配置。数据库 schema 要由迁移 owner 预先创建；运行账号不是 schema owner，在线 API 角色无 DELETE。Gateway 管理员 Rebind HTTP API 已实现幂等 operation 提交和查询，并通过 fake Kubernetes 与 JWT 权限测试，但尚未部署或对现场 CRD 验证；敏感授权策略、审计、生产 Secret 轮换与 live CRD 写入仍未完成。

2026-11-11 新增 `station-agent` 可执行程序和 DaemonSet 示例。Agent 每次上报都重新读取 projected Pod-bound ServiceAccount token，通过 HTTPS 发送 `cluster_id` 和从 `/proc/stat` 的 `btime` 解析出的内核启动时间。该时间仅是观测值，不是身份凭据；`btime` 缺失或格式无效时上报失败并进入有界指数退避，不发送不完整快照。Agent 提供仅绑定 loopback 的 liveness/readiness；readiness 只在最近一次上报成功且未超过两次 interval 加一个 request timeout 时成立。HTTP client 不使用环境代理并拒绝所有重定向，避免 bearer token 离开配置的 API origin。Pod、Node 与 Station 身份由服务端 TokenReview 和新鲜 API 读取解析，客户端不提交自报 Node 名。DaemonSet 使用 ConfigMap 保存非密 endpoint/Cluster hints，并要求 CA Secret；不挂载 host 配置文件或私钥。localhost HTTPS 集成测试覆盖 CA 验证、token 轮换、重定向拒绝、请求格式、启动时间解析及 readiness 时效，race 测试通过。当前 agent 尚不采集硬件 ID、WireGuard peer 状态或主动探测；endpoint 仍是待确定占位值，未形成已部署的跨站 agent/API 链路。

## 验证与剩余工作

2026-11-10 已在本地、机器人和金山云 Kubernetes 验证 Pod UID 绑定 token、错误 audience 和删除 Pod 后 TokenReview 拒绝；另在本地和金山云分别创建同名 namespace、ServiceAccount 和 Pod：两端 TokenReview 均接受本集群 token、拒绝对方 token，并验证成功响应的 audience 与 Pod UID。再次提交相同的有效 token，两端 TokenReview 均再次认证成功，证实 TokenReview 本身不提供一次性消费或重放防护。测试命名空间已删除。这仅覆盖协议的一部分。尚未实现 station-api、集群注册、服务端对象读取、绑定授权或 challenge 存储。上线前需要自动化覆盖：

- 正确身份成功；错误 audience、集群、ServiceAccount、Pod UID、Node UID、绑定版本、退役状态和删除对象均拒绝；同名重建不得接管。
- 可创建普通 Pod 的低权限主体不能使用 agent 身份；尝试伪造 ownerReference、标签、自报节点及跨集群 token 均拒绝。
- 并发消费同一 challenge 最多成功一次；不同摘要、超时、重启、多副本、请求响应丢失及持久存储故障均验证拒绝或可恢复语义。
- TokenReview 和 Pod/Node 读取的超时、权限不足、断网、TLS 失败及对象删除竞争均拒绝；审计记录不含凭证，并验证权限撤销与密钥轮换。

2026-11-11 发现并修复首次绑定闭环：此前 API 要求已有 Station binding 才接受 Observation，而 controller 又需要 Observation 才能创建首个 Station。现允许经过新鲜 TokenReview 及 Pod/ServiceAccount/Node 读取的未绑定 Node 仅访问普通心跳端点；初始 Observation 归属 Cluster ID 与 Node UID。Controller 创建 Station 后，后续上报先用 resourceVersion 条件 metadata patch 绑定精确 Station 对象 UID，再更新 status。Node 替换或已有 Observation UID 冲突仍 fail closed；challenge 和敏感端点明确拒绝未绑定身份。race 测试覆盖未绑定校验、bootstrap 创建、一次性 Station 绑定、替换冲突及敏感操作拒绝；另有 handler 级 fake Kubernetes 串联测试，依次执行首次上报、Station 创建、UID 绑定上报和 challenge 签发。该转换尚未对真实 CRD API 做集成测试。
