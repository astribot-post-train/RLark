# 金山云裸金属资产

最后核实：2026-11-12。本页记录团队访问入口和已观察到的节点角色，不包含登录密码、私钥或对象存储凭证。

| SSH 入口（用户 `root`） | Kubernetes 节点 | 已观察角色 | Kubernetes 状态 |
| --- | --- | --- | --- |
| `36.103.180.169:2222` | `10-0-2-57` | 控制面节点 | Ready，Kubernetes 1.31.14 |
| `36.103.180.169:2223` | `10-0-2-196` | Worker / GPU 节点 | Ready，Kubernetes 1.31.14 |

SSH 主机和端口映射于 2026-11-12 核实。集群报告的节点地址分别为 `10.0.2.57` 和 `10.0.2.196`。凭证不保存在本仓库；请使用团队批准的凭证管理器。

## rclone CSI 覆盖范围

2026-11-12 现场看到 `rlark-system/csi-driver-rclone-node` 只选择 `kubernetes.io/hostname=10-0-2-196`，DaemonSet 期望和就绪副本均为 1。它已经有通用的 `operator: Exists` 容忍，因此控制面污点不是排除原因；hostname selector 才使 `10-0-2-57` 没有插件。RLark Addon 目录支持可选节点选择器，留空表示不按 hostname 限制。应修改集群中由 RLark 管理的 Addon，并在调谐后核对两台节点上的 Pod 和 CSI 注册状态。不要脱离 Addon 所有权单独管理 DaemonSet。

## Fleet 使用 KS3

Gateway 使用 `https://ks3-cn-ningxia.ksyuncs.com`；已有云端类的 CSI 使用 `https://ks3-cn-ningxia-internal.ksyuncs.com`。本次从 `10-0-2-196` 直连公网 bucket 域名的未认证 TLS 探测 5 秒超时；其他路径收到的 403 不能证明该直连路径可用。新类继承此前已验证的 endpoint 配对，创建时仍需通过带认证的 `HeadBucket`。Fleet 专用类计划使用 bucket `stardust`、对象前缀 `ks3-regular`、虚拟主机寻址和 `vfs-cache-mode=off`；现有类保持不变。生成的 `remotePath` 应为 `stardust/ks3-regular`，对应现有 `ks3://stardust/ks3-regular/` 数据树。

保留 Fleet 启动检查。切换 Run 前，需在两台节点分别用隔离的新 Pod 验证缓存模式和挂载前缀，并测试关闭文件时的错误传播，以及上传中断后的完整文件重传。不要在活动 Run 中切换。

另见[英文资产页](bare-metal-assets.md)和[Station 部署记录](station-deploy/README.zh-CN.md)。

## Fleet KS3 实施快照：2026-11-12

在 Fleet 归档验收通过前，现有声明仍需保留：

| 声明 | 范围和用途 | 状态 |
| --- | --- | --- |
| `ks3-post-train` | 本地和机器人集群；既有共享数据读取 | 保留 |
| `ks3-post-train-ksyun` | 云端集群；只读模型/数据访问和验证 | 保留 |
| `fleet-ks3-ksyun` | 云端集群；`ks3-regular` 归档候选 | 保留用于隔离测试；不要用于活动 Run |

在核对线上 PVC 和 Job 引用前，不要删除两个 `ks3-post-train` 声明。不能因为 Fleet 专用类已经存在就删除云端类，后者仍未通过 KS3 multipart close 测试。

已通过升级后的 Gateway 创建 `fleet-ks3-ksyun` → `sc-801ae48b73084190`，只分发到 `rlark-ksyun-gpu`，配置为 `prefix=ks3-regular`、`remotePath=stardust/ks3-regular`、`vfs-cache-mode=off`，沿用已有公网 Gateway / 云内 CSI endpoint 配对。比对确认旧 `ks3-post-train-ksyun` 声明未变。Gateway 回滚容器为 `rlark-gateway-pre-fleet-storage-20261112`。

验收状态为**待通过**。在 `10-0-2-196` 两次写入 491,520 字节流式文件，close 都返回 EIO，rclone 报告 `corrupted on transfer`。独立签名 KS3 GET 下载内容和 SHA-256 与源文件完全一致，但 ETag 与内容 MD5 不同。这说明本次路径存在 ETag 校验兼容性问题，不代表已证明通用上传正确。不得据此关闭完整性检查或切换 Fleet Run。5 字节对象也已到达 KS3，但大 checkpoint 与中断上传恢复仍未验证。隔离验证对象和资源单独清理，不改已有数据。

当前 Run 的 `/root/fleet/code/fleet/runners/common/archive.py` 中，`Archiver.mounted()` 只接受 `fuse.ks3fs`，其实际部署版本尚未体现所述 rclone 缓存探测。保留启动检查，切换前核实目标 Fleet 版本；本次没有修改 Fleet 源码或 Run 配置。


