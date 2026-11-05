# 容器运行时管理控制台

容器运行时预览功能已集成到 RLark 管理后台的 **集群 → 容器集群**
（`/admin/container-clusters`），复用登录、语言、主题和同源 Gateway。
页面代码位于 `apps/rlark-ui/src/admin/container-runtime/`；管理逻辑仍在独立
Go 模块 `extensions/container-runtime/core/` 中，通过小型 Gateway 适配器挂载。
不会向原生 Cluster、Node、Job、Task API 增加容器专属字段。

## 本次范围

本预览版面向显式接入的 Raw 和 Docker 容器集群，提供：

- 集群列表和详情；
- 节点的权威身份、私网地址、运行时、能力、授权 GPU UUID、心跳新鲜度、
  Ready、可调度和忙闲状态；
- 可用 Agent 版本及当前目标版本；
- 可恢复的集群创建、目标版本选择和节点 cordon/uncordon；
- 显式启用的单进程 Raw Job 提交、绑定 Job 列表及持续暂停。

Allocation、其中的 Job 引用和 Agent 升级作为独立维度展示。控制台不会在缺少观测时
伪造利用率，也不会把过期心跳当作 Ready 或可调度。Ray/RLinf 组提交、
terminal/exec、自动注册、Docker Engine 创建和多节点调度仍不属于本预览版。
有界 Raw 日志读取需要显式启用独立的回环日志监听器和节点轮询器。

## Gateway 拓扑与服务管理预览

容器节点记录区分一个逻辑集群 Gateway 和 Worker。此角色只描述 RLark 容器拓扑，不会创建 Kubernetes 控制平面。Gateway 也可以执行工作负载。Agent 心跳与原生 Node 在线状态分别观测；两者都不能证明 A 到 Worker 的 SSH 可达性或 Ray 数据面连通。

节点登记可以填写明确的 GPU UUID 白名单。列表为空时，使用经过认证的 Agent 最近一次上报的可见 GPU 作为该节点的授权资源；心跳不得来自未来且必须在 45 秒内，否则调度失败关闭。自动模式仅适用于显式登记的容器节点，并要求 Notebook 的设备可见范围就是预期的资源隔离边界。

节点表分开显示 Gateway/Worker 角色、原生在线状态、Agent 心跳和 SSH 可达性。
显式启用的 Notebook 启动接入预览接受地址与角色，固定可用版本，再通过预配置的
严格 SSH 主机信任安装 Agent、任务管理器和 A 网关。进程启动后还要等待经过认证的
新鲜 Agent 心跳，PID 存活或 SSH 可达不能代替就绪。响应不确定时保留同一个操作
ID 重试。当前仍需要管理员本地接入 profile、已发布安装文件和附加 RBAC；真实
A/B/C 安装及恢复验收尚未完成。

单独登记仍只创建元数据。Notebook 停机时请先在云控制台启动，不静默接管新实例
或新地址。接入和 Agent 包不包含 GPU 保活。移除包支持不会修改独立部署的既有
服务，运维清理前必须确认归属。

接入成功后，后续 SSH 探测只更新节点的可达性和原因，不改写该操作的成功结果，也不
启动任何服务。`NotebookReplaced` 和 `PrivateAddressChanged` 表示 SSH 有响应但观测
身份不匹配，并不授权接管；`InvalidProbe` 将可达性标为未知。后续匹配的观测可以恢复
探测状态，即使历史版本已删除也能继续探测。此行为需要更新接入控制器；接入成功是
历史结果，不能证明当前节点或 Agent 已就绪。

新接入操作显式固定已有 Gateway 的 ContainerNode UID，且其认证 Agent 报告所选版本
时，更新后的控制器可以复用该网关，不重新安装或重启。此路径要求当前 generation 的
新鲜 Agent 心跳，并通过 SSH 只读检查 Notebook 实例、私网地址、私有服务配置、
PID/启动时间回执和运行中 Agent 的二进制摘要；必须同时观测到 Agent、gateway 和
task-manager。Agent 观测过期时返回 `ExistingAgentNotReady`；无法验证服务集合时返回
`ExistingGatewayUnverified`。两者均等待恢复，不会退回到重新安装网关。该检查只证明
当时观测到服务，不代表数据面或工作负载就绪；使用前必须部署更新后的控制器。

接入安装包还会保存经 SSH 探测的 Notebook 实例指纹（PID namespace、PID 1
启动时间和 boot ID）及容器私网地址。本地服务管理器在启动或接管进程回执前都会
重新校验两者；指纹或地址改变时拒绝接管旧服务，必须执行显式替换操作并签发
新的节点身份和安装包。完成接入还要求 Ready 条件本身对应当前节点 generation，
同时满足新鲜心跳和已固定的 Agent 版本。

可信接入 profile 的 `endpoint` 和启动参数可以使用 `$GATEWAY_PRIVATE_ADDRESS`，
启动参数也支持 `$NODE_PRIVATE_ADDRESS`，`$INSTALL` 表示节点专属安装目录。
地址来自经过认证的 Notebook 探测，不把平台 SSH/DNAT 地址当成容器地址。
未知占位符会导致安装包生成失败；已有静态配置仍然支持。地址变化后须显式替换
节点，并在安装前确保 API TLS 证书覆盖新探测的网关地址。这些占位符不会自动
轮换证书，也不会改写 `gatewayFiles` 中任意配置文件的内容。

