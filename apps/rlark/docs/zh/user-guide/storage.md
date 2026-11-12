# 存储

## 存储类型

Job 编辑器可创建两种存储：主机目录和 PVC 存储挂载。通过 API 提交的 Job 还可以使用其他 Kubernetes 原生 PodTemplate volume，编辑器对这类卷只做展示。其生命周期由 Kubernetes 原生 PodTemplate volume 形态决定：

| 类型 | 适用场景 | 生命周期 |
|------|----------|----------|
| 主机目录 | 数据已在节点上，高 I/O | 任务生命周期操作不删除数据 |
| 对象存储（Pod 级临时 PVC） | 当前 Web UI 为 PVC 挂载写入的形态 | Kubernetes 随 Pod 创建和删除 PVC |
| 已有持久 PVC 引用 | Task volume 使用 `persistentVolumeClaim.claimName` | RLark 不创建或删除该 PVC；生命周期由管理员管理 |
| 其他 Kubernetes 原生卷（`emptyDir`、`configMap`、`secret` 等） | 通过 API 提交，Job 编辑器仅展示 | 由卷类型本身决定；编辑或复制任务时 RLark 原样写回其原始定义 |

## 主机目录

- 管理员需确认目标节点上路径存在且权限正确
- 填写源路径（节点文件系统）和挂载路径（容器文件系统）
- 任务删除后数据保留在节点上

## 对象存储

- 使用 Kubernetes 通用临时卷和 RLark 管理的 StorageClass
- 当前 Web UI 将 PVC 挂载写为 `ephemeral.volumeClaimTemplate`，因此新建或编辑的 Job 使用 Pod 级临时 PVC
- 每个 PVC 挂载默认申请 10Gi，可配置范围为 1–200Gi
- 先选择集群；下拉框会列出已配置为该集群目标的 StorageClass 声明
- 每个 Worker Pod 使用独立的 PVC

## SMB / NAS 共享

- 创建存储类时，存储类型选择 **SMB / NAS 共享**，然后填写服务器地址、共享名、可选的子路径、用户名、密码和可选的域
- Job 选择 SMB 存储类的方式与对象存储完全相同；PVC 通过 rclone CSI 驱动挂载该共享（或其子路径）
- 创建时只检查服务器 445 端口是否可达，账号密码错误会在任务挂载卷时报出；挂载失败时请查看 Pod 事件
- SMB 存储类不支持在线浏览文件
- 编辑时密码留空表示保持原密码；创建后不能修改存储类型

## 管理存储类

进入 **存储** 页面可查看对象存储和 SMB 配置、目标集群、提供商和位置（存储桶，或 SMB 的 `//服务器/共享/路径`）。创建或更新存储类会在控制面保存其声明，随后将生成的 rclone CSI StorageClass 和 Secret 异步分发到目标集群。为 Worker 配置 PVC 挂载前，需要先创建对应的存储类并等待下发完成。

使用存储类前，请确认目标集群已安装 rclone CSI 驱动，且可以访问对象存储服务。若 PVC 一直处于 `Pending`，请检查资源分发状态、驱动、凭据和网络连通性。

> **截图说明：** 截图来自示例环境，资源名称和数据仅供说明，实际环境会有所不同。

![存储管理](../../images/ui/storage-file-browser.png)

## 在训练任务中使用存储

创建任务时，在 Worker 配置步骤中：
1. 选择存储类型（hostPath 或 PVC）
2. 输入源路径（hostPath）或选择 StorageClass（PVC）
3. 输入容器挂载路径
4. 训练代码读写挂载路径

## 检查读写

验证存储链路：
1. 源位置可访问
2. 容器挂载正确
3. 应用可读取输入数据
4. 应用可写入输出数据
5. 对当前 Web UI 创建的 Job，应将 PVC 视为 Pod 级临时存储，并在停止或重启前复制需要保留的输出。请检查 `spec.workload.template.spec.volumes`：`ephemeral.volumeClaimTemplate` 是 Pod 级临时卷，`persistentVolumeClaim.claimName` 引用已有且由外部管理的 PVC

## 生命周期

- 已有 `persistentVolumeClaim`：RLark 不创建、收养、修改或删除引用的 PVC；其生命周期和数据保留不属于 Task 管理范围
- Pod 级 `ephemeral.volumeClaimTemplate`：删除 Worker Pod 时删除对应临时 PVC，新 Pod 获得新 PVC
- hostPath：生命周期操作不删除节点数据
- 其他 Kubernetes 原生卷：Job 编辑器无法创建或修改；编辑或复制任务时会原样写回其原始定义，因此生命周期仍由该卷类型本身决定