## 恢复执行与当前验收结果：2026-11-12 09:29 UTC

用户确认另一操作结束后已恢复执行。Cluster Agent 以刚完成仓库迁移的版本为基础，仅替换 Agent 二进制，镜像为 `10.254.240.1:5443/rlark/cloud-agent@sha256:7515240c5d9f24640f7a6f92f052f943a12be3eb8dfe794e49c1dc6852162c3f`。原有参数、环境变量和 workload `--image` 保留。先在 Addon generation 5 应用 `nodeUpdateStrategy=OnDelete` / `nodeImagePullPolicy=IfNotPresent`，确认实际 DaemonSet 策略后，再在 generation 6 清空 `nodeSelector`，并将四个 CSI 镜像改为 KS3 仓库 `10.254.240.1:5443/csi-rclone/*@sha256:...` 的已核对 digest；Controller selector 仍为 `10-0-2-196`。

两台节点的插件均为 3/3 Ready，`CSINode` 均注册 `rclone.csi.veloxpack.io`。196 上原 Pod `csi-driver-rclone-node-cwgw2` 的 UID 仍为 `f896bb7a-9c28-49a6-ba9b-6709eee7937a`，三个容器重启次数均为 0；57 新 Pod 为 `csi-driver-rclone-node-4pwh7`。`OnDelete` 意味着 196 仍运行旧模板中的相同驱动镜像，只有安排维护并显式替换 Pod 后才使用新镜像地址。不要为了让 updated 副本数相等而删除仍提供挂载的 Pod。

隔离 PVC/Pod 在两台节点都成功挂载 `s3:stardust/ks3-regular`（`fuse.rclone`），CSI 请求确认 `vfs-cache-mode=off`。两台分别通过 5 字节文件的写入、close、rename 和本节点 SHA-256 读回。57 也通过挂载读取 196 写入的既有 491,520 字节验证对象，SHA-256 为 `2c2cd22ad2454be571aafde3be545ec44db86330585a860d1af8e274a493e203`；这只证明该既有对象可跨节点读取，不证明失败的 close 可以忽略。491,520 字节写入在两台均因 close EIO 失败，故**新类尚未通过 Fleet 写入验收，不应切换 Run**。

进一步使用独立 S3 API 对同样内容做 singlepart / multipart 对照，均下载核对 SHA-256：singlepart 和 multipart 的分片 ETag 都是内容 MD5 `0302637dadb24a465621d1be04717fb3`，完成 multipart 后对象 ETag 为 `b2931c9fb143d6a49b330182336d1a52`，与 rclone 失败对象相同。CSI v0.2.0 内嵌 rclone v1.71.2；其 S3 后端把 32 位十六进制 ETag 解释为 MD5，而 KS3 该 multipart 路径不满足这一假设。应修复/升级驱动的 KS3 multipart 校验兼容性，并保留真实的内容校验；仅关闭 `use_multipart_etag` 或设置 `disable_checksum` 不能解决最终流式传输比较，不能以关闭校验替代修复。

另有两项验收限制：默认 5 MiB multipart 分片最多支持约 48.8 GiB 的未知长度流，不能覆盖 80 GB checkpoint，需在兼容性修复时配置并验证合适的分片大小；另一节点即时读取新建文件返回 ENOENT，说明各挂载目录缓存尚未刷新，不能承诺跨节点即时可见。完整大文件、中途断网后的 close 错误及完整重传仍未通过；未对共享 CSI、节点网络或真实 Run 做故障注入。修复后须重跑这些测试，并验证目标 Fleet 版本支持 rclone 缓存探测，再在 Run 结束后切换。

本会话没有修改 Fleet 或 DataTools Job。验证期间管理面的 Task 出现删除/重建，旧 Fleet Pod 和两个 DataTools Pod 被控制器回收，因此不能声称所有工作负载 UID 保持不变；现有 CSI Pod 身份已独立核对。重建后的 DataTools Pod 的两个 NAS 目录可 stat，仍为 `fuse.rclone`。本轮只清理本轮唯一验证前缀及具有已核对 Pod/claim UID 的测试 Pod、PVC、Retain PV，不清理业务数据。

恢复记录：升级前 Agent 为 `10.254.240.1:5443/rlark/cloud-agent@sha256:3c8e38c898654ff120ceaf0216cd661d987f0049e213a428ec54a13131d35be1`。若需回滚 Agent，先恢复 Addon 的旧镜像值和 `nodeSelector`，避免旧目录调谐器忽略新参数、将 DaemonSet 自动恢复为 RollingUpdate；这会撤去 57 插件，必须先确认 57 没有活动 CSI 卷。不能直接回滚旧 Agent，也不能在有活动挂载时改回 RollingUpdate。优先保持已工作的节点插件和当前 Addon 配置，仅继续修复隔离测试中的 KS3 兼容性。
