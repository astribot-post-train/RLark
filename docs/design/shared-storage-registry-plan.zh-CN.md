# 共享存储与镜像仓库：设计和运维

## 金山云 Job 访问 KS3：只读排查

2026-11-12 检查：RLark 的 `ks3-post-train` 声明使用 bucket `stardust` 和 endpoint `https://ks3-cn-ningxia.ksyuncs.com`。这是 Gateway 的 endpoint；已有云端类另用 `https://ks3-cn-ningxia-internal.ksyuncs.com` 供 CSI 挂载，此前隔离读取验证了该配对。本次从 `10-0-2-196` 直连公网 bucket 域名仍在 5 秒后超时，因此新类继承现有已验证的 endpoint 配对。此前的 HTTP 403 属于授权响应，不能证明 endpoint 不可达。创建 StorageClass 仍要求凭证通过 Gateway 的 `HeadBucket` 校验。

裸金属节点只读检查确认两台 Kubernetes 节点均为 Ready。rclone CSI Node DaemonSet 当前只选择 `kubernetes.io/hostname=10-0-2-196`，就绪副本为 1。它已经通过 `operator: Exists` 容忍所有污点；使 `10-0-2-57` 被排除的是 hostname selector，而不是缺少控制面容忍。应更新 RLark 管理的 Addon 以移除 Node selector，再核验两台节点上的驱动注册。本次排查没有记录任何已完成的现场 Addon 或 StorageClass 变更。

Fleet 专用类计划作为独立声明创建，使用 bucket `stardust`、prefix `ks3-regular`、虚拟主机寻址和 VFS 缓存模式 `off`。生成的 `remotePath` 应为 `stardust/ks3-regular`，对应 `ks3://stardust/ks3-regular/`；现有类保持不变。关闭缓存后真实挂载上的 close 错误和上传中断重传仍须测试通过，才可切换 Fleet Run。

Gateway 现在为每个生成的 S3 StorageClass 使用稳定且按存储类隔离的 rclone remote 别名（`s3-<storage-class-id>`），并在 CSI 参数和配置段中统一使用该别名，避免同一节点插件上的读类和归档类共享可变 remote 配置。这不能解决 KS3 multipart ETag/CRC64 兼容性故障；CSI 驱动仍需增加面向提供商的完整性修复，并通过真实验收测试。

> 2026-11-12 更新：Fleet 已决定使用 [NAS 固定完整路径](fleet-storage-paths.zh-CN.md)。下文 KS3 个人 Fleet 工作树为历史提案，不再作为 Fleet 新任务约定；KS3 数据及镜像试运行记录保留。

本文记录当前环境的设计、已验证进度和手动操作，不代表 RLark 已正式支持或完成全部部署。进度是检查时的快照，不是持续健康监测。

### 存储类角色和保留策略

在 KS3 multipart 兼容性问题解决前，三个声明应保持独立：

| 声明 | 范围 | 当前用途 | 决定 |
| --- | --- | --- | --- |
| `ks3-post-train` | 本地和机器人集群 | 既有共享 KS3 读取和挂载 | 保留 |
| `ks3-post-train-ksyun` | 仅 `rlark-ksyun-gpu` | 云端只读 Job、模型/数据读取和验证 | 保留 |
| `fleet-ks3-ksyun` | 仅 `rlark-ksyun-gpu`，前缀 `ks3-regular` | 关闭缓存的 Fleet 归档候选 | 保留用于隔离测试；未通过活动 Run 验收 |

两个 `ks3-post-train` 声明不是重复项：前者保留本地/机器人端点和分发范围，带 `-ksyun` 的声明使用云端 CSI endpoint。在核对所有 PVC、Job 和验证引用前，不要删除任意一个。`fleet-ks3-ksyun` 在 close 仍返回 EIO 时不能替代云端只读类。

## 总体架构与设计决策