可信 profile 中可选的 `ssh.ControlDirectory` 只为安装接入启用 OpenSSH 连接复用。
必须使用该 profile 专属的规范绝对目录并设置 `0700` 权限；拒绝符号链接、公开目录
和超过 Unix 控制 socket 长度限制的路径。
连接按 OpenSSH `%C` 主机、用户、端口哈希区分，保留严格主机信任，在最后一条接入
命令结束 60 秒后退出。Worker 私钥仍留在 A。这只是安装传输，不承载推理隧道；
已有连接在间歇性 SSH 握手失败时仍可用，不能据此认定公网入口已恢复健康。

profile 可通过 `executablesByNode` 和 `workDirectoriesByNode` 按节点名覆盖默认
工作负载白名单。这些路径属于可信服务端配置，不接受页面传入。路径必须在对应
Notebook 内已存在且解析后仍为原路径；配置 Python 虚拟环境时须显式解析符号链接。
服务管理器先校验白名单，再启动服务，并只创建该节点专属的私有任务和升级状态目录。
缺失路径、公开状态目录和符号链接会阻止启动，不会静默忽略或改写。路径配置和 SSH
地址分别管理。

心跳已成功发布后发生的任务或升级协调错误会记录日志，并按正常心跳间隔重试，
不会进入连通性中断的退避，也不会让旧观测续命。本地观测或心跳发布失败仍使用
有上限的退避。Online 只表示连通性；任务执行和升级就绪可以独立失败。
该行为需要部署更新后的 Agent 二进制。

操作因 Notebook 停机或换实例一直等待时，先点击“取消本次接入（保留已接入节点）”，
再提交新的拓扑。管理员接口为
`POST /api/v1/container-runtime/clusters/{namespace}/{name}/enrollments/{operation}/cancel`，
请求体为 `{"uid":"<NodeEnrollment UID>","clusterUID":"<ContainerCluster UID>"}`。
取消是不可逆的 Spec 期望状态，不删除操作；控制器将其标为 Cancelled，停止后续
安装接入。已接入节点继续运行。取消前已发出的 SSH 命令仍可能完成，替换前应检查
节点回执。重复取消幂等，已终结的成功或失败结果保留。直接管理 API 可设置
`NodeEnrollment.spec.cancelRequested: true`，语义相同。部署控制器和 Gateway
前先应用更新的 CRD 与接入 RBAC。

剩余一键生命周期、网络自动化和混合 Fleet 验收见[交付清单](notebook-fleet-delivery.md)。

## 原生在线状态

扩展控制器将当前经过认证的 ContainerNode 观测投影为精确绑定 UID 的原生 heartbeat
Lease。原生 Node 和 Cluster 页面使用核心控制器的 Online/Offline 和
Online/Downgraded/Offline 聚合。Lease 有效期 45 秒，使用原始服务端心跳时间；停止
Hub 或适配器会停止续租，下一次有效观测恢复在线。在线与可调度分别判断：原生 Node
禁止调度、工作负载预留、运行时能力和版本就绪条件仍然有效。适配器只拥有自身 Lease
及受控 Raw 能力/凭据投影，拒绝未知 Lease 或冲突的原生身份，不修改 Node Spec 或
reservation。应用更新后的控制器 RBAC 并重启扩展控制器后，已有 UI 刷新即可。
详见[连通性投影契约和验收](container-gateway-plan.md)。

## 资源与所有权

扩展拥有 `containers.rlark.io/v1alpha1` 附属资源组。`ContainerCluster`
通过名称、namespace 和 UID 指向一个原生 Cluster；`ContainerNode` 指向精确
的原生 Node 和 Cluster 实例。服务端会验证这些引用、权威的
`Cluster.spec.agentNamespace` 以及独立租户，然后才返回或接受写入。

名称和标签只能用于检索，不能证明所有权。原生对象被替换后，必须由管理员
显式创建新的附属资源，服务端会将旧引用报告为冲突。

集群创建只有在使用相同规范化操作 UUID 和请求指纹时才幂等。重试可以恢复已
提交的 namespace 或附属对象，但不能接管所有权元数据不同的对象。选择目标
版本要求当前 Cluster resourceVersion、精确的 release UID、release 当前
`Available=True` 观测和相同租户。目标版本是持续的期望状态；清空或替换它
会影响后续滚动升级，不会取消已经处于 Applying 的 Agent 升级。

## 认证与部署

浏览器通过共享 API 层复用 RLark 正常登录会话。Gateway 先验证 JWT 和管理员
角色，再为扩展处理器建立类型化的进程内管理员身份；客户端请求头不能伪造
该身份。未认证返回 401，普通用户返回 403。管理员可跨租户查看平台容器资源，
普通用户不可访问。所有路由均显式注册在 `/api/v1/container-runtime` 下，认证
响应使用 `Cache-Control: no-store`。

