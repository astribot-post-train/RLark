# Storage API 文档

## 概述

Storage API 用于管理跨多个集群的 RLark StorageClass 声明。Gateway 将每个声明保存在控制面，Replication 和 Delivery 控制器再将生成的 rclone CSI StorageClass 及其 Secret 异步分发到所选数据面集群。

## API 端点

### 1. 获取 StorageClass 列表
**GET** `/api/v1/storage/storageclass`

从控制面列出由 RLark 管理的 StorageClass 声明。该接口不会查询 Agent，也不会校验数据面资源的实时状态。

#### 查询参数
- `clusters`（可选）：逗号分隔的集群 ID 列表，例如 `?clusters=agent-beijing,agent-shanghai`。提供该参数时，响应仅包含其已配置目标集群与指定 ID 有交集的声明；不传则返回全部已管理声明。

#### 响应示例
```json
{
  "data": {
    "ceph-rbd": {
      "name": "ceph-rbd",
      "storageClassName": "sc-0123456789abcdef",
      "clusters": ["agent-beijing", "agent-shanghai"],
      "description": "Ceph RBD StorageClass",
      "bucket": "",
      "provider": "s3",
      "endpoint": "https://s3.amazonaws.com",
      "region": "us-east-1",
      "pathStyle": false,
      "accessKeyId": "AKIA..."
    }
  },
  "success": true
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | `string` | 供人阅读的 StorageClass 声明名称 |
| `storageClassName` | `string` | 已下发到目标数据面集群的生成式 `sc-<id>` Kubernetes StorageClass 名称；PVC 的 `spec.storageClassName` 必须使用该值 |
| `clusters` | `[]string` | 该 StorageClass 已配置的目标集群 ID 列表 |
| `description` | `string` | 可读描述 |
| `bucket` | `string` | 存储桶名称 |
| `provider` | `string` | 存储提供商类型（s3、gcs、azureblob 等） |
| `endpoint` | `string` | 存储服务端点地址 |
| `region` | `string` | 存储区域 |
| `pathStyle` | `bool` | 是否使用 path-style 寻址 |
| `accessKeyId` | `string` | 已掩码的 Access Key ID（`<前两个字符>*****<后两个字符>`）；不会返回 Access Key Secret |

### 2. 获取存储提供商列表
**GET** `/api/v1/storage/storageclass/provider`

列出支持的存储提供商。所有已认证用户均可读取该目录，以及创建、更新和删除 StorageClass。该目录是创建、更新 StorageClass 时 `provider` 合法取值的唯一来源：提交 `value`，`name` 仅用于展示。rclone 遇到无法识别的 provider 不会报错，只会打印告警并回退到通用配置，因此 Gateway 会直接拒绝不在该目录中的 `provider`。

#### 响应示例
```json
{
  "data": [
    { "name": "AWS S3", "value": "AWS" },
    { "name": "阿里云 OSS", "value": "Alibaba" },
    { "name": "MinIO", "value": "Minio" }
  ],
  "success": true
}
```

### 3. 创建 StorageClass
**POST** `/api/v1/storage/storageclass`

在控制面创建 StorageClass 声明。Replication 和 Delivery 控制器会将生成的 rclone CSI StorageClass 及其 Secret 异步分发到请求指定的目标集群。

Gateway 在存储前会对 S3 配置做规范化和校验：

- `provider` 必须命中提供商目录。canonical 取值、展示名以及旧客户端写入的历史拼写都会被接受，但记录中始终保存 canonical 取值；无法识别的取值返回 `400`。
- `endpoint` 必须是不含存储桶名的 `http`/`https` URL，缺少 scheme 时默认补 `https`。存储桶名绝不会拼进 endpoint，因为 Gateway 的 S3 客户端和 rclone 都会自行拼接 virtual-hosted URL。
- 只有发布了区域 endpoint 格式的提供商（如阿里云 OSS）才允许 `endpoint` 留空，此时必须提供 `region`，endpoint 由 region 推导得出。
- 对仅支持 virtual-hosted 寻址的提供商（如阿里云 OSS、腾讯云 COS、华为云 OBS），`path_style` 会被强制置为 `false`；其他提供商保留提交的取值。
- 完成结构校验后，Gateway 会在有界超时时间内发起带认证的 `HeadBucket` 请求。若配置的凭据无法访问存储桶，创建和更新请求会返回 `400 INVALID_REQUEST`；响应不会包含提供商错误详情或任何凭据。

该校验上线前写入的历史记录会在读取时做 canonical 化，确保 Gateway 的 S3 客户端与下发到数据面的 rclone 配置在 provider 和寻址方式上保持一致。

校验失败会使用稳定的机器可读错误码和不含敏感信息的结构化详情。缺少必填字段时，接口返回 `400`、错误码 `STORAGE_CLASS_REQUIRED_FIELDS` 以及 `details.fields`，例如：

```json
{
  "code": "STORAGE_CLASS_REQUIRED_FIELDS",
  "message": "bucket are required",
  "details": { "fields": ["bucket"] }
}
```

客户端必须使用 `code` 和 `details.fields` 生成本地化字段提示，而不是翻译面向人类的 `message` 文本。

#### SMB（NAS）StorageClass

将 `type` 设为 `smb` 即可挂载 SMB/CIFS 共享，而不是对象存储。`type` 默认为 `s3`，该字段出现前写入的历史记录读取时按 `s3` 处理。SMB 存储类通过 rclone 的 `smb` 后端使用同一个 rclone CSI 驱动（`rclone.csi.veloxpack.io`），不需要额外安装驱动。请求示例：

```json
{
  "name": "nas-post-train",
  "clusters": ["rlark-local-dev-gpu"],
  "type": "smb",
  "host": "10.0.0.10",
  "share": "shared",
  "sub_path": "team/data",
  "username": "user",
  "password": "********",
  "domain": "corp.example.com"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `host` | 是 | 只填主机名或 IP；开头的 `//` 或 `smb://` 会被去掉。带端口或路径会被拒绝。 |
| `share` | 是 | 单个共享名。 |
| `sub_path` | 否 | 要挂载的共享内目录；首尾的 `/` 会被去掉，`.`、`..` 段会被拒绝。 |
| `username` | 是 | 列表接口会返回，不视为机密。 |
| `password` | 创建时必填 | 不会返回。保存在控制面声明中，下发到数据面的 rclone 配置里使用 rclone 的混淆（obscure）形式。混淆不等于加密，请按机密妥善保护相关 Secret。 |
| `domain` | 否 | Windows/AD 域。 |

SMB 存储类会忽略 S3 字段（`provider`、`endpoint`、`bucket`、`region`、`path_style`、访问密钥），也不使用提供商目录。生成的 StorageClass 参数为 `remote: smb`、`remotePath: <share>/<sub_path>`。校验只检查 `host` 的 TCP 445 端口能否在超时时间内连通；Gateway 不会登录共享，因此账号密码错误要到 Pod 挂载卷时才会暴露。文件浏览接口（第 6–9 节）对 SMB 存储类返回 `400`。

### 4. 更新 StorageClass
**PUT** `/api/v1/storage/storageclass/{name}`

更新指定 StorageClass 声明的对象存储配置和目标集群。请求中的 `clusters` 是期望的最终目标集群集合；分发流程会从未包含的集群清理资源。更新时 `access_key_id` 和 `access_key_secret` 均可留空；Gateway 会读取并复用相应的已存储值，不会将凭据替换为空值。列表返回的 `accessKeyId` 使用五个星号掩码；客户端提交的 `access_key_id` 只要包含 `*****`，Gateway 就会将其视为未变更，而不是替换值。保留凭据时，Gateway 会使用之前存储的 Access Key ID 和 Secret 校验更新后的连接。对于 SMB 存储类，`password` 留空表示保留已存储的密码。更新时不能修改存储 `type`，如需更换请新建存储类。

### 5. 删除 StorageClass
**DELETE** `/api/v1/storage/storageclass/{name}`

删除 StorageClass 声明。分发 finalizer 会异步移除其 Delivery 声明，以及所有关联集群中的生成 StorageClass 和 Secret。

### 6. 列出存储桶文件
**GET** `/api/v1/storage/storageclass/{name}/list`

列出 StorageClass 存储桶中的文件。Gateway 会直接从控制面 StorageClass 声明读取存储桶。

#### 路径参数
- `name`：StorageClass 名称

#### 查询参数
- `prefix`（可选）：要列出的对象键前缀。非根目录前缀使用 `/` 分隔且以 `/` 结尾，例如 `a/b/c/`。
- `delimiter`（可选）：对象键分隔符。默认值为 `/`；使用该分隔符时，直属子目录会在 `common_prefixes` 中返回。
- `maxKeys`（可选）：返回的对象和公共前缀的最大数量。

#### 目录导航

对象存储使用前缀而不是真实目录。文件浏览器将非空目录前缀视为以 `/` 结尾的 canonical 值。例如，目录路径 `a/b/c` 表示为 `a/b/c/`；其上级目录为 `a/b/`，面包屑目标依次为 `a/`、`a/b/` 和 `a/b/c/`。即使某个前缀不包含直属对象或子前缀，上述导航仍然可用。

#### 响应示例
```json
{
  "data": [
    {"name": "model-checkpoint.pt", "size": 1048576, "modified": "2026-07-30T10:00:00Z"},
    {"name": "logs/training.log", "size": 2048, "modified": "2026-07-30T09:30:00Z"}
  ],
  "success": true
}
```

### 7. 上传文件
**POST** `/api/v1/storage/storageclass/{name}/upload`

向指定 StorageClass 存储桶上传文件，使用 multipart/form-data 格式。

#### 路径参数
- `name`：StorageClass 名称

#### 请求体
multipart/form-data，字段 `file` 为上传的文件。

#### 响应示例
```json
{
  "data": {"key": "model-checkpoint.pt", "size": 1048576},
  "success": true
}
```

### 8. 下载文件
**GET** `/api/v1/storage/storageclass/{name}/object/*key`

下载指定存储桶中的对象，返回原始文件内容。

#### 路径参数
- `name`：StorageClass 名称
- `key`：对象路径（如 `model-checkpoint.pt` 或 `logs/training.log`）

#### 响应
- `200`：文件内容（二进制流）
- `404`：文件不存在

### 9. 删除文件
**DELETE** `/api/v1/storage/storageclass/{name}/object/*key`

删除指定存储桶中的对象。

#### 路径参数
- `name`：StorageClass 名称
- `key`：对象路径

#### 响应示例
```json
{
  "data": {"deleted": true},
  "success": true
}
```

## 实现说明

### 控制面分发架构

```
Gateway ──▶ 控制面 StorageClass 声明
                    │
                    ▼
          Replication Controller ──▶ Delivery Controller ──▶ K8s API
```

1. Gateway 校验 StorageClass 请求，并在控制面保存复制声明。
2. Replication Controller 为已配置目标集群创建 Delivery 声明。
3. 各 Delivery Controller 将生成的 rclone CSI Secret 和集群级 StorageClass 应用到本地 Kubernetes 集群。
4. 更新会替换期望目标集群集合；删除通过分发 finalizer 触发异步清理。

列表中的声明表示已配置的分发意图。创建 PVC 前，请确认下发已完成，且目标集群中的 rclone CSI 驱动正常、对象存储凭据有效，并且可连通存储服务。

### 文件结构
- **`../pkg/gateway/storage_handler.go`** - 存储 API 处理函数和生成的清单
- **`../pkg/controllermanager/replication/controller.go`** - 控制面复制控制器
- **`../pkg/agent/controllers/delivery/controller.go`** - 数据面分发控制器
- **`../pkg/gateway/router.go`** - 路由注册

### 数据结构
```go
type StorageClassData struct {
    Name        string   `json:"name"`
    Clusters    []string `json:"clusters"`
    Description string   `json:"description"`
    Bucket      string   `json:"bucket"`
    Provider    string   `json:"provider"`
    Endpoint    string   `json:"endpoint"`
    Region      string   `json:"region"`
    PathStyle   bool     `json:"pathStyle"`
}

type Provider struct {
    Name  string `json:"name"`
    Value string `json:"value"`
}
```

## 使用示例

### 获取全部已管理 StorageClass
```bash
curl "http://localhost:8080/api/v1/storage/storageclass"
```

### 获取以指定集群为目标的 StorageClass
```bash
curl "http://localhost:8080/api/v1/storage/storageclass?clusters=agent-beijing"
```

### 获取存储提供商列表
```bash
curl "http://localhost:8080/api/v1/storage/storageclass/provider"
```

## 与 Task PVC 挂载的集成

Task 通过 Kubernetes 通用临时卷声明远程存储：

```yaml
workload:
  template:
    spec:
      volumes:
        - name: data
          ephemeral:
            volumeClaimTemplate:
              spec:
                accessModes: [ReadWriteOnce]
                storageClassName: ceph-rbd
                resources:
                  requests:
                    storage: 10Gi
```

Kubernetes 根据 `volumeClaimTemplate` 为每个 Pod 创建 PVC，并随 Pod 删除。当前 Web UI 写入这种通用临时卷形式，并通过 Storage API 获取已为所选集群配置的 RLark 管理 StorageClass 声明。`pvcStorageMap` 和 `pvcSizeGbMap` 不属于 canonical Task Schema；破坏性升级时必须替换依赖这些字段的旧资源。

## 独立挂载 Endpoint

S3 创建、更新及列表响应新增可选字段 `mount_endpoint`。`endpoint` 仍用于 Gateway 的凭证校验（`HeadBucket`）和文件 API；CSI Secret 使用非空的 `mount_endpoint`。缺省或空值沿用 `endpoint`，更新时传空值会清除覆盖。两端共用 provider、bucket、region、凭证和 path-style，并使用同一套 endpoint URL 规范化。SMB 拒绝非空的 `mount_endpoint`。

云专用存储类可以让 Gateway 使用公网地址，CSI 使用云内挂载地址。Gateway 不探测挂载地址：必须在目标集群验证 DNS、TLS、权限和实际挂载；配置成功或列表可见不代表分发或挂载就绪。同一声明的所有目标集群使用相同挂载地址，需要不同地址时创建独立声明。不能假设已有挂载卷会自动重载地址变更，应先用新测试 Pod 验证再迁移工作负载。

```json
{
  "name": "ks3-post-train-ksyun",
  "type": "s3",
  "clusters": ["rlark-ksyun-gpu"],
  "provider": "Other",
  "endpoint": "https://ks3-cn-ningxia.ksyuncs.com",
  "mount_endpoint": "https://ks3-cn-ningxia-internal.ksyuncs.com",
  "bucket": "stardust",
  "region": "cn-ningxia",
  "path_style": false
}
```

这是环境专属配置示例，不能原样提交：需通过受保护的渠道补充获授权凭证，不得将凭证写入源码或日志。资源仍异步分发，Job 使用前需确认目标集群实际生成的 `sc-<id>`。

## S3 前缀与 VFS 缓存模式

S3 声明的创建/更新请求和列表响应支持可选字段 `prefix`、`vfs_cache_mode`。`prefix` 是 bucket 内以 `/` 分隔的对象路径，并作为挂载后的根目录；例如 bucket 为 `stardust`、prefix 为 `ks3-regular` 时，生成的 rclone 参数为 `remotePath: stardust/ks3-regular`。前缀为空时挂载 bucket 根目录。`vfs_cache_mode` 仅接受 `writes` 或 `off`；创建时省略使用 `writes`，更新时省略保留已存储模式。该设置只影响生成的 CSI StorageClass 挂载选项。前缀仅限定 CSI 挂载范围，Gateway 文件 API 仍使用 bucket 相对 key，因此前缀不是授权边界。SMB 不接受这些 S3 专属字段。

大文件输出若要求关闭文件时对象已上传，应使用独立存储类并设置 `vfs_cache_mode: off`。关闭缓存后，远端上传错误可能从文件操作（例如 close）返回；应用必须检查错误并重传失败文件。该设置不提供应用级原子发布保证，也不能替代端到端故障测试。

```json
{
  "name": "fleet-ks3",
  "type": "s3",
  "clusters": ["rlark-ksyun-gpu"],
  "provider": "Other",
  "endpoint": "https://ks3-cn-ningxia.ksyuncs.com",
  "bucket": "stardust",
  "region": "cn-ningxia",
  "path_style": false,
  "prefix": "ks3-regular",
  "vfs_cache_mode": "off"
}
```

Gateway 使用配置的 endpoint 执行 `HeadBucket` 校验。403 是授权响应，单凭它不能判断 endpoint 不可达；应检查凭证、bucket policy 和所需权限。不得将凭证写入源码、示例或日志。


### KS3 multipart 验收限制

缓存模式 `off` 不保证提供商兼容性。已测 KS3 配置下，CSI rclone v0.2.0（内嵌 rclone v1.71.2）的 multipart 完成后对象 ETag 是不同于内容 MD5 的 32 位十六进制值；即使独立下载核对字节一致，流式 close 仍可能返回 EIO（`corrupted on transfer`）。不要通过关闭完整性检查验收此挂载；用于归档前必须验证驱动与提供商组合。未知长度流还需要合适的 multipart 分片大小：默认 5 MiB 和 10,000 个分片只允许约 48.8 GiB。当前存储表单不暴露分片大小配置，大 checkpoint 验收需要兼容的驱动/配置和真实故障恢复测试。各挂载有独立目录缓存，close 成功本身也不承诺另一挂载立即可见。


## 每个 S3 存储的挂载配置

创建/更新接受 `mount_profile`（`default`、`read`、`archive`）和 `mount_options`（`key=value` 字符串数组）。列表返回这两项及 `effective_mount_options`、`effective_backend_options`。更新时省略字段保留原值；空选项数组清除覆盖项。default 保留现有设置；read 强制只读挂载、目录缓存五分钟；archive 强制关闭写缓存、目录缓存五秒、S3 分片 32 MiB、分片并发 4。这是配置 profile，不代表已通过 KS3 归档验收。

显式选项覆盖 profile 默认值。重复/未知选项、与 `vfs_cache_mode` 或 profile 强制设置矛盾的配置被拒绝。支持：`vfs-cache-mode`、`read-only`、`fast-list`、`dir-cache-time`、`attr-timeout`、`poll-interval`、`vfs-write-back`、`vfs-cache-max-age`、`buffer-size`、`transfers`、`checkers`、`low-level-retries`、`tpslimit`、`tpslimit-burst`、`s3-chunk-size`、`s3-upload-concurrency`。后端参数写入 remote 配置中的 `chunk_size` / `upload_concurrency`。不能通过这里注入凭证、路径、控制接口或 `ignore-checksum`。SMB 不接受 S3 profile 或覆盖项。

每个生成的 S3 StorageClass 都使用稳定且按存储类隔离的 rclone remote 别名（`s3-<storage-class-id>`），并同时写入 `remote` 和 `configData`。这样同一节点插件上的读类和归档类不会共享可变的 rclone 配置。它只隔离配置，不修复提供商的校验语义。

profile 不提供账号级总请求预算。设置限速前应确认 429 的来源和作用域，并统筹 Fleet、DataTools 和镜像仓库；CSI 与 Gateway 是不同请求路径。缩短目录缓存不代表原子发布或即时可见。修改仅对新挂载生效，应在维护窗口停止、更新、启动相关 Job；存储变更不会自动重启活动 Job。
