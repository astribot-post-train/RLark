# API 参考

本页仅列出 Gateway 在 [`pkg/gateway/router.go`](https://github.com/RLinf/RLark/tree/main/apps/rlark/pkg/gateway/router.go) 中实际注册的 HTTP 路由。可运行请求参见 [API 调用样例](examples.md)，机器可读子集参见 [OpenAPI 规范](../../api/swagger.yaml)。

除 `POST /api/v1/auth/login` 和 `/metrics` 外，Gateway 路由均要求在 `Authorization: Bearer <token>` 请求头中携带登录返回的 JWT。token 默认 8 小时过期，可通过 `--jwt-token-ttl` 配置。缺失或无效 token 返回 `401`；已认证 `user` 调用仅管理员接口返回 `403`。

仅管理员操作包括 Node 和 Domain 变更，以及全部证书、镜像仓库、Addon 接口、系统配置更新和 StorageClass provider/变更接口。系统配置读取及全部 SSH 密钥操作允许任一已认证角色访问。

授权同时按角色与租户作用域执行。Workflow、Job、Task、Pod 路由被限定在调用方所属的租户内：作用域之外的 `?tenant=` 过滤或创建请求体返回 `403`，归属其他租户的具名对象返回 `404`，以免确认该名称存在。`admin` 调用方不受限制。其余资源（包括 SSH 密钥）仍只按角色授权，尚未按具体用户隔离。完整模型见[租户作用域强制](../developer-guide/api-conventions.md#51)。

下表用 `{name}` 表示路径参数；Router 源码中的 Gin 等价写法为 `:name`。Namespaced CRD 路由需要 `namespace` query 参数。

## CRD 资源

| 资源 | 作用域 | 路由 |
|------|--------|------|
| `nodes` | Namespaced | `GET, POST /api/v1/rlinf.io/v1alpha1/nodes`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/nodes/{name}` |
| `workflows` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/workflows`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/workflows/{name}` |
| `jobs` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/jobs`；`GET /api/v1/rlinf.io/v1alpha1/jobs/tags`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/jobs/{name}`；`GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/logs`；`GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/logs/label-values`；`GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/metrics` |
| `tasks` | Namespaced | `GET, POST /api/v1/rlinf.io/v1alpha1/tasks`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/tasks/{name}`；`GET /api/v1/rlinf.io/v1alpha1/tasks/{name}/exposed-ports` 读取实际分配 NodePort；`/api/v1/rlinf.io/v1alpha1/tasks/{name}/tensorboard/{path}` 接受所有方法 |
| `pods` | Namespaced | `GET /api/v1/rlinf.io/v1alpha1/pods`；`GET, PATCH /api/v1/rlinf.io/v1alpha1/pods/{name}`；`GET /api/v1/rlinf.io/v1alpha1/pods/{name}/events`；`GET /api/v1/rlinf.io/v1alpha1/pods/{name}/terminal` |
| `domains` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/domains`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/domains/{name}` |

创建 Job 时，Gateway 会将请求中的 `metadata.name` 保存为展示名，并在响应的 `metadata.name` 中返回系统生成的 `jo-<16 位十六进制字符>` 资源 ID。后续 Job API 请求应使用该返回 ID。

Gateway Router 未暴露 CRD status 子资源路由；状态随普通资源响应返回。

Job 日志接口已实现：`logs` 支持 `from`、`to`、`task`、`pod`、`query`、`cursor`、`order`。`from`、`to` 使用 RFC 3339 时间；`order=asc` 表示后端结果升序，其他值均为降序。提供 `from` 且已配置日志后端时，成功响应包含 `source: "backend"`、`entries`、`hasMore`、`nextCursor`；其他情况（包括后端查询失败）回退到 Pod 日志，响应包含 `source: "pod"` 和 `pods` 数组，数组项含 `taskName`、`podName`、`phase`、`node`、`logs`。

`logs/label-values` 支持 `label`（默认 `pod`）、`from`、`to`、`task`、`pod`，返回 `{ "values": [...] }`；未配置日志后端时数组为空。Job `metrics` 已注册，但当前仅返回 HTTP `501 Not Implemented`。

## 集群与证书

集群列表和详情响应的 `id` 是稳定的 Cluster 资源名；`name` 优先返回去除首尾空白后的 `spec.displayName`，为空时回退到资源名；`region` 优先返回去除首尾空白后的 `spec.region`，为空时保留从节点推导元数据的行为。显示元数据支持中文等 Unicode 字符。通过管理面的 Kubernetes API 配置 Cluster；URL、分发和调度引用使用 `id`，不要使用 `name`。

| 方法 | 路径 |
|------|------|
| `GET` | `/api/v1/clusters` |
| `GET` | `/api/v1/clusters/{cluster_id}` |
| `GET` | `/api/v1/certificates/agent` |
| `GET` | `/api/v1/certificates/agent/{cluster_id}` |
| `POST` | `/api/v1/certificates/agent` |
| `POST` | `/api/v1/certificates/revoke` |

Agent 证书签发仅在管理面 Secret 保存后返回凭证；存储失败会报错，归属冲突或不明确的 Secret 不会被接管。响应中断时证书仍可能已经保存，重试签发可能产生不同凭证。详见[证书管理](../deployment.md#92-签发-agent-证书)。

证书撤销需要管理员权限及配置了持久撤销存储的兼容 Server。POST JSON 包含 `cert_type`（`x509` 或 `ssh`）、规范十进制 `serial_number`、`subject_key_id`（X.509 为小写十六进制，SSH 为 key ID）和可选 `reason`。返回 200 和 `{"persisted":true}` 表示 Server 已持久保存撤销记录；重复提交同一身份是安全的。502 可能表示结果未知，包括旧 Server 未返回持久化回执的情况；恢复兼容 Server 后可用相同身份重试。撤销影响新认证，不会终止已有会话；Gateway 不会删除或轮换凭证。该源码变更需要同时升级 Gateway 和 Server，目前尚未在站点部署。

## Station 身份（预览）

`GET /api/v1/node-networks/{name}?cluster_id=<cluster-id>` 返回经验证的节点 WireGuard 接口、CIDR 地址、`state`（`current`、`stale` 或 `unreported`）和可选的服务端接收时间。接口采用与节点列表相同的用户认证和集群可见性规则。命名空间归属冲突返回 `409`；未配置、成员身份验证失败或 Station API 不可用返回 `503`。响应不包含 Peer 或密钥。启用条件见 Station 网关 README。

| 方法 | 路径 |
|------|------|
| `GET` | `/api/v1/station/stations?cluster_id={cluster_id}&limit={limit}&continue={token}` |
| `GET` | `/api/v1/station/stations/{name}?cluster_id={cluster_id}` |
| `POST` | `/api/v1/station/rebinds` |
| `GET` | `/api/v1/station/rebinds/{name}?cluster_id={cluster_id}` |
| `GET` | `/api/v1/station/observations?cluster_id={cluster_id}&limit={limit}&continue={token}` |
| `POST` | `/api/v1/station/node-operations` |
| `GET` | `/api/v1/station/node-operations?cluster_id={cluster_id}&limit={limit}&continue={token}` |
| `GET` | `/api/v1/station/node-operations/{name}?cluster_id={cluster_id}` |

两个 `GET` 接口均需要管理员权限。列表从管理面 Kubernetes API 读取 Station 自定义资源；`cluster_id` 是已注册 Cluster 对象名，Gateway 会将其解析为权威的 `spec.agentNamespace`。不要把 namespace 当作 `cluster_id` 传入。分页 `limit` 为 1 到 500（默认 200），`continue` 使用接口返回的不透明 token。Station 名称具有 namespace 作用域，因此详情接口必须提供 `cluster_id`。Cluster 到 namespace 映射冲突返回 `409`；管理面 API 不可用返回 `502`。这两个 GET 接口为只读预览。

`StationObservation.status.wireguard.publicKey` 是可选字段，报告观测到的本地接口公钥，供资产清单和配置流程使用；它不是认证凭证，不包含私钥、Peer 公钥、端点或 AllowedIPs。旧版 Agent 可以省略此字段。

`GET /api/v1/station/observations` 需要 `cluster_id`，返回该 Cluster 映射 namespace 中最近一次经服务端验证的快照，其中的 Node UID 可用于选择重绑目标；controller 执行前仍会重新读取成员 Node。`POST /api/v1/station/rebinds` 是仅管理员可调用的预览操作接口。请求提交 `cluster_id`、稳定的 `request_id`、`station_name`、页面读取到的 `station_uid` 与 `expected_binding_version`、`target_node_name`、`target_node_uid` 和操作者填写的 `reason`；`target_system_uuid` 是可选辅助证据。请求必须为严格 JSON，不超过 16 KiB，且不能包含未知字段。Gateway 按已注册 Cluster 解析 namespace，并拒绝 Station UID 过期、Station 已退役或 binding 版本变化的请求。接口创建名称确定的 `StationRebind` operation 并返回 `202`；相同 ID 和载荷重试返回同一 operation，复用 ID 但载荷不同返回 `409`。`GET /api/v1/station/rebinds/{name}` 返回当前异步状态。controller 会再次从成员集群读取目标 Node、核对 UID，并拒绝已绑定给其他 Station 的 UID 后才修改绑定。`202` 不代表重绑已完成。控制台支持对 `Conflict` 或 `Offline` Station 提交并跟踪 operation，但必须先有显示不同 Node UID 的新鲜观测；实际完成仍需安装 Station CRD/controller。

Station 发现和 Rebind reconcile 在 controller-manager leader election 下共享串行协调器；race 测试验证二者同时处理同一 Node UID 时最终只有一个 owner。leader 故障切换与真实 CRD operation 尚未测试，因此此预览接口还不能作为生产身份唯一性保证。

节点生命周期路由仅管理员可用，当前为实验能力。`POST /api/v1/station/node-operations` 接受严格 JSON，包含 `cluster_id`、稳定的 `request_id`、`action`、`node_name`，以及已人工确认 SHA256 主机指纹的 SSH 连接信息。SSH 密码和私钥必须且只能提供一种；sudo 密码可选。凭证保存在独立 `station-operation-credentials` namespace，以 Kubernetes operation UID 绑定，完成后清理；响应不返回凭证值或引用。只有 ID 和完整载荷（包括凭证）相同才按重放返回原操作；内容变化返回 `409`。列表接口的 `limit` 为 1 到 500（默认 100），`continue` 为不透明 token。`Join` 的 `containerd_runtime` 可选 `containerd`（默认 socket `/run/containerd/containerd.sock`）或 `fleet`（`/run/containerd-fleet/containerd.sock`）。成员集群须先安装匹配的 node-agent DaemonSet profile；controller 会核验所选 socket，并在 Kubernetes Node UID 验证后写入 runtime 标签。Join 默认拒绝已有 kubelet 配置。显式设置 `reset_existing_kubeadm: true` 后，controller 才会先清理旧 kubeadm 状态；前提是主机名匹配、目标集群内没有同名 Node 或相同 systemUUID、主机不是控制面且 sudo 成功。此检查不查询其他集群，也不会接管已有 Node。Join 前置条件要求 `net.ipv4.ip_forward=1`。如果 Station 仍绑定旧 Node UID，kubeadm 可能先完成而 RLark readiness 保持阻塞；新鲜观测报告新 UID 后，提交显式 Station Rebind，Join 调谐器会继续推进。Join 使用专用操作成员 kubeconfig、kubeadm bootstrap discovery 和固定 CA 的 SSH 验证，直到 Kubernetes Node 和 RLark Node 均就绪才完成。失败的一次性 kubeadm 尝试不会自动重放；恢复前须检查主机和操作拥有的 marker。`Remove` 会遵守 PDB 排空，扫描所有登记成员集群，并从可读配置中仅删除按 Node UID 归属或由新鲜身份观测匹配的 WireGuard Peer；关闭网络 DaemonSet 并等其 Pod 停止后再 reset 和删除 Node。所有集群凭证和 ConfigMap 均须可访问；缺凭证、API 故障、更新冲突或残留引用都会阻止剔除。旧配置归属无法确认时会在排空前阻止。`EnableWireGuard` 因尚未实现 Peer 分配而拒绝。`202` 只代表已记录，不代表完成。操作控制器仍属实验能力，不可视为生产就绪。

## 认证与 SSH 密钥

| 方法 | 路径 |
|------|------|
| `POST` | `/api/v1/auth/login` |
| `GET` | `/api/v1/ssh-user-keys` |
| `POST` | `/api/v1/ssh-user-keys` |
| `DELETE` | `/api/v1/ssh-user-keys/{index}?user={user}` |

## API 参考元数据

| 方法 | 路径 |
|------|------|
| `GET` | `/api/v1/api-reference` |

该接口要求认证，返回本地化分类名称、接口方法与路径、描述及响应示例。Web UI 的“接口参考”页面以此接口为数据源，不再单独维护接口列表。

## Gateway 可观测性

| 方法 | 路径 |
|------|------|
| `GET` | `/metrics` |

`GET /metrics` 以 Prometheus 文本格式暴露 Gateway 指标。

## 镜像、镜像仓库与系统配置

| 方法 | 路径 |
|------|------|
| `GET` | `/api/v1/images` |
| `GET, POST` | `/api/v1/image-registries` |
| `GET, PUT, DELETE` | `/api/v1/image-registries/{id}` |
| `GET` | `/api/v1/image-registries/{id}/password` |
| `GET` | `/api/v1/image-registries/{id}/repositories` |
| `GET` | `/api/v1/image-registries/{id}/tags?repository={name}` |
| `GET` | `/api/v1/image-registries/{id}/tag-details?repository={name}` |
| `GET` | `/api/v1/image-registries/{id}/manifest?repository={name}&tag={tag}` |
| `GET, PUT` | `/api/v1/system-config` |

系统配置分为 `ssh` 和 `log` 两类。`ssh.jumpHost` 必须是不含协议、用户、路径和端口的主机名或 IP；设置 `ssh.jumpPort` 时，其值必须为 1 到 65535 之间的整数。将 `log.backend` 设置为 `none` 可关闭历史日志查询，设置为 `sls` 时必须提供完整的 SLS 连接参数。响应中的 SLS 敏感凭据显示为 `****`，将该占位符原样提交表示保留已存储值。`PUT` 成功后返回最终生效且已掩码的配置。

可选的 `deployment` 分类沿用 `rlarkadm` `DeployConfig` 中与数据面 Agent 相关的子集，用于控制签发数据面 Agent 集群后展示的部署 YAML 默认值。仅接受 `controlPlaneAddress`、`sshAddress`、`insecureSkipTlsVerify`，以及 Kubernetes 下的 `kubeconfig`、`agentImage`、`image`、`imagePullPolicy`、`imagePullSecrets` 和 `containerdSocket`。控制面组件、数据库、证书、Docker 和 Raw 部署字段会被拒绝。`controlPlaneAddress` 为空时使用签发接口返回的 Server 地址；镜像拉取策略可为 `Always`、`IfNotPresent` 或 `Never`。

镜像仓库凭证使用不可变的 `ir-<16 位小写十六进制字符>` ID；展示 `name` 可以重复或修改。创建请求必须包含 `name`、`registry`、`username`、`password` 和 `clusterSelection`：

列表和详情不返回密码。管理员可显式调用 `GET /api/v1/image-registries/{id}/password`，返回 `{"password":"..."}`，响应包含 `Cache-Control: no-store`。已认证的请求会产生包含操作者、凭据 ID 和状态的结构化审计日志，不含密码。

```json
{"name":"生产 Harbor","registry":"harbor.example.com","username":"robot","password":"secret","clusterSelection":{"mode":"Selected","clusters":["cluster-a"]}}
```

`clusterSelection.mode` 可为 `None`（仅保存）、`Selected`（一个或多个逻辑集群名）或 `All`（当前及未来集群）。`None` 和 `All` 模式下 `clusters` 必须为空。普通凭据响应不返回密码，但会返回 `id`。`POST` 返回 `201 Created`。执行 `PUT` 时，省略 `password` 表示保留原密码，显式空密码非法。`DELETE` 返回 `202 Accepted`，因为 Replication 和 Delivery 会异步删除已分发的 Secret。

四个只读浏览接口仅限管理员。Gateway 需配置 `--registry-browse-allowed-hosts=registry.example.com:5443`（多个精确 `host[:port]` 或 IP CIDR 以逗号分隔）；默认空列表关闭浏览。CIDR 允许该网段内的 IP 字面地址使用任意端口，不对域名解析后匹配；应将网段限制在可信网络。只访问显式允许的 HTTPS 地址，不跟随重定向或使用代理，保存的凭证不返回给浏览器。Gateway 必须能访问仓库，并通过系统 CA 信任其证书；私有 CA 应在 Gateway 启动前加入其运行环境，不要关闭证书校验。仓库必须支持目录、分页标签查询、返回 `sha256` 摘要的 manifest HEAD 和 Basic 认证；暂不支持 token 认证或不提供目录接口的仓库。目录和标签每页最多 50 项，使用返回的 `next` 作为下一次请求的 `last`。仓库失败返回 502，不视为空结果。`tag-details` 读取同一页标签，并对每个标签 GET manifest 和镜像 config blob，按仓库顺序返回 `{items: [{tag, digest, created, size, platforms, error}], next}`。`created` 是镜像 config 中的构建时间（RFC 3339），不是推送时间；`size` 是 config 与压缩层字节数之和；`platforms` 列出 `os/architecture[/variant]`。对于 OCI index 或 Docker manifest list，会跳过 attestation 条目并列出全部平台，`created` 和 `size` 取 `linux/amd64`（没有时取第一个平台）。manifest 和 config 内容上限 4 MiB，且必须与其 `sha256` 摘要一致。无法读取元数据的标签（例如不支持的媒体类型，或 blob 重定向到外部存储）返回 `error: "unavailable"` 且不带元数据；只有标签列表本身失败时整个请求才失败。Gateway 每次请求都会重新解析标签，最多同时读取四个标签，并在内存中按 manifest 摘要缓存元数据（最多 4096 条，重启后清空）。已有 `GET /api/v1/images` 是 Job 镜像使用历史，不是仓库库存。查询期间标签和摘要可能变化，发布时应固定经过验证的摘要。

## 存储

| 方法 | 路径 |
|------|------|
| `GET, POST` | `/api/v1/storage/storageclass` |
| `PUT, DELETE` | `/api/v1/storage/storageclass/{name}` |
| `GET` | `/api/v1/storage/storageclass/provider` |
| `GET` | `/api/v1/storage/storageclass/{name}/list` |
| `POST` | `/api/v1/storage/storageclass/{name}/upload` |
| `GET, DELETE` | `/api/v1/storage/storageclass/{name}/object/{key}` |

StorageClass 接口管理控制面声明。Replication 和 Delivery 控制器会将生成的 rclone CSI 资源异步分发到已配置的目标集群；列表返回的是已管理声明，而非实时查询 Agent 的结果。文件路由会直接访问声明的存储桶。

## Addon

| 方法 | 路径 |
|------|------|
| `GET` | `/api/v1/addons` |
| `GET` | `/api/v1/addons/{name}` |
| `GET` | `/api/v1/installed-addons` |
| `GET, POST` | `/api/v1/clusters/{cluster_id}/addons` |
| `GET, PUT, DELETE` | `/api/v1/clusters/{cluster_id}/addons/{name}` |