```text
                              KS3：stardust bucket，宁夏
                         ks3-regular/post_train/（共享数据）
                           /          |           \
                          /           |            \
                    KS3FS 手动     云端访问       KS3FS 手动
                      已验证        未验证           已验证
                       /              |                \
              本地开发机          金山云 A/B          机器人 x86
              RLark 服务 + UI     计算 / Job          工作负载
              （现有本地环境）    （待验证）           （待验证）
                       |
              独立的本地 OCI 镜像仓库（受限试运行，TLS + 认证）
                       |
              KS3 对象存储后端（试运行推送和完整性验证通过）
```

- **代码：**计划将 Cailiang 的 RLark 和 Fleet 开发代码放在个人 KS3 路径；目前尚未迁移，也未验证这种开发方式。初期只允许一个写入者。若编辑器、Git、构建、锁或缓存语义不满足需求，则保留本地工作树，发布带版本的产物。
- **数据与 Job：**通过父挂载共享已有数据集；新共享数据集及每次 Job 的输入、输出分别使用获准的独立前缀。所有输出关闭后再写完成标记；避免多个写入者同时修改同一文件。
- **镜像：**独立的本地 Distribution 仓库已受限试运行，通过 KS3 对象 API 而非 KS3FS 目录存储镜像。本地 Docker 推送、重启以及机器人 Docker 和 Kubernetes 拉取已测试。RLark 已有管理员凭证管理页面，试运行凭证已分发至选定的开发和机器人集群；实际 RLark Job 链路、云端连接、生产安全及恢复仍待验证。现有本地仓库未认证，确认其使用者及安全性前不要接管或修改。
- **数据库：**运行中的 PostgreSQL 数据保存在合适的本地文件系统或 Docker volume，不放在 KS3FS 上。独立 KS3 备份路径、保留策略和恢复验证仅为计划，本次尚未配置。
- **安全：**目录名不是访问控制。需结合云端策略和主机权限；不要把访问密钥写入 Git、日志、清单或截图。TLS、仓库认证、最小权限存储凭证及各客户端连通性均是验收条件。

KS3FS 将对象存储呈现为文件，不应假设具有完整 POSIX 锁、原子目录重命名、缓存一致性或安全的 Git 多写者语义；正式依赖前应验证应用实际行为及故障恢复。父前缀为读写挂载：已有数据集和模型在技术上并非只读。

## 已约定和拟定的路径

开发机和机器人约定将 `/mnt/ks3/post_train/` 映射到 `ks3://stardust/ks3-regular/post_train/`。云端主机验证访问和已有挂载后再尽量使用相同映射。已有数据集直接从父挂载读取，不需要另行挂载。

```text
/mnt/ks3/post_train/
├── datasets/                          已有共享数据集
├── models/                            已有共享模型
└── rlark/                             项目前缀
    ├── .validation/                   隔离的验证对象
    ├── users/cailiang/code/rlark/     拟定的个人 RLark 代码目录
    ├── users/cailiang/code/fleet/     拟定的个人 Fleet 代码目录
    └── shared/
        ├── datasets/                  拟定的新共享数据集目录
        └── jobs/<job-id>/<run-id>/    拟定的单次运行输入和输出目录
```

上面的项目代码及共享 Job/数据集子目录是规划，不代表已经创建或完成代码迁移。试运行仓库使用独立的 `rlark/registry/` 前缀，是否适合生产仍待确认；数据库备份前缀尚未选定或创建。检查并获准之前，不覆盖或迁移已有数据。

## 当前进度快照：2026-11-05