Gateway 使用其配置的管理 API 凭据，并以非缓存客户端校验所有权和资源版本。
为该身份安装附加管理 RBAC；示例绑定 `rlark-system` 中的 `rlark-gateway`，
请按实际 Gateway 身份修改，勿绑定为 Agent API 身份。浏览器只接收类型化摘要，
不会收到 Secret、kubeconfig、Agent 证书或签名密钥。节点登记和 Job 提交各自
需要显式开关及附加角色。Node patch 在服务端限定为 `spec.unschedulable`；
Kubernetes RBAC 本身无法按字段限定 patch。

Agent mTLS 仍由独立 `container-runtime-api` 服务承载。可选诊断使用其回环
监听器，并再次校验同一 Gateway JWT；只有该私有监听器需要 Gateway 签名密钥
文件，普通管理不再向另一个控制台进程分发密钥。日志不得记录凭据或任务负载，
不提供任意 Kubernetes 路径代理。

## UI 状态与失败处理

页面使用 RLark 共用内容视口进行纵向滚动。集群选择区横跨内容区域，之后依次展示
所选集群概况、资源指标、版本目标、Raw Job、节点登记、节点和操作表格。窄屏下
集群选项与指标会自动换行；宽表格保留独立的横向滚动区域。

节点以摘要表格展示私网 IP、原生 Node 状态、容器 Agent 就绪状态、Agent 版本、可调度
状态和心跳距今时间。展开行后可查看身份、原生地址与节点信息、环境、网络范围、完整
心跳时间、容量、授权 GPU、能力及条件；详情默认收起。原生 Kubernetes Node 阶段与
容器 Agent 就绪状态分别显示：原生 Node 可以在线而容器 Agent 心跳已过期；无论原生
连接状态如何，过期的 Agent 都不能调度。

集群页面将期望状态和观测分开：

| 维度 | 示例 |
| --- | --- |
| 期望状态 | runtime、目标 Agent 版本、暂停/升级意图 |
| 身份 | 原生/附属 UID、租户、namespace、私网地址 |
| 观测 | 心跳新鲜度、Ready、可调度、忙闲、能力 |
| 容量 | CPU/内存/GPU 总量和授权 GPU UUID |
| 操作 | allocation 数量、升级阶段、最近已验证回执 |

每次刷新都可取消，并丢弃过期响应。API 出错时保留可识别的 HTTP 错误类别，
显示“数据不可用”，不能显示成零计数。过期节点仍然可见并被调度拦截，不会被
静默删除。写入使用乐观 resourceVersion；发生冲突时提示管理员刷新。

控制台升级期间，UI 兼容未提供 `jobSubmissionEnabled` 的旧版资源响应，并将
Job 提交能力视为关闭。显式提供的能力值仍必须是布尔值；格式错误的资源响应
仍然报错。

## 支持的部署顺序

1. 安装扩展 CRD 和受限 API/controller RBAC。
2. 使用 `container-runtime-admin` 创建独立原生 Cluster 和容器附属资源。
3. 注册现有 Raw 容器主机并导出受限 Agent 包。cgroup-v2 或 systemd 不可用时，
   主机使用受信任的 root 进程树后端；它不是任务隔离边界。
4. 启动扩展 controller、Agent API 和更新后的 RLark Gateway/UI。仅在隔离预览集群启用 Job
   reservation 与 task execution。
5. 打开控制台，确认节点心跳新鲜且 GPU 在授权白名单中，再把可用 Agent 版本
   发布为目标版本。

当前金山云验收机使用 `regular-training` 队列，20 CPU、236 GiB、1 张 GPU、无
公网 IP；替代镜像为 `rlinf-dashboard:1.1`。该镜像只是起点，不能证明源机器
后来安装的所有依赖都存在。单进程 Raw GPU 生命周期和单节点空闲滚动升级已经
通过验收；多节点 Ray/RLinf 和 Docker Engine 生命周期需单独完成一致性测试后
才能对外宣称支持。


## 在 RLark 内构建、启用与调试

原独立控制台二进制、登录和 UI 已退役；管理页面现在由 RLark 主 UI 提供。需要同时重新构建 Gateway 和主 UI；
仅重启旧 UI 镜像不会增加页面。扩展运行时和控制器保留独立构建入口。

```bash
make -C apps/rlark gateway
make -C extensions/container-runtime build
npm --prefix apps/rlark-ui ci
npm --prefix apps/rlark-ui run build
kubectl apply -f extensions/container-runtime/config/crd/bases/
kubectl apply -f extensions/container-runtime/config/rbac/container-runtime-management.yaml
```

在现有 Gateway 启动参数中增加 `--enable-container-runtime=true`，保留其管理
API、认证及其他部署配置。可选节点登记需安装同目录下的
`container-runtime-management-registration.yaml` 并增加
`--container-runtime-enable-node-registration=true`。Raw Job 提交需安装
`container-runtime-management-jobs.yaml` 并增加
`--container-runtime-enable-job-submission=true`。关闭能力时移除对应附加绑定。
实际执行任务仍需分别启用扩展 controller、API、Agent 的执行开关及 RBAC。

如需诊断，让 Agent API 回环监听器与 Gateway 处于同一网络命名空间（Kubernetes
部署时放在同一 Pod），并给 Gateway 配置
`--container-runtime-diagnostics-origin=http://127.0.0.1:9446`。Agent API 仍使用
`--log-read-addr=127.0.0.1:9446` 和 `--gateway-jwt-key-file=...`。
仅接受明确的回环 IP HTTP origin；未配置时诊断返回不可用，资源管理仍可工作。