已有 PVC 引用必须填写 `claimName`，且该 claim 必须已存在于 workload namespace。挂载或绑定失败由 Kubernetes 通过 workload 和 Pod 状态报告。删除被引用的 PVC 属于外部管理操作；删除前应确认没有 workload 使用它，并考虑绑定 PV 的 reclaim policy。

`pvcStorageMap` 和 `pvcSizeGbMap` 不属于 canonical Task Schema。当前 Web UI 和 Kubernetes 原生示例写入 `ephemeral.volumeClaimTemplate`；破坏性升级时必须替换依赖已移除字段的旧资源。

## API 等效操作

使用 StorageClass、provider 和对象文件接口，详见 [Storage API](../storage-api.md)。

## 挂载 Endpoint

创建 S3 存储类时可填写“挂载 Endpoint（可选）”。留空沿用 Endpoint；填写后仅影响目标集群的 CSI 挂载，Gateway 校验和文件浏览仍使用原 Endpoint。适用于 Gateway 通过公网、云端 Job 通过云内网络访问同一个 bucket。地址格式校验不代表云端可达，需提交只读测试 Job 验证。所有目标共用此地址；不同网络环境应使用独立存储类。编辑时清空该字段会恢复沿用 Endpoint。SMB 不提供此选项。

S3 存储类还可限定 bucket 内对象 `prefix`，并选择 rclone VFS 缓存模式（`writes` 或 `off`）。现有类缺省仍为 `writes`。需要无缓存写入的任务应使用独立存储类，并在生产使用前验证实际集群上的关闭错误传播和失败重传。生成的 `remotePath` 规则见 [Storage API](../storage-api.md)。


### KS3 multipart 验收限制

缓存模式 `off` 不保证提供商兼容性。已测 KS3 配置下，CSI rclone v0.2.0（内嵌 rclone v1.71.2）的 multipart 完成后对象 ETag 是不同于内容 MD5 的 32 位十六进制值；即使独立下载核对字节一致，流式 close 仍可能返回 EIO（`corrupted on transfer`）。不要通过关闭完整性检查验收此挂载；用于归档前必须验证驱动与提供商组合。未知长度流还需要合适的 multipart 分片大小：默认 5 MiB 和 10,000 个分片只允许约 48.8 GiB。当前存储表单不暴露分片大小配置，大 checkpoint 验收需要兼容的驱动/配置和真实故障恢复测试。各挂载有独立目录缓存，close 成功本身也不承诺另一挂载立即可见。


## 每个 S3 存储的挂载配置

创建/更新接受 `mount_profile`（`default`、`read`、`archive`）和 `mount_options`（`key=value` 字符串数组）。列表返回这两项及 `effective_mount_options`、`effective_backend_options`。更新时省略字段保留原值；空选项数组清除覆盖项。default 保留现有设置；read 强制只读挂载、目录缓存五分钟；archive 强制关闭写缓存、目录缓存五秒、S3 分片 32 MiB、分片并发 4。这是配置 profile，不代表已通过 KS3 归档验收。

显式选项覆盖 profile 默认值。重复/未知选项、与 `vfs_cache_mode` 或 profile 强制设置矛盾的配置被拒绝。支持：`vfs-cache-mode`、`read-only`、`fast-list`、`dir-cache-time`、`attr-timeout`、`poll-interval`、`vfs-write-back`、`vfs-cache-max-age`、`buffer-size`、`transfers`、`checkers`、`low-level-retries`、`tpslimit`、`tpslimit-burst`、`s3-chunk-size`、`s3-upload-concurrency`。后端参数写入 remote 配置中的 `chunk_size` / `upload_concurrency`。不能通过这里注入凭证、路径、控制接口或 `ignore-checksum`。SMB 不接受 S3 profile 或覆盖项。

profile 不提供账号级总请求预算。设置限速前应确认 429 的来源和作用域，并统筹 Fleet、DataTools 和镜像仓库；CSI 与 Gateway 是不同请求路径。缩短目录缓存不代表原子发布或即时可见。修改仅对新挂载生效，应在维护窗口停止、更新、启动相关 Job；存储变更不会自动重启活动 Job。