| 范围 | 已观察到的状态 | 尚待完成 |
| --- | --- | --- |
| 本地开发机 | 以 `cailiang` 手动读写挂载 KS3FS 1.3.1，`datasets/` 可访问；验证对象经过卸载、重挂载后仍可读取。 | 开机恢复、挂载缺失时阻止误写、其他用户和容器访问。 |
| 机器人 `x86` | 以 `astribot` 手动读写挂载 KS3FS 1.3.1，`datasets/` 可访问；用户级局部代理提供所需 DNS 和源地址。 | 持久生命周期、网络变化恢复、其他用户和容器访问。 |
| 跨主机 | 两端各写入并关闭唯一验证对象，另一端读取且 SHA-256 一致。对象保留在 `rlark/.validation/`。 | 这不是缓存一致性、并发写、大模型或重启恢复测试。 |
| 云端 A/B | A 直连仓库超时；一次临时 SSH 反向隧道内的认证和 TLS manifest 请求成功。用户确认此前已测 A 的 Lumo2；本次未重测。 | 验证持久、安全的云端访问及实际镜像拉取；分别确认 A/B 存储，B 访问未测。 |
| 代码和 Job | 已提出个人及共享目录布局；本次未迁移代码或验证工作负载。 | 验证 Git/构建行为，协商迁移后运行测试 Job。 |
| 镜像仓库和 RLark | 独立的 TLS/认证试运行仓库使用 KS3；本地推送、重启、机器人 Docker 直连拉取和机器人及 `cailiang` Kubernetes Job 拉取成功。RLark 已将凭证分发到两个选定集群。 | 配置并验证其他节点信任；验证实际 RLark Job、云端连接、最小权限和生产恢复。未修改已有未认证仓库。 |
| 数据库 | 观察到本地 PostgreSQL 使用 Docker volume。 | 设计并验证独立备份和恢复。 |

2026-11-05 已再次检查开发机和机器人的 `findmnt`、数据集访问及机器人代理状态。此前验证仅使用唯一测试对象，未修改已有数据集或模型。挂载状态本身不能证明远端前缀身份或持续可访问。

## 镜像仓库试运行：2026-11-05

- Docker 容器 `rlark-ks3-registry` 使用按 digest 固定的 Distribution 3.1.2，仅绑定 `192.168.31.2:5443`，启用 HTTPS、bcrypt htpasswd 和 `unless-stopped` 重启策略。自签名证书覆盖该 IP，于 2027-11-05 到期；客户端需要专用 CA，变更地址时须同步更新证书和客户端。原有 5555 端口仓库仍保持独立。
- 后端是 `ks3://stardust/ks3-regular/post_train/rlark/registry/`，启用前查询为空。通过 S3 API 访问 KS3，并关闭重定向，镜像下载经过开发机代理。测试镜像和对象仍保留并产生存储费用。仓库容器根文件系统和配置挂载只读，临时 tmpfs 有容量上限；这不能限制 Docker 镜像缓存、上传或 KS3 费用。
- 密钥和配置位于开发机仅所有者可访问的 `~/.config/rlark-registry/`：`config.yml` 包含 KS3 凭证和仓库 HTTP secret，`server.key`、`htpasswd` 和 `client-password` 均为敏感内容。应妥善保护和备份；丢失这些文件或主机后，即使镜像仍在 KS3，仓库也可能不可访问。当前复用了 KS3 凭证，**尚未验证**云端最小权限；轮换与灾难恢复尚未测试。不要打印这些文件或提交到版本库。
- htpasswd 用户 `rlark` 既能推送也能拉取，**不是**机器人只读账号。同一凭证通过 RLark 管理员 API 分发到所选开发和机器人集群的 `rlark-system`；能读取这些 Secret 的人也能推送。如不接受这一信任范围，应撤销或替换凭证。本试运行尚无多用户授权、审计和隔离。
- 已分别为开发机与机器人 Docker、机器人 Kubernetes containerd（`/etc/containerd-robot/certs.d/192.168.31.2:5443/`）和开发机 containerd（`/etc/containerd/certs.d/192.168.31.2:5443/`）安装专用 CA；初次安装没有重启运行时。仓库重启后，机器人 Docker 及显式引用已分发 `imagePullSecrets` 的 Kubernetes Job 拉取成功。本地 Docker 推送成功；开发机 Kubernetes Job 初次仍报 `x509: certificate signed by unknown authority`，后续修复见下文。开发集群其他节点尚未配置，不要未经协调重启繁忙的运行时。
- 本地和机器人的 Kubernetes Job 均显式指定了 `imagePullSecrets`；这验证了凭证分发及机器人 Kubernetes 运行时，**不能**证明当前部署的 RLark Agent 自动注入或 RLark Job 端到端通过。当前源码包含 Kubernetes Task 凭证匹配注入逻辑，部署版本及其他命名空间需另行核实。本地 Gateway 已验证只读目录、标签和摘要浏览，并提供 Push/Pull 命令指南；实际上传和下载仍由 Docker 客户端执行。这是本地试运行，不代表完整发布或重启验收。部署与回滚详见代码仓库中的 `docs/operations/private-registry.zh-CN.md`，云端临时隧道测试及其限制见下文。

