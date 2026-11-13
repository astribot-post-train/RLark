# Fleet NAS 固定路径契约

生效约定：2026-11-12。本文固定后续开发路径；现场迁移与跨站验收单独记录，不代表所有 Job 已切换。English: [Fleet NAS path contract](fleet-storage-paths.md)。

## 唯一目录与挂载

NAS 为 `//10.11.5.12/post-train_group`，宿主机挂载根为 `/mnt/post_train`。容器使用相同完整绝对路径，不创建 `/fleet`、`/mnt/rl` 别名或兼容软链接。

| 用途 | 宿主机与容器统一路径 | 权限 |
| --- | --- | --- |
| 个人 Fleet 源码 | `/mnt/post_train/RL/user/<name>/fleet` | 开发者可写，Job 只读 |
| Liangcai 源码 | `/mnt/post_train/RL/user/liangcai/fleet` | 工作目录；`PYTHONPATH` 设置为该目录的 `src` |
| 输入数据集 | `/mnt/post_train/RL/fleet-data/datasets` | Job 默认只读 |
| 模型和 tokenizer | `/mnt/post_train/RL/fleet-data/models` | Job 默认只读 |
| 采集结果 | `/mnt/post_train/RL/fleet-data/collections/<run-id>` | 仅所属 Run 写入 |
| Checkpoint | `/mnt/post_train/RL/fleet-data/checkpoints/<run-id>` | 仅所属 Run 写入 |
| 已发布文件 | `/mnt/post_train/RL/fleet-data/artifacts/<artifact-id>` | 发布后不可原地覆盖 |
| 临时写入 | `/mnt/post_train/RL/fleet-data/staging/<run-id>` | 仅所属 Run 写入 |
| 迁移与校验清单 | `/mnt/post_train/RL/manifests` | 执行者写入；不存凭证 |

`<run-id>` 是一次执行的稳定 ID；重新执行使用新 ID，不能仅用可复用的 Job 名称代替。既有 KPFS 数据和旧 NAS 数据保持原路径，只有清单确认的对象才迁移；不自动复制、删除或软链接外部数据。目录名不是授权边界，各节点必须验证实际 NAS ACL 和 Job 身份。

## Job 配置

镜像只提供 Python、Torch、Ray、RLinf 和系统依赖。源码位置由 Job 配置，不是镜像约定。以下是 Pod 模板片段，合入 RLark Job 的 Kubernetes workload；镜像使用现有私有仓库固定 digest，本次不使用 GHCR。

```yaml
spec:
  containers:
    - name: main
      workingDir: /mnt/post_train/RL/user/liangcai/fleet
      env:
        - name: PYTHONPATH
          value: /mnt/post_train/RL/user/liangcai/fleet/src
        - name: FLEET_FRONTEND_ROOT
          value: /mnt/post_train/RL/user/liangcai/fleet/frontend/dist
        - name: RLINF_COLLECTION_ROOT
          value: /mnt/post_train/RL/fleet-data/collections/run-20261112-example
      volumeMounts:
        - {name: source, mountPath: /mnt/post_train/RL/user/liangcai/fleet, readOnly: true}
        - {name: models, mountPath: /mnt/post_train/RL/fleet-data/models, readOnly: true}
        - {name: datasets, mountPath: /mnt/post_train/RL/fleet-data/datasets, readOnly: true}
        - {name: collection, mountPath: /mnt/post_train/RL/fleet-data/collections/run-20261112-example}
  volumes:
    - name: source
      hostPath: {path: /mnt/post_train/RL/user/liangcai/fleet, type: Directory}
    - name: models
      hostPath: {path: /mnt/post_train/RL/fleet-data/models, type: Directory}
    - name: datasets
      hostPath: {path: /mnt/post_train/RL/fleet-data/datasets, type: Directory}
    - name: collection
      hostPath: {path: /mnt/post_train/RL/fleet-data/collections/run-20261112-example, type: Directory}
```

示例 Run 目录必须由操作方事先创建，启动新 Run 时替换所有对应路径。仅挂载任务需要的子目录，不把整个 NAS 交给工作负载。checkpoint、staging 和 artifact 使用同路径挂载规则。需要先在所选节点确认 `findmnt -T /mnt/post_train` 为预期 CIFS 挂载；`Directory` 只检查目录存在，不能防止 NAS 消失后写入节点同名空目录。启动脚本应检查 `/proc/self/mountinfo` 中实际挂载类型（原生 SMB 为 `cifs`；CSI 路径单独验证）并读取源码后才运行；检查失败直接退出。NAS 会话在运行中失效仍需超时和恢复处理。

