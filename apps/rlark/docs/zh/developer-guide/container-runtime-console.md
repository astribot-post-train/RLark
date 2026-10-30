# 容器运行时管理控制台

容器运行时扩展提供独立的管理员控制台，作为原生 RLark 控制台的补充。
代码和构建入口位于 `extensions/container-runtime/`，不会向原生
Cluster、Node、Job、Task API 增加容器专属字段。

## 本次范围

本预览版面向显式接入的 Raw 容器集群，提供：

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

浏览器通过同源 Gateway 复用 RLark 的正常登录会话。扩展 API 只接受受信任
Gateway 签发的短期 RLark HS256 JWT，并强制校验 issuer、subject、管理员角色、
算法和过期时间，同时拒绝客户端伪造的身份请求头。扩展 API 不签发浏览器
token，不接受 kubeconfig，不暴露签名密钥，也不代理任意 Kubernetes 路径。API
进程使用权限受限的独立管理 ServiceAccount；Agent 使用独立的 mTLS 接口。Agent
证书不会进入浏览器。

独立 UI 和 API 从同源的 `/api/v1/container-runtime/` 提供服务。开发环境使用
TLS 或仅监听回环地址的反向代理。认证响应设置 `Cache-Control: no-store`，日志
不得记录 Authorization、kubeconfig、证书材料或任务负载。没有管理员 JWT 返回
401；已认证但不是管理员返回 403。

浏览器只接收类型化摘要，不接收 Secret、证书路径、私钥或管理 kubeconfig。
启用 `--enable-node-registration=true` 并安装附加登记 RBAC 后，管理员可以在选定集群
页面声明已有 Raw 节点。证书/安装包导出仍在管理员 CLI 完成，浏览器不会成为接入或
签名通道。

## UI 状态与失败处理

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
4. 启动扩展 controller、Agent API 和独立控制台。仅在隔离预览集群启用 Job
   reservation 与 task execution。
5. 打开控制台，确认节点心跳新鲜且 GPU 在授权白名单中，再把可用 Agent 版本
   发布为目标版本。

当前金山云验收机使用 `regular-training` 队列，20 CPU、236 GiB、1 张 GPU、无
公网 IP；替代镜像为 `rlinf-dashboard:1.1`。该镜像只是起点，不能证明源机器
后来安装的所有依赖都存在。单进程 Raw GPU 生命周期和单节点空闲滚动升级已经
通过验收；多节点 Ray/RLinf 和 Docker Engine 生命周期需单独完成一致性测试后
才能对外宣称支持。


## 构建和启动独立控制台

控制台只接受 Bearer 认证。登录表单把类型化凭据转交配置的 Gateway，返回的
管理员 token 只留在浏览器内存中，刷新或退出后消失，不转发 Gateway Cookie。
本增量采用独立入口/会话，尚未向原生 UI 增加导航。管理员可跨租户查看平台
资源；普通用户不能访问。

从扩展目录部署 `config/rbac/container-runtime-console.yaml`。示例 ServiceAccount
位于 `rlark-system`，更换 namespace 时需同步修改三处引用。该身份可读取资源、
创建原生 Cluster 和专用 namespace、创建/patch 附属 Cluster；不能读取 Secret、
派发 Task、修改观测 Status、删除资源或释放 allocation。签名密钥不放在该
kubeconfig 中。原生 Node patch 由 API 限定为 `spec.unschedulable`；Kubernetes
RBAC 无法按字段限制 patch 权限，因此控制台仍是受信任管理员服务。

HS256 密钥必须是配置 Gateway 使用的原始字节（至少 32 字节），以只读文件
挂载到受信任控制面服务。HS256 是对称算法，持有密钥也能签发 token，因此
控制台与 Gateway 具有相同的 token 信任级别。运维负责分发密钥并同步轮换/
重启两个服务；控制台不会通过 Kubernetes Secret 权限获取密钥。

```bash
cd extensions/container-runtime
make build
make build-ui
bin/container-runtime-console \
  --addr=127.0.0.1:9445 \
  --kubeconfig=/private/console.kubeconfig \
  --gateway-jwt-key-file=/private/gateway-jwt.key \
  --gateway-origin=http://127.0.0.1:19000 \
  --ui-directory=ui/dist \
  --enable-node-registration=true
```

启用该开关前，先与基础控制台角色一起应用
`extensions/container-runtime/config/rbac/container-runtime-console-registration.yaml`；
关闭登记能力时同时移除附加绑定。

打开 `http://127.0.0.1:9445/` 并用管理员登录。HTTP 只允许显式回环 IP；其他
地址必须提供 `--tls-cert` 和 `--tls-key`。远程 Gateway 也必须使用系统 CA
信任的 HTTPS，没有跳过 TLS 校验选项。节点 mTLS 仍连接独立
`container-runtime-api` 服务。UI 构建要求 Node.js 20.19+ 或 22.12+，验收使用
Node.js 24。

预览 UI 创建 Raw 集群。API/CLI 也能创建 Docker 资源记录，但不代表 Docker
Engine 注册/执行已支持。节点登记仅在显式启用的管理员入口提供；安装包导出、产物发布、drain 仍通过
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


## 有界 Raw 日志预览

启动 API 时增加 `--log-read-addr=127.0.0.1:9446` 和
`--gateway-jwt-key-file=/private/gateway-jwt.key`，并在 Agent 安装包中同时启用
`enableExecution: true`、`enableLogReads: true`，控制台即可查看保留的 Raw Task
日志。Agent 通过既有 mTLS 接口轮询，supervisor 只读取精确 Task 的保留日志。
`--log-read-addr` 只接受明确的回环地址；独立控制台用
`--log-api-origin=http://127.0.0.1:9446` 代理固定的
`/api/v1/container-runtime/logs` 路由。

Agent 轮询端点和日志监听器必须属于同一 API 进程，因为待处理的读取请求保存在
该进程内存中。即使两个实例共用管理 API，把日志读取指向另一个实例仍会超时。
此预览尚无跨实例请求路由。

读取必须携带当前 inventory 中的 allocation UID 和 Task UID。最多返回合并
stdout/stderr 的开头 64 KiB，并附带截断信息。请求最长等待 20 秒，每节点一个、
全局 32 个；API 重启后请求丢弃。禁止调度、暂停和就绪过期不会撤销精确诊断访问，
但凭据撤销、对象替换、allocation 释放和含糊所有权会拒绝。监听器不接受路径、
不把日志写入 Status、不转发 Cookie、不跟随重定向。工作负载输出按转义纯文本显示；
历史归档、follow/tail、stats 和 terminal/exec 不在此预览范围。

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
附加 Job/ContainerJob RBAC 和显式 `--enable-job-submission` 开关，默认关闭。
在基础 console role 外安装
`extensions/container-runtime/config/rbac/container-runtime-console-jobs.yaml`，
再给既有 console 启动命令增加该开关。附加权限仅授予原生 Job get/create/patch
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
`--enable-node-registration=true`，并安装附加权限
`extensions/container-runtime/config/rbac/container-runtime-console-registration.yaml`。
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