### 后续连通性、证书与传输验证：2026-11-05

- 云端 A 通过既有可信主机密钥的 SSH `10001` 登录，观测容器私网地址 `10.1.3.214` 与已有接入记录一致。从 A 直接访问 `192.168.31.2:5443` 在 5 秒内连接超时；不能断言具体网络阻断原因。随后从开发机建立临时、仅监听 A 回环地址的 SSH 反向隧道，A 通过隧道验证服务器证书、使用仓库凭证取得小镜像 manifest，HTTP 200 且摘要与下方小镜像记录一致。证书和凭证仅用于该次进程，隧道已关闭；A 未安装持久信任或凭证，且未发现 Docker、Podman 或 nerdctl 命令。该验证不是云端容器运行时拉取、持久网络方案或 B 的验证，A 的 Raw 运行时不能因此视为支持容器镜像工作负载。
- 开发节点 `cailiang` 的 containerd 2.2.1 处于 transfer 拉取模式，原 CRI registry `config_path` 为 `/etc/containerd/certs.d:/etc/docker/certs.d`。显式凭证的小镜像 Kubernetes Job 曾因 `x509` 失败。隔离的 `ctr` transfer 诊断中，同一个 CA 以单目录 `/etc/containerd/certs.d` 访问时到达认证阶段（未提供凭证，返回未认证），使用原冒号路径则复现 `x509`。[containerd 2.2.1 transfer 源码](https://github.com/containerd/containerd/blob/v2.2.1/core/transfer/registry/registry.go) 将该值作为单个 host 目录处理；在字面组合路径下临时放置已有仓库的 `hosts.toml` 后，实际 CRI 的显式凭证小镜像 Job 成功。临时目录随后移除，不作为长期方案。
- 经用户同意，18:05 左右先通过 RLark 的 `spec.suspended=true` 有序停止 `jo-e8ec7d67138a422c` 和 `s1u-sync-overlap-20261105-1700`，确认 `Stopped`、开发机及机器人对应 Pod 消失、开发机 GPU 计算进程释放；没有自动恢复这些 Job。检查两个证书目录时只发现此试运行仓库的文件。将开发机 `/etc/containerd/config.toml` 的 CRI registry `config_path` 改为 `/etc/containerd/certs.d`，并仅重启开发机 containerd；原配置备份为 `/etc/containerd/config.toml.rlark-registry-8acb9ab8-3f29-4aef-98a7-3c6d33abfbf6.bak`。如需回滚，先核对当前配置、其他仓库依赖及任务影响，在维护窗口协调还原备份和重启，不自动覆盖后续变更。重启后显式 `imagePullSecrets`、`imagePullPolicy: Always` 的小镜像 Job 在 `cailiang` 成功，镜像层可能来自缓存；本地 API `/readyz`、三个节点和机器人节点均正常，Docker、kubelet、仓库和控制面 Pod 保持运行。此结果不证明整机重启恢复或其他节点的信任；`alexfu`、`wendychen` 仍需分别配置和验证。
- 为避免开发机 Docker 和 containerd 共用内容缓存影响结论，另在机器人生成 1 MiB 随机文件并推送唯一镜像 `192.168.31.2:5443/rlark/transfer-validation:robot-cri-03600ca5-ccdf-47e0-891a-50d3c70cf1c6`。开发机 `cailiang` 的显式凭证 Kubernetes Job 成功执行，文件 SHA-256 `0653a709a781116e7da88f10ffc9fc5aea293f9c138f2c1c4630a7e867c56516` 一致；仓库日志记录了该新压缩层的 GET。manifest digest 为 `sha256:0449e2118dab467d99a342a23ca5013506672957403802f4d39100d764ef31b8`。这验证该节点经仓库下载新层，并非直接验证 KS3 对象或其他节点。该镜像和另一个本机构建的 `cri-364b88cb-0417-4575-b68d-1233f25b952b` 测试镜像及缓存保留，会产生少量存储费用；临时登录和构建文件已移除。
- 随后管理 API 中原 Job CR 已不再列出，RLark 任务页面仍显示上述两个 Job 已停止，并提供重启操作；本次没有执行重启。
- 大小受控的单次传输：开发机向 KS3 后端仓库推送唯一随机层镜像 `192.168.31.2:5443/rlark/transfer-validation:large-f6af8d8b-7af0-45ff-a883-bb526811e836`，新层 268,517,572 字节（约 256.08 MiB），推送耗时 98.43 秒，约 2.60 MiB/s。机器人 Docker 首次拉取该新层，基础层命中缓存；整个 pull 含解压耗时 80.38 秒，按新层大小约 3.19 MiB/s。两端随机文件 SHA-256 均为 `96106e5e52baeeb6bf0704eb5bd76a3ac7ae9c4c0edf97f2116bf27e31a19efe`，manifest 摘要为 `sha256:0e0e56d7812d576caff0c6e5de17e54cdee620f069edcca7b2d78b1dd8757dcb`。镜像和 KS3 对象及两端镜像缓存保留，会产生存储费用；临时登录文件和原始构建文件已移除。该次未直接对 KS3 的大层对象做 GET 校验，仅验证配置的后端和通过仓库拉取的结果；单样本也不代表 15.5 GB Fleet 镜像、多层并发、断点恢复或长期可靠性。

试运行验证镜像为 `192.168.31.2:5443/rlark/validation:deploy-b2c57f5c-a807-47f8-868c-d2a740656302`。仓库报告的 manifest digest 为 `sha256:f97baa533a26513c453a362be331a43eb60214302f449c16571b23cea141dce4`；直接从 KS3 GET 并进行 SHA-256 校验，该 manifest 及其约 2.1 MiB 镜像层均一致。机器人认证 Docker 拉取返回相同的 manifest digest。这是小镜像功能验证，不是大镜像性能测试。

在开发机执行 `docker ps --filter name=rlark-ks3-registry` 和 `docker inspect rlark-ks3-registry --format '{{.State.Status}}'` 检查服务；不带认证执行 `curl --noproxy '*' -o /dev/null -w '%{http_code}\n' --cacert "$HOME/.config/rlark-registry/server.crt" https://192.168.31.2:5443/v2/` 应返回 401。仅对该容器使用 `docker start rlark-ks3-registry` 或 `docker restart rlark-ks3-registry`；已验证容器重启，但尚未验证整机重启恢复或 Docker 启动时地址是否就绪。不要为了恢复而直接删除容器、凭证或 KS3 前缀。

从开发机推送时，使用唯一镜像标签和隔离的 Docker 配置；密码不进入命令参数。替换示例镜像名称，并在完成后安全移除临时 Docker 配置：

```bash
cfg=$(mktemp -d)
DOCKER_CONFIG="$cfg" docker login 192.168.31.2:5443 -u rlark --password-stdin < "$HOME/.config/rlark-registry/client-password"
docker tag IMAGE:TAG 192.168.31.2:5443/PROJECT/IMAGE:UNIQUE_TAG
DOCKER_CONFIG="$cfg" docker push 192.168.31.2:5443/PROJECT/IMAGE:UNIQUE_TAG
unlink "$cfg/config.json"
rmdir "$cfg"
```

### 服务镜像快照：2026-11-05

已将当时运行的 RLark Server/Controller Manager、Gateway 和开发集群 Agent 三个 `amd64` 镜像推送至试运行仓库，并通过认证读取 manifest，比对摘要与源镜像一致。这些是运行版本备份，不是当前工作区构建或已部署升级；尚未从仓库重新拉取并启动这些服务。操作手册记录了精确标签和摘要。UI 和机器人当前镜像尚未上传；A 仍无法直连，`alexfu` 和 `wendychen` 的证书信任尚待配置。

## 镜像仓库和 KS3 性能测试：2026-11-05

经授权，早期测试在 `ks3://stardust/ks3-regular/post_train/rlark/registry-test/` 下使用唯一对象。隔离运行 ID 为 `1a330a71-b31e-4f14-9a0f-9b31edde5dce`；仓库数据位于 `registry/<run-id>/`，测速对象位于 `benchmark/<run-id>/`。测试对象保留在 KS3，可能产生存储费用；它与后续试运行前缀分开。

临时仓库使用 Distribution 3.1.2 和受保护配置，通过 KS3 的 S3 API、v4 签名及虚拟主机形式访问后端，镜像下载由仓库代理而不是重定向到 KS3。它仅监听本机回环，没有 TLS 或认证，**不是生产部署**。测试后仅移除了这一临时容器及其匿名本地 volume，并删除临时含凭证配置；原有仓库未修改且仍运行。本地及机器人 Docker 缓存中保留了测试镜像标签。

本地 Docker 推送了小型 BusyBox 镜像和包含 16 MiB 随机内容镜像层的独立镜像。重启测试仓库后，manifest 和镜像层 digest 一致；另用 S3 API 列表及 GET 证实大镜像层确实位于目标 KS3 前缀且 SHA-256 一致。机器人 Docker 通过临时 SSH 反向隧道拉取 16 MiB 镜像，并通过隧道下载镜像层验证 SHA-256；隧道已关闭。这只证明机器人**经隧道**拉取，不证明机器人直连仓库、云端拉取、Kubernetes 集成、认证或生产可用性。另以 S3 API 直接上传一个 16 MiB 对象、分为 3 个分片，完成并校验 SHA-256；这本身不能证明仓库所有分片及重试路径。

兼容性发现：所测 Distribution 2.8.3 对自定义 S3 endpoint 强制使用路径式 bucket 地址，被 KS3 以 `SecondLevelDomainForbidden` 拒绝；Distribution 3.1.2 在本次测试中使用虚拟主机式地址成功。3.x 的默认配置路径与 2.x 不同：初次运行 3.x 时误用了默认本地文件系统后端，该次推拉结果已丢弃；上述结果均来自修正 `/etc/distribution/config.yml` 后、独立检查过 KS3 对象的测试。有一次 S3 列表返回 HTTP 429，稍后列表成功，其原因及持续运行情况尚未确认。

### 单流速度实测

每项使用一个 64 MiB 随机内容对象、单次测量及表中所示主机；S3 API GET 和 KS3FS 读取包含 SHA-256 校验。写入时间包含 S3 PUT 完成，或 KS3FS 的 `fsync` 和关闭文件。本地 KS3FS 首次读取及机器人读取使用新对象，但无法完全排除缓存和其他流量影响。本地 KS3FS 写入发生在首轮，可能与仓库镜像下载重叠；直接 API 数值是该下载结束后的复测。这些是当时的测量值，不代表容量、并发性能或 SLA。

| 主机和方式 | 写入 | 读取 |
| --- | --- | --- |
| 开发机，直接 S3 API | 3.11 MiB/s | 3.34 MiB/s |
| 开发机，KS3FS | 2.17 MiB/s | 3.21 MiB/s |
| 机器人，KS3FS 经局部代理 | 2.93 MiB/s | 2.81 MiB/s |

两台主机通过 KS3FS 写入的对象均由直接 S3 API 读取并通过 SHA-256 校验。尚未执行清缓存、并发、大模型、多轮稳定性或云端测速；正式设计大镜像分发前应在目标生产链路使用代表性数据复测并记录费用。

## 挂载前提与安全边界

- 以下脚本及安装均为现有**机器本地**文件，不是仓库内交付的安装脚本。开发机的 KS3FS 和 FUSE 2 库位于 `~/.local/opt/rlark-ks3fs/1.3.1/`；机器人在相同基础目录下解压 Ubuntu 20.04 的 KS3FS 1.3.1，使用系统 FUSE 2 库。未检查系统和库兼容性前不能互换二进制。
- 挂载用户需要 `/dev/fuse`、合适的 FUSE 权限、网络，以及由该用户拥有、权限为 `600` 的 `~/.config/rlark-ks3/passwd`。机器人经授权使用开发机凭证的受保护副本；将凭证格式化为指定 bucket **不会**缩小云端权限，应通过云端策略限制，并在轮换或撤销时同步处理两份副本。不要打印文件内容或将密钥放进命令参数。
- 脚本会拒绝已有挂载或非空底层目录，并检查挂载类型和数据集访问，但不会证明远端前缀身份；挂载后检查失败时会保留挂载供排查。不要覆盖已有本地文件。
- **两端均未配置开机自动挂载或恢复。**挂载缺失时底层本地目录仍可能可写，应用可能误写到本地；每次使用前检查挂载和预期数据，在故障保护与恢复验证完成前不要用于无人值守任务。其他用户、容器和 Kubernetes 工作负载尚未验证。

## 本地开发机操作

以 `cailiang` 普通用户操作，不使用 root。先检查挂载和数据集访问：

```bash
findmnt -M /mnt/ks3/post_train -o TARGET,SOURCE,FSTYPE,OPTIONS
timeout 15 test -d /mnt/ks3/post_train/datasets && echo 'datasets accessible'
```

开机后或主动卸载后，确认目标未挂载且底层目录为空，再运行：

```bash
~/.local/bin/ks3-post-train-mount.sh
```

脚本默认挂载到 `/mnt/ks3/post_train`；可用 `RLARK_KS3_MOUNTPOINT` 覆盖。所有使用者停止访问后，可用 `fusermount3 -u /mnt/ks3/post_train` 卸载。不要仅为测试本文操作而卸载运行中的工作负载。现有本地 `~/.local/bin/ks3-post-train-mount-test.sh` 可做脚本模拟检查，但不能替代存储端到端测试。

## 机器人操作与网络

在 `x86` 上以 `astribot` 普通用户操作，不使用 root：

```bash
findmnt -M /mnt/ks3/post_train -o TARGET,SOURCE,FSTYPE,OPTIONS
systemctl --user status rlark-ks3-proxy.service
timeout 15 test -d /mnt/ks3/post_train/datasets && echo 'datasets accessible'
```

开机后或主动卸载后，确认目标未挂载且底层目录为空，再运行：

```bash
~/.local/bin/ks3-post-train-mount-robot.sh
```

脚本会先启动用户级代理。所有使用者停止访问后，可用 `fusermount -u /mnt/ks3/post_train` 卸载；挂载依赖代理时不要停止代理。代理未启用开机启动，且用户 lingering 已关闭，用户会话结束时服务可能停止；目前没有自动重挂载或健康监控。

机器人的默认 DNS 和源路由不能稳定访问宁夏 KS3。局部代理监听 `127.0.0.1:23793`，仅允许 CONNECT 到 `ks3-cn-ningxia.ksyuncs.com:443` 和 `stardust.ks3-cn-ningxia.ksyuncs.com:443`；它以 `192.168.31.250` 为源地址向路由器 `192.168.31.1` 查询 DNS，并以相同源地址建立外连。KS3FS 仍进行端到端 TLS 验证。其他本机用户也能连接此回环代理；域名和端口白名单不等于本机用户认证。没有修改全局 DNS 或路由；地址或路由改变可能导致此环境专用方案失效，不要硬编码解析得到的 KS3 IP。

## 故障排查

1. 若 `findmnt` 看不到挂载，先停止使用者再检查底层目录，不要假定目录中的文件已经写入 KS3。保留意外出现的本地文件；脚本会拒绝隐藏它们。
2. 若挂载存在但 `datasets/` 访问失败或超时，检查网络、凭证有效性和权限。机器人还需检查代理用户服务，确认路由器和源地址仍适用。匿名 HTTP 403 只能说明 HTTPS 通路可达，不能证明认证或写入成功。
3. 若脚本提示已有挂载，应检查现有挂载，不要再次挂载。若提示目录非空或凭证权限不安全，应排查原因，不删除数据也不暴露密钥。收集诊断信息时不要打印凭证。
4. 若挂载失效或挂载后检查失败，先停止使用者并排查，再决定是否卸载或重试。脚本不保证自动恢复，重挂载成功也不代表容器访问或整个前缀已经验证。

## 后续执行与验收

1. **权限与恢复：**确认获准前缀、策略、配额和费用责任；在每台主机验证最小权限、缓存上限、重命名与锁限制、断网、重挂载及故障时阻止误写。无人值守前设计开机生命周期和告警。
2. **云端：**不改变现有工作负载，先检查 A/B 已有挂载，再用隔离对象验证选定云端映射及双向可见性；不假设 B 已有可用资源。
3. **代码与数据：**保留现有工作树和未提交修改，在隔离目录测试编辑器、Git、构建和跨主机访问；迁移 RLark 或 Fleet 前协商迁移和回滚。语义确认前只允许一个写入者；不满足时使用版本化产物。
4. **镜像仓库：**完善最小权限凭证、账号分离、TLS 续期、本地磁盘限制、监控和备份恢复。用于生产前，验证整机重启、故障中断、大镜像，以及每个目标客户端的网络和运行时信任。
5. **RLark 与工作负载：**验证已部署 Agent 的自动凭证匹配和实际 RLark Job；为其余目标节点配置信任。验证云端拉取及容器访问隔离运行目录。不能假定尚不完整的 Raw runtime 能运行 OCI 镜像。
6. **数据库与端到端：**单独设计 PostgreSQL 备份并验证恢复；联合验证代码版本、数据集、运行输出和镜像，覆盖存储或网络故障以及重启恢复，记录结果和剩余限制。

修改机器人网络、重启工作负载、创建计费服务或迁移数据之前，应先确认目标及影响。不要将密钥写入文档或版本库。

## 金山云 KS3 挂载验证：2026-11-12

已实现可选 `mount_endpoint`：Gateway 使用现有公网 endpoint，CSI 使用云内域名，无 DNS 或固定 IP 修改。独立声明 `ks3-post-train-ksyun` 已仅分发到 `rlark-ksyun-gpu`，实际 StorageClass 为 `sc-1ff57f5f06154dfd`；原 `ks3-post-train` 仍只面向本地和机器人。

两个短时 RLark Job 在 `10-0-2-196` 使用 Pod 级临时 PVC、只读 `/mnt/ks3` 挂载，读取已有 92 字节验证对象，其 SHA-256 与公网 API 读取结果一致。未写入 KS3。首次文件访问出现 I/O 错误，CSI 日志显示 KS3 ListObjects HTTP 429（HTML 响应），后续重试恢复；第二个新 Job 也经历失败后恢复。成功绑定 PVC 或 Pod Running 不足以证明文件可读，测试必须检查读取退出码和内容校验；应用需处理有界重试。尚未定位 429 根因，也未验证大文件、并发、跨节点、长期可靠性或写入。

现场 Gateway 已切换至 `rlark-gateway:ks3-mount-endpoint-20261112`，旧容器 `rlark-gateway-pre-ks3-20261112` 停止保留。配置快照位于开发机受保护的 `~/.local/share/rlark-runtime/gateway-before-ks3-20261112.json`。Gateway 鉴权和新字段读取已验证；UI 开发服务从当前工作区加载新表单。回滚到不支持新字段的 Gateway 前，应先停止使用新声明的 Job；旧版编辑保存不能保留覆盖字段，不应使用它编辑该声明。两个测试 Job、Task、Pod、PVC 及对应 Retain PV 已核对清理完成。首个测试卷卸载曾报告忙，确认测试容器已结束后，仅对该测试挂载完成卸载清理；共享 CSI 未重启，现有 NAS 工作负载保持运行。凭证仅存于受保护的管理面声明及下发 Secret，不加入本文。

使用时在 Job 中选择金山云集群和 `ks3-post-train-ksyun`，容器挂载路径按应用配置。当前验证仅覆盖 `10-0-2-196`，不要将使用该卷的任务调度到未安装 CSI 的节点。

## Fleet KS3 实施快照：2026-11-12

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