按现有部署流程更新 Gateway 二进制/镜像及主 UI 静态产物/镜像。本地调试使用
`npm --prefix apps/rlark-ui run dev`，沿用 Vite 的 Gateway 代理配置。通过 RLark
管理员登录后访问 `/admin/container-clusters`；集群链接包含 namespace/name，
刷新及浏览器前进后退会恢复选择。扩展关闭时导航仍可见，API 返回 503，页面
显示错误而非空列表或零资源。缺少 CRD、API 权限不足同样显示失败。未启用
`--enable-container-runtime` 却设置依赖选项时，Gateway 启动直接报错。

共享 UI API 层处理 token 存储、过期和退出；扩展返回 401 会清除同一 RLark
会话，不再维护第二套登录或浏览器 token。

### 模块边界与跟随上游更新

- `core/` 包含附属 API 类型、登记/提交、类型化管理处理器、制品校验和共享
  合约；依赖原生 API 模块，不依赖原生应用或 Docker SDK。
- `extensions/container-runtime/` 包含 Agent、运行时、控制器和 mTLS 传输，
  依赖 `core/` 及原生执行接口；Gateway 只引用 `core/`，避免 Go 模块循环。
- `apps/rlark/pkg/gateway/container_runtime.go` 是集成适配器。原生入口只增加
  配置、初始化及显式路由；UI 只增加导航和页面挂载。共享所有权/租户元数据
  在 `api/metadata` 定义，原生导出保留兼容别名。
- 根 workspace 和 Gateway Docker 构建包含 `core/`；扩展 `make test`、
  `make vet` 和 CRD 生成显式进入对应模块根目录。本次提取不改变 CRD
  group/version 和线上协议。

### 主 UI 集成验证

仓库 `make test-go`、扩展 test/vet/build、Gateway 构建、主 UI 全部 150 项测试、
UI 生产构建和严格中英文文档构建均通过。使用 API 传输模拟的浏览器冒烟覆盖原有
管理界面、两个集群切换、刷新/前进/后退、英文/暗色主题、扩展禁用 503 和 401 后
共享会话退出，无 JavaScript 异常。这不证明已完成线上部署或 Docker GPU 验收。

core 模块 lint 和原生增量 lint 通过。原生全量 lint 有 30 项既有发现；运行时模块
有 30 项既有发现。前端 lint 以成功状态结束，保留 58 项既有警告。修改的 UI 文件
格式检查通过；扩大格式检查时，5 个无关的既有测试文件未通过。

预览 UI 可以创建 Raw 或 Docker 集群。API/CLI 和管理员控制台可以登记 Docker 主机，接入[观测或按需开启的单容器执行](docker-agent-observation.md)；
当前表单仍不提交 Docker Job。节点登记仅在显式启用的管理员入口提供；安装包导出、产物发布、drain 仍通过
CLI 或运维流程完成。allocation 行保留其 Job 引用；可选 Raw Job 面板单独列出经过
验证的绑定 Job、提交单进程和记录暂停意图，不提供历史归档。

页面每 15 秒刷新观测。心跳超过 45 秒、未来心跳、过期 generation、凭据不匹配、
原生节点被替换、cordon/drain、旧版预留、已有锁和目标版本不一致都会阻止页面
显示可调度。快照只是提示，allocation API 在派发时仍重新检查权威状态。页面
显示登记授权与上次观测容量，不是实时利用率，也不保证 GPU 空闲。目标变化
可能按现有调度策略拦截旧节点，不保证服务可用性。

创建请求与规范操作 UUID 保存在 sessionStorage（不含凭据）。结果不确定时
只能重试完全相同请求；不能通过清除记录、换 UUID 修复部分创建，需先检查现有
原生对象/namespace/附属对象的所有权。清空与设置目标都要求相同 Cluster UID
和 resourceVersion。版本选择检查精确 UID/当前可用状态，但读后引用仍可能变化，
rollout 和 Agent 控制器在执行副作用前仍须复核。错误只返回有界 `code`/`message`
和 `X-Request-ID`，不泄露上游凭据或配置详情。

## 2026-10-31 验证记录

Go 测试、vet、扩展构建和增量 lint 通过。UI 传输/过期读取测试、TypeScript
检查、生产构建与依赖审计通过。浏览器对真实 Gateway/KCP 使用受限 console
ServiceAccount 验证了管理员登录、真实 `ksyun-containers` 资源、普通用户拒绝、
专用临时集群创建（故意丢弃首次已提交响应）、相同 UUID 重试、发布目标版本、
中英文切换和退出。浏览器没有 JavaScript 异常。临时资源通过正常生命周期清理，
现有云端 Cluster 的目标版本未改变；不代表多节点训练或生产部署已验收。

## 节点调度管理