`.venv`、site-packages、缓存、JWT/数据库凭证和 PostgreSQL PGDATA/WAL 不放共享源码目录。缓存留在各节点本地并按实例区分。前端资源在本地构建后随源码发布；源码快照本身不能证明运行镜像包含兼容依赖。

## 数据一致性与迁移

开发者可以编辑固定源码入口，运行任务必须冻结所用目录，并记录 commit、dirty 状态、diff 摘要、配置和镜像 digest。模块加载过程中不得修改共享目录。生产任务如使用版本化快照，仍以快照完整路径同路径挂载，不重映射为短别名。

源数据按 `storage_id + relative_path` 引用；站点挂载映射与业务身份分离。该元数据契约需要 Fleet/DataTools 另行适配，本次不把它描述为已完成接口。

输出先进入 staging；完成后校验数量、大小和 SHA-256，确认从另一客户端可读，再发布 artifact 和完成清单。SMB 直接挂载的同文件系统 rename 可以用于发布目录；不得将 rclone 的 rename、关闭文件或 flock 推断为跨站原子提交/持久性保证。NAS 文件发布与 PostgreSQL 提交是两个步骤，使用 staging / committed / failed 状态恢复，不重放结果未知的机器人动作。

迁移先盘点源码、版本和未提交内容，在新 staging 目录准备完整 checkout、构建资源和摘要清单。目标有不同内容时停止覆盖；经用户决定、并确认无活动任务使用后，将旧目录重命名为带时间戳备份，再切换固定入口。两次 rename 间入口可能暂时不存在，不允许有读取任务。切换失败恢复原目录。迁移保留旧目录及校验清单，不删除数据集、标注或旧数据。

数据库目标为金山云独立 Fleet PostgreSQL 服务，与 DataTools 共享目录和标注表；这替代此前复用现有实例的讨论。其 PGDATA 使用云端持久磁盘，不放 NAS/KS3。本次只固定文件路径，不迁库；现有 RLark/Station 本地 PostgreSQL 保持原状。

## 验收和状态

- 本地、机器人和金山云分别验证同路径只读挂载、Python 模块来源和源码 SHA-256；未测站点必须标待验收。
- 使用独立 Run 测试读写权限、不同客户端摘要可见性、staging 中断恢复、NAS 断网、Pod/节点重启，不修改真实数据或机器人状态。
- 新挂载不可用必须启动失败；不得静默创建本地替代目录。rclone 的 `dir-cache-time=5m` 不是端到端可见性上限。
- 切换前后核对文件清单、Git HEAD、dirty/未跟踪内容及前端构建摘要；旧目录保持可恢复。
- 路径文档、示例及源码准备通过不等于跨站运行验收；当前任务不自动提交或重启已有 Job。

## 2026-11-12 本地执行记录

- 固定源码入口已切换至本地 Fleet `bfc5aad` 工作树，包含未提交和未跟踪源码改动及重新构建的前端，共 231 个发布文件、8,337,599 字节，全部 SHA-256 核对一致。未复制虚拟环境、凭证、缓存、output 或草稿目录。
- 旧 `832a8b9` checkout 原样保留为 `/mnt/post_train/RL/user/liangcai/fleet.backup-20261112T054328Z`，包括其原有未跟踪文件。
- 迁移清单位于 `/mnt/post_train/RL/manifests/fleet-nas-paths-20261112T054328Z/migration.json`，同目录保留 Git 状态、差异及旧状态。
- 已创建 fleet-data 的 datasets、models、collections、checkpoints、artifacts、staging 空目录；尚未搬迁旧 NAS/KPFS 大文件。未改 ACL、网络、数据库或现有 Job。
- 切换前核对三地活动 Pod 和管理面 Job/Task 未使用此入口；云 DataTools 的 `/mnt` 挂载不暴露此 NAS 源码。NAS 源码解析、Python 实际导入来源、同路径 Pod 模板和只读输入检查通过；Fleet 前端类型检查/构建及两套文档严格构建通过。
- 跨站只读 Pod、写入/断网、节点重启恢复尚未验收。本地通过不代表这些能力已通过。
