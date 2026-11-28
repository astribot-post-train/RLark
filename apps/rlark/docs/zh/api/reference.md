# API 参考

本页仅列出 Gateway 在 [`pkg/gateway/router.go`](https://github.com/RLinf/RLark/tree/main/apps/rlark/pkg/gateway/router.go) 中实际注册的 HTTP 路由。可运行请求参见 [API 调用样例](examples.md)，机器可读子集参见 [OpenAPI 规范](../../api/swagger.yaml)。

除 `POST /api/v1/auth/login` 和 `/metrics` 外，Gateway 路由均要求有效的 Bearer 凭据。除非启用 `--disable-native-login`，原生登录会签发 JWT；启用后该接口返回 HTTP `403`，错误码为 `native_login_disabled`，文案为 `Native username/password login is disabled; use IAM authentication`，且不会设置访问令牌 Cookie。JWT 默认 8 小时过期，可通过 `--jwt-token-ttl` 配置。此开关不影响服务账号令牌。

仅管理员操作包括 Node 和 Domain 变更，以及全部证书、镜像仓库、Addon 接口、系统配置更新和 StorageClass provider/变更接口。系统配置读取及全部 SSH 密钥操作允许任一已认证角色访问。

授权同时按角色与租户作用域执行。Workflow、Job、Task、Pod 路由被限定在调用方所属的租户内：作用域之外的 `?tenant=` 过滤或创建请求体返回 `403`，归属其他租户的具名对象返回 `404`，以免确认该名称存在。`admin` 调用方不受限制。其余资源（包括 SSH 密钥）仍只按角色授权，尚未按具体用户隔离。完整模型见[租户作用域强制](../developer-guide/api-conventions.md#51)。

下表用 `{name}` 表示路径参数；Router 源码中的 Gin 等价写法为 `:name`。Namespaced CRD 路由需要 `namespace` query 参数。

## 场站相机播放试点

管理员可向 `POST /api/v1/rlinf.io/v1alpha1/nodes/{name}/cameras/{camera}/whep?namespace={namespace}`
提交 `application/sdp` offer。启用试点的 Gateway 返回 HTTP 201、SDP answer 和同源
`Location`。播放时每 6 秒向此地址发送 PUT 续约，关闭时发送 DELETE 请求清理。
14 秒未续约即过期。续约时 Gateway 重新确认节点所属集群和推流端，并在 MediaMTX
确认清理前保留并发配额。打开失败或结束播放时，Gateway 撤销对应的机端推流
租约。最后一个读者离开后停止推流，机器人也会回收 20 秒未续约的推流租约。
试点默认关闭。已配置试点的机器人会核对发布路径是否与 Gateway 的 agent、节点
和视图一致。每台机器人最多软件编码一路 480p 视频，每个 Gateway 认证主体
最多 4 路观看会话，每个 Gateway 最多 20 路。试点由单个 Gateway
独占 MediaMTX 实例。Gateway 重启后通过私有管理接口断开旧的 WebRTC 会话，确认
列表为空后恢复播放入口。清理期间 JPEG 服务仍可用。
Gateway 为每个读者向机器人端点
`/cameras/{camera}/h264-lease/{UUID}` 发送 `PUT` 创建或续约源租约，并使用相同 UUID
发送 `DELETE` 释放租约。两种请求均携带 `X-RLark-Camera-Source-Key` 和
`X-RLark-Camera-Source-Path`，后者为 `/{agent}/{node}/{camera}`。机器人只接受与
配置的 Agent 及本地 `NODE_NAME` 相符的路径。

在试点集群的 `s1u-robot` Addon values（v1.7.4 起支持）中同时配置 `cameraRTSPPrefix`
和 `cameraSourceSecret`。例如将前缀设为
`rtsp://media.internal:8554/robot-agent/$(NODE_NAME)`，Secret 名称设为
`s1u-camera-source`。使用集群实际的 Agent 名称及其私有 RTSP 或 RTSPS
地址。Kubernetes 在 `S1U_CAMERA_RTSP_PREFIX` 中展开 `$(NODE_NAME)`，
video 容器会在末尾附加相机视角。机器人集群的 `rlark-system` 命名空间下，
该 Secret 的 `source-key` 提供 `S1U_CAMERA_SOURCE_KEY`。Gateway 通过
`X-RLark-Camera-Source-Key` 向 Addon 发送相同的 `RLARK_CAMERA_SOURCE_KEY`，
密钥至少 32 字节。

在 Gateway 端启用试点还需配置私有 `RLARK_CAMERA_WHEP_ORIGIN` 和
`RLARK_CAMERA_WHEP_CONTROL_ORIGIN`。生产 Compose 部署将 `readerUser`、
`readerPassword`、`controlUser`、`controlPassword` 和 `sourceKey` 保存于 Gateway
身份目录中的受保护 `camera-credentials.json` 文件。Gateway 从该文件读取凭据，
不会将其放入进程环境。reader 和 Control API 必须使用不同用户名。启动清理成功前，
播放入口返回 HTTP 503。

Addon 的两个参数默认均为空。任一参数未设置，或 Secret 中的密钥缺失或过短时，
Addon 继续提供 JPEG 快照和流，H.264 租约接口返回 HTTP 404。

## CRD 资源

| 资源 | 作用域 | 路由 |
|------|--------|------|
| `nodes` | Namespaced | `GET, POST /api/v1/rlinf.io/v1alpha1/nodes`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/nodes/{name}` |
| `workflows` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/workflows`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/workflows/{name}` |
| `jobs` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/jobs`；`GET /api/v1/rlinf.io/v1alpha1/jobs/tags`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/jobs/{name}`；`GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/logs`；`GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/logs/label-values`；`GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/metrics` |
| `tasks` | Namespaced | `GET, POST /api/v1/rlinf.io/v1alpha1/tasks`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/tasks/{name}`；`/api/v1/rlinf.io/v1alpha1/tasks/{name}/tensorboard/{path}` 接受所有方法 |
| `pods` | Namespaced | `GET /api/v1/rlinf.io/v1alpha1/pods`；`GET, PATCH /api/v1/rlinf.io/v1alpha1/pods/{name}`；`GET /api/v1/rlinf.io/v1alpha1/pods/{name}/events`；`GET /api/v1/rlinf.io/v1alpha1/pods/{name}/terminal` |
| `domains` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/domains`；`GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/domains/{name}` |

创建 Job 时，Gateway 会将请求中的 `metadata.name` 保存为展示名，并在响应的 `metadata.name` 中返回系统生成的 `jo-<16 位十六进制字符>` 资源 ID。后续 Job API 请求应使用该返回 ID。

Gateway Router 未暴露 CRD status 子资源路由；状态随普通资源响应返回。

Job 日志接口已实现：`logs` 支持 `from`、`to`、`task`、`pod`、`query`、`cursor`、`order`。`from`、`to` 使用 RFC 3339 时间；`order=asc` 表示后端结果升序，其他值均为降序。提供 `from` 且已配置日志后端时，成功响应包含 `source: "backend"`、`entries`、`hasMore`、`nextCursor`；其他情况（包括后端查询失败）回退到 Pod 日志，响应包含 `source: "pod"` 和 `pods` 数组，数组项含 `taskName`、`podName`、`phase`、`node`、`logs`。

`logs/label-values` 支持 `label`（默认 `pod`）、`from`、`to`、`task`、`pod`，返回 `{ "values": [...] }`；未配置日志后端时数组为空。Job `metrics` 已注册，但当前仅返回 HTTP `501 Not Implemented`。

## 站点机器重接

`POST /api/v1/station/machines/{name}/rejoin` 仅管理员可调用。请求体为 `{ "cluster_id": "cluster-a", "request_id": "retry-001", "robot_model": "s1u" }`；当已有 admission 和 NodeClass 可确定机型时，`robot_model` 可省略。接口返回 `202 Accepted` 和 `{ "data": <NodeOperation> }`。使用 `GET /api/v1/station/node-operations/{name}?cluster_id=cluster-a` 轮询，或使用 `GET /api/v1/station/node-operations?cluster_id=cluster-a` 列表查询，直到 `status.phase` 为 `Succeeded`、`Failed` 或 `Blocked`；失败详情位于有界的 `status.conditions` reason/message，不包含 SSH 密码。

目标机器必须已加入请求中的集群，且具有匹配的 Robot NodeAdmission 和已注册的 RobotModel/NodeClass。使用相同 request ID 重放会返回同一操作。存在活动 Join 时返回 `409`；机型无效或未知时返回 `400`，Machine 不存在时返回 `404`，查询失败时返回 `502`。重接操作进入 `Failed` 或 `Blocked` 终态后，控制台可使用新的 request ID 重试，Gateway 会重新校验原 Machine 和 admission。controller 仅在本地 WireGuard key、原 NodeAdmission、NetworkEnrollment 和成员 Node UID 一致时恢复已有 WireGuard 文档，不会运行普通 Join 的 `Uplink`、`Push`、`Install` 或 `RotateEntryHostKey` 阶段。缺少或不匹配的本地身份会以 `RejoinStateUnavailable` 进入可轮询 `Blocked`，原 admission 和 Node 保持不变。重接使用 NodeClass 已存凭据和现有网络配置，不接受 SSH 密码，不删除或替换 Node/NodeAdmission，也不签发或撤销入网码。

| 方法 | 路径 |
|------|------|
| `POST` | `/api/v1/station/machines/{name}/rejoin` |

## 集群与证书

| 方法 | 路径 |
|------|------|
| `GET` | `/api/v1/clusters` |
| `GET` | `/api/v1/clusters/{cluster_id}` |
| `GET` | `/api/v1/certificates/agent` |
| `GET` | `/api/v1/certificates/agent/{cluster_id}` |
| `POST` | `/api/v1/certificates/agent` |

Gateway 虽注册了 `POST /api/v1/certificates/revoke`，但该接口尚未实现，不应视为可用 API。

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
| `GET, PUT` | `/api/v1/system-config` |

系统配置分为 `ssh` 和 `log` 两类。`ssh.jumpHost` 必须是不含协议、用户、路径和端口的主机名或 IP；设置 `ssh.jumpPort` 时，其值必须为 1 到 65535 之间的整数。将 `log.backend` 设置为 `none` 可关闭历史日志查询，设置为 `sls` 时必须提供完整的 SLS 连接参数。响应中的 SLS 敏感凭据显示为 `****`，将该占位符原样提交表示保留已存储值。`PUT` 成功后返回最终生效且已掩码的配置。

可选的 `deployment` 分类沿用 `rlarkadm` `DeployConfig` 中与数据面 Agent 相关的子集，用于控制签发数据面 Agent 集群后展示的部署 YAML 默认值。仅接受 `controlPlaneAddress`、`sshAddress`、`insecureSkipTlsVerify`，以及 Kubernetes 下的 `kubeconfig`、`agentImage`、`image`、`imagePullPolicy`、`imagePullSecrets` 和 `containerdSocket`。控制面组件、数据库、证书、Docker 和 Raw 部署字段会被拒绝。`controlPlaneAddress` 为空时使用签发接口返回的 Server 地址；镜像拉取策略可为 `Always`、`IfNotPresent` 或 `Never`。

镜像仓库凭证使用不可变的 `ir-<16 位小写十六进制字符>` ID；展示 `name` 可以重复或修改。创建请求必须包含 `name`、`registry`、`username`、`password` 和 `clusterSelection`：

```json
{"name":"生产 Harbor","registry":"harbor.example.com","username":"robot","password":"secret","clusterSelection":{"mode":"Selected","clusters":["cluster-a"]}}
```

`clusterSelection.mode` 可为 `None`（仅保存）、`Selected`（一个或多个逻辑集群名）或 `All`（当前及未来集群）。`None` 和 `All` 模式下 `clusters` 必须为空。响应不返回密码，但会返回 `id`。`POST` 返回 `201 Created`。执行 `PUT` 时，省略 `password` 表示保留原密码，显式空密码非法。`DELETE` 返回 `202 Accepted`，因为 Replication 和 Delivery 会异步删除已分发的 Secret。

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
# 站点集群态势摘要

`GET /api/v1/station/summary` 会遍历所有 `NetworkEnrollment` 分页，可选传入 `cluster_id`。Gateway 会按 namespace 和同名 RLark Node 关联入网记录。阶段统计读取 `status.node.state`，`Retired` 优先。机器人、电源、占用、自检和相机统计读取该 Node 的标签、注解及条件。Node 缺失时回退到入网记录自身的元数据和条件。

`GET /api/v1/station/stations` 支持 `q`、`phase`、`model`、`zone`、`power_state`、`occupancy`、`alert_only` 和 `sort` 参数。带过滤条件的请求会聚合全部分页后再应用现有 `limit`（1–500）和数字偏移量 `continue`，响应包含 `totalMatched`。`phase` 参数不区分大小写。不带过滤条件时继续使用 Kubernetes 原生 continuation token。

指定的集群缺少唯一的 `spec.agentNamespace` 映射时，两个接口均返回 `409 Conflict`。查询全场站汇总或筛选列表时，如果多个集群共用的命名空间内有入网记录，也会返回 `409 Conflict`。请为各集群分配独立的 agent 命名空间后重试。