控制台为现有 Raw 节点提供管理员 cordon/uncordon。接口只修改原生
`Node.spec.unschedulable`，每次校验 companion UID、引用的原生 UID 和原生
resourceVersion。页面把原生调度意图与 Ready、忙闲、已有 allocation 分开展示。
Cordon 阻止后续启动授权，不停止运行中进程、不释放 allocation，也不请求 Agent 升级。
它不暂停已配置的 Agent rollout，升级仍需独立的空闲证明和锁；也不能撤销恰好
在该意图被观察到前并发取得的启动授权。
Uncordon 只有在所有已知锁和旧版预留消失后才清空意图，不授予 Ready，也不绕过
placement/升级校验。未知锁、正在删除的锁、预留读取失败都阻止 uncordon。

接口为 `PATCH /api/v1/container-runtime/clusters/{namespace}/{cluster}/nodes/{node}`，
请求必须包含 `uid`（companion Node UID）、`resourceVersion`（原生 Node 版本）和
显式布尔 `unschedulable`。冲突或响应不确定时先重新读取，不能对新对象盲目重试
切换。写入与集群操作使用同样的平台管理员认证。新增 console RBAC 仅授予原生
Node `patch`，不允许修改 Node status。

Drain 是后续独立操作：停止 Job、证明清理和退役 allocation 必须可恢复。该调度
意图接口不提供 drain 或强制解锁按钮。

## 开发机重启后的恢复

平台停止/启动可能替换已有容器的实例，即使开发机 ID、私网地址和 GPU UUID 不变。
需要比较当前 PID namespace、PID 1 启动证据和 boot identity 与注册的 `instanceID`。
不能复用旧凭据，也不能修改不可变绑定来接管新实例。确认旧实例已停止，且 Task、
allocation 和升级操作都已消失后，使用 UID/resourceVersion 前置条件正常退役精确的
旧原生/companion Node，再重新注册并导出新 package 到独立安装/状态目录。Cluster
继续与本地 Kubernetes 资源分开。自动注册和启动恢复尚未实现，临时反向隧道也需重建。

2026-10-31 发现测试开发机已停止后，显式重启，未改变队列、规格或公网配置。
PID 1 启动证据发生变化；Python/PyTorch/Ray 和 CUDA `42.0` 仍可用。旧节点通过
正常生命周期退役，随后创建新实例的精确注册和 package。这是人工运维恢复，
与 Agent 二进制滚动升级是不同流程，不代表已实现自动恢复。

随后将替换实例的 package 安装到独立私有目录，并恢复临时反向隧道。当前 Agent
报告 `preview-4b8258c`、Ready、可调度和一张授权 GPU。真实
`TestCloudRawLifecycle` 复测在 165.10 秒内通过成功、GPU 复用、exit code 7
失败及暂停清理四项；测试 Task、binding 和 allocation 均正常删除。这仍是单节点
Raw 验收，没有 Docker socket，也没有完成多节点 Ray/RLinf 验证。


## 有界 Raw 日志预览

启动 API 时增加 `--log-read-addr=127.0.0.1:9446` 和
`--gateway-jwt-key-file=/private/gateway-jwt.key`，并在 Agent 安装包中同时启用
`enableExecution: true`、`enableLogReads: true`，控制台即可查看保留的 Raw Task
日志。Agent 通过既有 mTLS 接口轮询，supervisor 只读取精确 Task 的保留日志。
`--log-read-addr` 只接受明确的回环地址；Gateway 用
`--container-runtime-diagnostics-origin=http://127.0.0.1:9446` 代理固定的
`/api/v1/container-runtime/logs` 路由。

Agent 轮询端点和日志监听器必须属于同一 API 进程，因为待处理的读取请求保存在
该进程内存中。即使两个实例共用管理 API，把日志读取指向另一个实例仍会超时。
此预览尚无跨实例请求路由。

读取必须携带当前 inventory 中的 allocation UID 和 Task UID。最多返回合并
stdout/stderr 的开头 64 KiB，并附带截断信息。请求最长等待 20 秒，每节点一个、
全局 32 个；API 重启后请求丢弃。禁止调度、暂停和就绪过期不会撤销精确诊断访问，
但凭据撤销、对象替换、allocation 释放和含糊所有权会拒绝。监听器不接受路径、
不把日志写入 Status、不转发 Cookie、不跟随重定向。工作负载输出按转义纯文本显示；
历史归档、follow/tail 和 terminal/exec 不在此预览范围；Docker 的有界资源快照见下文。

2026-10-31，私有云验收通过控制台代理在专用 `regular-training` H100 开发机上
实际验证了该路径。测试使用短期 Gateway token，拒绝未认证、非法 path 和错误
Task UID 请求，然后读取到 65,536 字节的 CUDA 标记及截断元数据。任务正常停止
并释放 allocation 后，旧 allocation 身份再次访问也被拒绝。验收运行器只使用
管理 kubeconfig；受限 controller 凭据按设计不能创建原生 Job。

## Raw Job 提交增量（预览实现）

新增须显式启用的管理员提交入口：
`POST /api/v1/container-runtime/clusters/{namespace}/{cluster}/jobs`。
严格请求包含规范 operation UUID、精确的附属 Cluster UID、预置环境、可执行
argv 和工作目录。服务端从当前绑定取得租户、原生 Cluster 和 namespace；客户端
不能选择另一租户、注入 Pod 模板、指定 GPU 可见性或请求 Docker/Ray/整组派发。
仅创建单个 Raw 进程 Job，整节点/授权 GPU 预留仍由现有控制器负责。提交可以排队，
不保证容量，也不证明节点已安装可执行文件或依赖；supervisor 继续执行路径白名单。

operation UUID 确定唯一原生 Job 和附属 binding 名称。先创建暂停的 Job，再创建
绑定精确 Job UID 的 ContainerJob，最后以原子 UID/resourceVersion 前置校验的
patch 激活，同时记录激活证明。响应丢失后使用相同输入重试；每次写入前比较请求
指纹、完整工作负载及所有权。重试不能恢复激活后被暂停的 Job，也不能重跑已完成
Job。binding 已存在但 Job 丢失或替换时禁止重建；binding 创建前暂停的 Job 没有
执行授权。部分失败保留对象，等待相同请求恢复，不能通过删建或新 UUID 修复不确定
状态。只要两种记录之一保留就维持幂等；管理员显式删除两者才结束该保证。不新增
后台提交 worker。

提供按集群绑定隔离的类型化 Job 列表，以及携带精确 Job UID/resourceVersion 的
持续暂停请求。暂停不等于立即完成清理：分别显示 Job phase 与条件新鲜度，等待
现有 allocation 清理流程。不提供强制解锁、重启、自动重试或删除动作。控制台需要
附加 Job/ContainerJob RBAC 和显式 `--container-runtime-enable-job-submission` 开关，默认关闭。
在基础管理 role 外安装
`extensions/container-runtime/config/rbac/container-runtime-management-jobs.yaml`，
再给既有 Gateway 启动命令增加该开关。附加权限仅授予原生 Job get/create/patch
与附属 Job get/list/create，不授予删除、Task 派发或 status 写入。Kubernetes RBAC
不能把 Job patch 限定为暂停字段，因此控制台仍是受信任管理员服务。详情响应提供
`jobSubmissionEnabled`；未启用或不支持的集群不显示提交入口，也不轮询缺失接口。

表单使用 JSON 字符串数组输入 argv 与环境变量，保留空格和空参数，不进行 shell
解析。可执行文件与工作目录必须是规范绝对路径，禁止覆盖 GPU 可见性。未确认
请求保留 operation ID 和精确 Cluster UID，禁止切换集群，只能原样重试。Job phase、
暂停意图和当前 generation 的观测分别显示；绑定无效或已终态的 Job 禁用暂停。

列表使用相同 `/jobs` 路由的 `GET`；暂停使用
`PATCH .../jobs/{job-name}/suspend`，包含 `uid` 与 `resourceVersion`。冲突返回
409 并要求重新读取；非法请求返回 400，管理面故障返回脱敏 503。接口不提供恢复
已经暂停的 Job。

POST 请求示例（身份、环境及路径需按当前 inventory 替换）：

```json
{
  "operationID": "d07faab0-5896-4b9a-af29-887ba579c69f",
  "clusterUID": "REPLACE_COMPANION_CLUSTER_UID",
  "environment": "REPLACE_REGISTERED_PROFILE",
  "process": {
    "argv": ["/usr/bin/python3", "train.py"],
    "env": [],
    "directory": "/work"
  }
}
```

argv 可能含敏感信息，浏览器提交意图只保留在内存。响应不确定时保留表单/UUID 并
原样重试；刷新页面后使用保留的 operation ID 和管理员检查恢复。日志只记录操作/
资源身份，不记录 argv/environment 或上游错误。

验收需覆盖未认证/非管理员、错误/替换/含糊的 Cluster 绑定、运行时不支持、非法
路径及 GPU 覆盖、部分创建与激活响应丢失、并发重试、已停止/完成 Job 重放、所有权/
Spec 冲突、列表隔离、过期暂停、UI 请求校验/取消，以及真实私网节点派发与清理。
通过后才记录云端提交验收。


### 2026-10-31 提交验收

专用 `regular-training` 开发机在平台停止后重新启动，1 GPU / 20 CPU / 236 GiB
及公网配置未变。重新核对当前容器证据，只有在确认 Task、allocation、upgrade 均
不存在后才退役停止的精确旧 Node，随后重新注册并导出安装包。PyTorch
2.11.0+cu126、Ray 2.56.1 和 CUDA 均保留，无需安装依赖。通过临时 SSH 连接复用与
反向转发恢复测试连通；这是运维恢复，不是自动重启注册。

真实 Gateway/KCP 与受限 console ServiceAccount 上的浏览器提交了 Raw 进程，
故意丢弃已提交 POST 的响应，原样重试内存中的请求并恢复同一 Job UID。页面观测
Running，以精确 UID/resourceVersion 暂停，等待 Stopped 和 allocation 删除，
再验证重复提交不会恢复已暂停任务。保留的 supervisor 日志包含
`RLARK_CONSOLE_GPU_OK`，该标记只在单 GPU CUDA 计算得到 `42.0` 后输出。中英文
切换通过，无 JavaScript 异常。专用验收 Job、binding、Task 均以精确前置条件正常
清理，节点恢复 Ready、空闲、可调度且无 allocation。这仅证明单进程 Raw 提交预览；
分布式 Ray/RLinf 和 Docker Engine Job 提交仍不在范围内。

扩展测试/vet/build、submission/console race 测试、11 项 UI 传输/模型测试、
UI 生产构建与严格双语文档构建通过。仓库 `make test-go` 通过；增量 lint 零问题，
全仓库 lint 仍被原生 RLark 中的 30 项既有发现阻塞。

## 管理员节点登记

本增量在选定集群的详情页提供显式启用的已有 Raw 容器登记入口，API/UI 及失败/恢复
测试已覆盖；云端接入和 Agent 安装包验收仍是独立的运维门槛。启用
`--container-runtime-enable-node-registration=true`，并安装附加权限
`extensions/container-runtime/config/rbac/container-runtime-management-registration.yaml`。
默认控制台没有登记路由和 Node 创建权限。附加角色仅增加原生 Node 与
ContainerNode 创建，不增加 Secret、签名、status、删除、派发或凭据导出权限。

`POST /api/v1/container-runtime/clusters/{namespace}/{cluster}/nodes` 要求管理员
JWT 和严格 JSON 请求体：

```json
{
  "operationID": "5b70ee0b-563c-4bc9-8117-58e80ba70f75",
  "clusterUID": "replace-with-current-containercluster-uid",
  "name": "cloud-gpu-2",
  "instanceID": "replace-with-verified-container-incarnation",
  "privateAddress": "10.0.3.86",
  "environment": "rlinf-pi05",
  "networkScope": "ksyun-private",
  "authorizedGPUUUIDs": ["GPU-replace-with-platform-authorized-uuid"]
}
```

`clusterUID` 是 companion UID 前置条件，不是原生 Cluster UID。服务端读取精确
原生绑定、namespace 与独立租户；请求自行携带租户/原生引用会被拒绝。登记仅支持
已有容器 Raw 模式。先核对真实实例证据和平台 GPU 授权，不能从可见性推断授权。
同网络范围内重复私网地址、重复 GPU UUID 均报冲突。服务再次校验 companion UID
及原生绑定。CLI 的可选 `containerClusterUID` 前置条件也可固定 companion 实例；
省略它保持原 CLI 请求和操作摘要兼容。

成功响应包含 `namespace`、`name`、companion `uid`、`nativeUID`、`clusterUID` 和
`operationID`。登记不写 Ready，不签发证书、安装包或启动进程，也不解除已有 cordon。
在可信制包机上使用这些精确节点身份执行管理员 `node-package`，其中 `credentialID`
为登记操作 UUID。CA 私钥保留在制包机，私下传输/安装包，收到新鲜授权心跳后
再确认可调度。

浏览器首次 POST 前在内存中保留完整请求。响应不确定、HTTP 409 或服务不可用时
保留原请求，并禁止切换集群及并发创建 Job/集群；重试沿用相同 UUID 与输入。
只有副作用前的 HTTP 400 校验失败允许重新编辑。刷新、退出或关闭页面会丢失
内存请求：应私下保留 UUID 与输入，检查部分创建资源后再决定下一操作。
响应中的节点、集群或操作身份不匹配视为结果不确定，不能当成接入成功。
旧 API 省略 `nodeRegistrationEnabled` 时禁用表单，显式非布尔值仍报错。

## 独立 Raw 模板调度

显式启用的预留控制器支持原生 Job 中最多 16 个名称唯一的独立 Raw 进程模板，
分别分配并聚合清理。控制台提交 API 和表单仍只创建一个进程模板。测试多个独立
成员时，请使用[交付计划](raw-container-runtime-plan.md)中的管理员管理 API 步骤
和示例。允许部分成员先行运行；这还不是 Ray/RLinf 分布式训练或整组准入调度器。

两个模板可以在一个合格节点上依次执行；已清理并释放的兄弟成员不阻止下一个成员运行。启动仍需自身精确且当前有效的 allocation，整组退役仍阻止新启动。

## Docker 诊断读取（预览）

Docker allocation 行现在可手动读取日志和资源快照，要求节点同时声明
`docker-diagnostics-v1`、`docker-container-execution-v1`，且尚未确认清理。
`/api/v1/container-runtime/stats` GET 路由与日志使用相同四个确切身份参数和
Gateway 管理员 JWT；固定回环代理复用 `--container-runtime-diagnostics-origin`。不向浏览器暴露新凭据、
任意上游路由或远程 Docker socket。

快照显示采样时间、CPU 累计纳秒、内存使用/上限、PID 数、块设备读/写字节。
uint64 保持十进制字符串，缺失数据显示不可用；不提供 CPU 百分比、GPU 利用率或
host network 流量归属。仅支持手动读取；切换/退出、观测到 allocation 替换/清理
或能力撤销都会取消在途请求并清除显示。读取临时、有界且在返回前重验；API 重启
后需手动重试。启用、所有权、限额和故障恢复见 [Docker Agent](docker-agent-observation.md)。
底层 Engine 传输通过模拟 daemon 测试，真实 Docker GPU 验收仍未完成。浏览器
Docker Job 提交仍是后续工作；Docker 登记和升级见[集群管理交付计划](docker-cluster-management-plan.md)。


## Raw Notebook 资源复用原生 Node

Raw 已有容器节点现已将经过身份校验的资源观测投影到原有 RLark
`Node.status.capacity`、`allocatable`、`used`、`nodeInfo` 和 `addresses`。
详情 DTO 包含原生 generation，使两处视图都能拒绝过期资源/调度条件。
原生节点页与容器集群详情复用同一个只读节点概况组件，Raw 不使用 Kubernetes
Pod 申请量作为回退值。Kubernetes 作业节点选择器排除显式 Raw 和 Docker 节点；
能看到资源不代表这些运行时可以执行 Kubernetes 工作负载。

- CPU 取进程有效亲和性与 cgroup v2 各级祖先配额的较小值。内存取各级祖先最小有限
  上限；无限或无法获取的内存上限不填报，不拿物理宿主内存代替。
- GPU 只统计管理员授权、并由 Agent 确认可见的 UUID 集合。额外可见的卡不带来调度权限。
- `Allocatable = Capacity`，表示容器资源上限，不任意扣除 Agent/系统开销。
  有效的零值与缺失信息严格区分。
- `Used` 是 RLark 工作负载预留量，**不是 CPU/GPU/内存实时利用率**。当前 Raw 运行时
  每个 Task 独占整个 Notebook，与请求几张卡无关。只要对应分配对象仍存在，所有已知
  CPU、内存和授权 GPU 都继续预留；任务终止、对象删除中、清理确认、心跳失联或重启
  都不会直接释放。只有分配对象删除后才释放。升级维护锁和 supervisor 发现的外部占用
  单独阻止调度，不计入 RLark 工作负载 Used。
- 控制面在收到有效正数 CPU 观测时，使用服务端时间写入
  `ContainerNode.status.resourceObservedAt`。Agent 可选上报 OS、架构和内核版本；
  旧 Agent 缺少这些字段仍兼容，但不会伪造系统信息。只有 GPU 的旧心跳不视为完整的新鲜资源观测。
- 独立资源控制器重新读取绑定与分配，每 15 秒修复，只负责投影资源/系统字段、
  `RawResourcesReady` / `RawSchedulingReady` 条件，以及
  `containers.rlark.io/native-resource-projection` / `resources-observed-at` 注解。
  投影所有权绑定 companion UID，核心继续负责连接 `Ready`/Phase 与 Node Spec。
  状态补丁使用乐观锁，保留并发写入的其他条件。资源投影不创建/释放调度锁，也不续心跳。
- 超过 45 秒的观测、未来时间、凭证撤销、过期 Spec 观测、所有权冲突或读取分配清单
  失败，都不能宣告资源空闲。上次容量与预留量可保留展示，同时标明不可用/过期；
  未知剩余量显示 `—`。已知停止调度、维护或 Agent 忙碌时，即使预留量为零，可下发资源
  仍为零。原生资源视图最终一致，真正下发仍以当前分配检查为准。

本预览不报告物理宿主容量、实时利用率、GPU 型号、存储配额、磁盘压力和硬件拓扑。
磁盘缺失表示未上报，不表示磁盘健康或空闲。Notebook 节点的这项资源展示不需要
Docker socket、systemd 或 `CAP_NET_ADMIN`。

### 资源功能部署与只读验收

1. 新 Agent API 启动前，先应用更新后的 `containers.rlark.io_containernodes.yaml` CRD，
   然后部署扩展控制器、Agent API 与原生 Gateway。
2. 部署使用乐观锁状态补丁的原生节点控制器，重新构建 RLark 主 UI。两处视图使用同一份
   原生资源快照，不增加独立仪表盘或资源数据库。
3. 只升级 Notebook Agent 采集进程以获得可选系统信息。验收期间保持 Node 停止调度，
   不重启承载活动任务的 supervisor。
4. 核对 companion/native Node 的准确 UID、服务端观测时间、授权 GPU、资源条件及
   `status.nodeInfo`；确认第二次观测更新时间，所有权与停止调度状态不变。

```bash
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n ksyun-container-agents \
  get containernodes.containers.rlark.io ksyun-a -o yaml
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n ksyun-container-agents \
  get nodes.rlinf.io ksyun-a -o yaml
```

没有新鲜资源投影的节点，只要心跳有效仍可显示已连接，但资源可用量保持未知。
升级资源展示不会启动工作负载或释放分配；任务执行验收另行进行。


### 2026-11-03 云端只读验收

通过真实管理 API、Gateway 和 RLark 主 UI，检查了现有 `ksyun-a` Notebook 的
原生节点页与展开后的容器集群详情。两处与原生 Node 快照一致：100 核 CPU、
1,270,236,577,792 字节内存（约 1183 GiB）、5 张授权 GPU、Linux/amd64、
内核 `5.15.0-88-generic`，以及内网地址 `10.1.3.100`。这是当前 Notebook 上报的
资源上限，不是早期单 GPU 测试机的规格。

Capacity 与 Allocatable 相等；RLark 工作负载 Used 为零，节点保持停止调度，因此
可下发资源为零。原生与 companion UID 均未改变，连接保持 Online，观测时间随心跳
持续刷新。仅重启了 Agent 采集进程，原有 supervisor 和 Raylet 的 PID/启动时间未变。
未提交任何工作负载，两个真实页面均无 JavaScript 异常。

本地 Gateway 的持久启动配置已包含 `--enable-container-runtime` 及可选注册/提交开关；
只在临时进程中添加这些开关会导致重启后控制台返回 503。诊断日志/资源快照代理仍需
单独配置，不在本次验收范围内。
