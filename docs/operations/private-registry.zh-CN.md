# 私有镜像仓库操作手册（试运行）

2026-11-12，机器人 worker `s1u-02-x86` 使用源地址 `192.168.31.251` 的持久主机路由访问 `10.11.14.128:5443`。其专用 containerd 已配置现有仓库 CA，并完成经 TLS 校验和认证的 `busybox:latest` 拉取。该结果只验证当时该节点和镜像，不代表自动凭证分发、其他仓库或跨节点 Pod 网络已验证。


更新时间：2026-11-12（拓扑一节）；其余章节最后更新于 2026-11-05。此手册记录当时环境的操作，不代表生产验收完成。设计、历史测试和待办见 [共享存储与镜像仓库计划](../design/shared-storage-registry-plan.zh-CN.md)。

## 当前仓库拓扑：2026-11-12

本节与下方地址表不一致时以本节为准。后文的 `192.168.31.2:5443` 地址和 `rlark-ks3-registry` 容器均为历史记录。

| 入口 | 服务方 | 存储 | 使用方 |
| --- | --- | --- | --- |
| `10.11.14.128:5443`（HTTPS，`rlark` 账号） | 开发机上的 `rlark-ks3-registry-migrated` 容器 | KS3 `stardust/ks3-regular/post_train/rlark/registry` | 开发机推送；RLark 凭证 `ir-e9a22bbb4e156a1b`（Local registry 10.11.14.128） |
| `10.254.240.1:5443`（WireGuard，HTTP，无认证） | 开发机上的 `rlark-ks3-registry-wg` 容器 | 同一 KS3 路径 | 金山云工作负载使用的镜像名；云节点优先经本机镜像站解析 |
| 各云节点 `127.0.0.1:5444`（只读） | `rlark-registry/rlark-registry-mirror` DaemonSet | 同一 KS3 路径，KS3 内网 endpoint | containerd `certs.d` 为 `10.11.14.128:5443` 和 `10.254.240.1:5443` 配置的镜像站；失败时经 WireGuard 回退到 `10.254.240.1:5443` |
| `10.254.240.2:5443`（HTTPS，`rlark` 账号，可写） | `10-0-2-57` 上的 `rlark-registry/rlark-registry-cloud` Deployment，`hostNetwork`，仅监听 WireGuard 地址 | 同一 KS3 路径，KS3 内网 endpoint | 金山云侧推送；RLark 凭证 `ir-667c8af90d294c2b`（金山云 KS3，仅保存） |
| `localhost:5555`、`10.11.14.128:5555`、`10.254.240.1:5555` | 开发机上的 `local-registry` 容器 | 本机 Docker volume，**不是** KS3 | 本地开发集群和机器人集群 |

**分工规则。** 本地开发集群和机器人集群从 `local-registry`（`:5555`）拉取。金山云集群从 KS3 拉取：镜像地址写 `10.254.240.1:5443/<repo>@sha256:<digest>`，节点会经本机 KS3 镜像站读取。云上工作负载引用的镜像必须先进入 KS3：在云上推到 `10.254.240.2:5443`，在开发机推到 `10.11.14.128:5443`，或用 `skopeo copy --all --preserve-digests` 从 `:5555` 复制。所有 KS3 入口共用同一存储路径，从任一入口推送的镜像在其他入口都可见。

**命名。** 使用带命名空间的仓库名：`rlark/agent`、`rlark/cloud-agent`、`rlark/gateway`、`rlark/controller-manager`、`rlark/station-*`、`data-tools/*`、`fleet/*`。推送时同时打时间前缀标签 `YYYYMMDD-HHMM-<别名>` 和普通 `<别名>`，部署时按 manifest 摘要固定。

**迁移记录（2026-11-12）。** `local-registry` 中的 35 个标签和仅存在于开发机 Docker 缓存中的 19 个镜像，已按上述名称复制到 KS3，复制后逐个校验摘要；KS3 上已存在且指向运行中 `c5b1b05f…` 的 `rlark/station-agent:wg-observation-20261111` 保留原值，未被覆盖。每个副本另有 `20261112-1541-<标签>` 别名。批量复制后才推到 `:5555` 的三个镜像（`rlark-cloud-agent:fleet-storage-20261112`、`station-controller:enroll-20261112`、`station-services:enroll-20261112`）单独补复制。云端 `rlark-agent`（`rlark/cloud-agent@sha256:3c8e38c8…`，`--image=rlark/agent@sha256:82c2d7b8…`）、`rlark-agent-node` 和两个 data_tools Job 已改为引用 `10.254.240.1:5443`。本地开发集群和机器人集群按约定未改，`local-registry` 继续运行。云上的 `kube-system/rlark-nvidia-device-plugin` 使用各云节点自己的 `localhost:5555`，与此无关。回滚云上某个工作负载时，把镜像改回 `10.254.240.1:5555/...` 即可；修改前的清单已备份在 Git 之外。

**云上仓库服务。** 部署描述见 [ksyun-registry.yaml](ksyun-registry.yaml)，Secret 单独创建。TLS 证书（`CN=rlark-registry-ksyun`，`IP:10.254.240.2`，有效期至 2027-11-12）由与开发机仓库相同的 `rlark-registry-local` CA 签发，因此 Gateway 现有的 `SSL_CERT_FILE` 信任和 `--registry-browse-allowed-hosts` CIDR 已覆盖它。它复用 `rlark` 的 htpasswd，从 Secret `rlark-registry-mirror-s3` 读取 KS3 密钥，关闭后端重定向，并**关闭删除**，因为所有入口共用同一存储。已验证：未认证返回 `401`、认证后可读目录和 manifest、可开启并取消上传会话，以及 Gateway 浏览和构建时间读取。未验证：从云主机完整推送镜像、节点重启后的恢复，以及在 Pod 中直接以 `10.254.240.2:5443` 作为镜像地址（节点尚未信任其 CA，也没有对应拉取凭证；请改用 `10.254.240.1:5443` 名称）。只要还有可写入口，就不要运行垃圾回收。

## 仓库与权限

| 项目 | 当前值 |
| --- | --- |
| 仓库地址 | `192.168.31.2:5443`，仅绑定开发机该内网地址 |
| 后端 | `ks3://stardust/ks3-regular/post_train/rlark/registry/`，由仓库通过 S3 API 访问 |
| 用户名 | `rlark` |
| 密码 | 在开发机 `cailiang` 账号的 `~/.config/rlark-registry/client-password`，文件权限为 `600` |
| TLS | 自签名证书 `~/.config/rlark-registry/server.crt`；到期时间 2027-11-05，客户端需单独信任 |
| RLark 凭证 | `RLark KS3 Registry (pilot)`，已选择开发及机器人集群；分发目标为 `rlark-system` |

需要人工查看密码时，在开发机的私密终端执行 `cat ~/.config/rlark-registry/client-password`。不要将密码发到聊天、截图、Git 或放入命令行参数。下述开发机登录命令直接从文件读取，不需要显示密码。

**重要：当前唯一账号既能拉取也能推送。** 能读取已分发 Kubernetes Secret 的人也能推送；目前没有只读账号、仓库级授权或完善审计。不要向不可信的用户或机器发放凭证。客户端只需要仓库账号及公开证书，不需要 KS3 凭证或服务器私钥。仓库下载由开发机代理，开发机关机、网络不可达或 KS3 故障都会影响拉取。

## 哪些镜像可以上传

RLark 服务和 UI、Fleet/rollout、机器人运行环境等 Docker/OCI 镜像都可以作为不同仓库名上传；上传本身不代表这些镜像已验证能在所有目标节点运行。先确认镜像来源、授权、架构、驱动和依赖，检查镜像里没有密钥、训练数据、模型 checkpoint 等不应发布的内容。当前开发集群和机器人 `x86` 报告的架构为 `amd64`；其他机器人架构需单独构建或提供经过验证的多架构镜像。不要假定 Docker 的本地单平台 tag/push 保留原来的多架构索引。

建议按用途区分仓库名，使用构建版本、Git revision 和架构生成**新标签**，不要覆盖正在使用的标签：

```text
192.168.31.2:5443/rlark/control-plane:<version>-amd64
192.168.31.2:5443/rlark/ui:<version>-amd64
192.168.31.2:5443/fleet/rollout:<version>-amd64
192.168.31.2:5443/robot/runtime:<version>-amd64
```

这些名称是约定示例，不表示镜像已经上传。推送前确认空间、网络和 KS3 费用；本机已有 Fleet GPU 镜像，Docker 列表显示约 15.5 GB，实际传输量取决于压缩、层复用等情况。一次约 256 MiB 随机层推送和机器人拉取成功，不代表 15.5 GB 镜像的耗时、可靠性或可恢复性；详见[设计与进度记录](../design/shared-storage-registry-plan.zh-CN.md)。仓库后端放在 KS3 不等于节点不占磁盘：构建、Docker/containerd 缓存和解压仍使用本地空间。

## 已上传的服务快照：2026-11-05

以下是当时运行版本的备份，不代表当前工作区源码构建或已完成升级；已通过认证读取仓库 manifest 并比对摘要，尚未验证从仓库重新拉取并启动业务服务。使用时建议固定 manifest digest，勿覆盖原标签。

| 用途 | 仓库镜像 | Manifest digest |
| --- | --- | --- |
| Server、Controller Manager | `192.168.31.2:5443/rlark/control-plane:snapshot-20261105-0a32522f-f2fa86cd9016-amd64` | `sha256:f2fa86cd9016788b1dccb48fc9315585abecbed4390a8bfa3046e5d55513804e` |
| Gateway | `192.168.31.2:5443/rlark/gateway:snapshot-20261105-0a32522f-7a9216e3e37b-amd64` | `sha256:7a9216e3e37b39d64a141cc08e5e69cdda715bea38e154abc041fe6fed3cd570` |
| 开发集群 Agent | `192.168.31.2:5443/rlark/agent:snapshot-20261105-0a32522f-cb1fcfb7c0a4-amd64` | `sha256:cb1fcfb7c0a467db0a4490aa92b79754829788aa280a369c1b291a071cf65624` |

这些快照不会自动更新正在运行的工作负载。UI 尚未上传；机器人当前使用的独立镜像也尚未上传。云端 A 无法直连仓库，`alexfu` 和 `wendychen` 尚未配置容器运行时信任。

## 开发机：登录、推送、拉取

以下操作在开发机执行，要求 Docker 可用，且已安装该仓库的专用 CA。先查看本地候选镜像及架构，确认使用正确的机器和版本：

```bash
docker image ls
docker image inspect fleet-runtime-gpu:dev --format 'os={{.Os}} arch={{.Architecture}}'
```

此处的 `fleet-runtime-gpu:dev` 只是已观察到的本地镜像示例，不保证它就是需要发布的 rollout 版本。要检查的镜像必须存在于**执行 Docker 命令的那台机器的 daemon**；不要因为镜像名相同就假设机器人和开发机上的内容相同。

下面完整示例使用隔离的临时 Docker 凭证目录；替换 `SOURCE_IMAGE:TAG` 与 `UNIQUE_TAG` 后，在同一 shell 中执行。推送完成时 Docker 会显示 manifest digest，记录该 digest 和来源版本用于部署或回滚。不要在命令出错后忽略失败继续发布。

```bash
(
  set -eu
  cfg=$(mktemp -d)
  trap 'rm -f -- "$cfg/config.json"; rmdir -- "$cfg"' EXIT
  export DOCKER_CONFIG="$cfg"
  docker login 192.168.31.2:5443 -u rlark --password-stdin \
    < "$HOME/.config/rlark-registry/client-password"
  docker tag SOURCE_IMAGE:TAG 192.168.31.2:5443/fleet/rollout:UNIQUE_TAG
  docker push 192.168.31.2:5443/fleet/rollout:UNIQUE_TAG
  docker pull 192.168.31.2:5443/fleet/rollout:UNIQUE_TAG
  docker image inspect 192.168.31.2:5443/fleet/rollout:UNIQUE_TAG \
    --format 'os={{.Os}} arch={{.Architecture}} digests={{json .RepoDigests}}'
)
```

Docker 在临时配置中保存登录信息；正常退出时示例会清理，异常强制终止后需检查并安全删除残留的临时目录。对于已经验证的 BusyBox 小镜像，可以将 pull 地址换为 `192.168.31.2:5443/rlark/validation:deploy-b2c57f5c-a807-47f8-868c-d2a740656302`。不要把它当作 rollout 镜像。

RLark 源码的构建目标也可先构建到本地再按上例发布，例如在仓库根目录执行 `make docker-build-rlark` 或 `make docker-build-rlark-ui`；默认标签和 buildx 平台以对应 Makefile 为准，构建需要其基础镜像和依赖可用。构建不会自动切换正在运行的 RLark 服务。

## 机器人和其他客户端

机器人 `x86` 的 Docker 及其 Kubernetes 专用 containerd 已安装本仓库 CA；机器人 Docker 守护进程需要 `sudo`。可以在机器人私密终端手动登录，密码输入不会回显；若要从开发机查看密码，使用上面的安全方式。以下使用临时配置，并以已验证的小镜像为例：

```bash
(
  set -eu
  cfg=$(mktemp -d)
  trap 'rm -f -- "$cfg/config.json"; rmdir -- "$cfg"' EXIT
  export DOCKER_CONFIG="$cfg"
  docker login 192.168.31.2:5443 -u rlark
  sudo env DOCKER_CONFIG="$cfg" docker pull \
    192.168.31.2:5443/rlark/validation:deploy-b2c57f5c-a807-47f8-868c-d2a740656302
)
```

如果镜像只存在于机器人 Docker，应在该机器人检查后 `sudo docker tag` 并使用相同的临时登录配置执行 `sudo env DOCKER_CONFIG="$cfg" docker push ...`；先确认上传内容和费用。上例的 Docker CA 不等于所有 Kubernetes 节点的 containerd CA。

新客户端必须先确认能访问 `192.168.31.2:5443`，再由管理员通过可信渠道提供该仓库的 CA、配置对应容器运行时信任，并验证 TLS；不要使用 `insecure-registries` 或关闭证书校验作为长期方案。云端 A 直连超时，一次临时 SSH 反向隧道内的 TLS 和认证 manifest 请求成功；这不是持久接入或容器运行时拉取验证，隧道已关闭，B 尚未测试。若部署到其他节点，还需逐节点验证。

## 在 RLark 和 Kubernetes 使用

已部署的 RLark 管理后台**镜像仓库凭证**页保存并分发本仓库的凭证；它不构建或上传镜像。Job 中应填写完整镜像引用，例如 `192.168.31.2:5443/fleet/rollout:UNIQUE_TAG`；不同机器上的 `localhost` 不是同一个仓库。推荐验证成功后使用推送输出的 manifest digest 固定版本，例如 `192.168.31.2:5443/fleet/rollout@sha256:<digest>`。

2026-11-05 本地 `rlark-gateway` 容器已切换到本机构建的 `rlark-gateway:registry-browse-20261105`（镜像 ID `sha256:595ea08e40c0c06e3ac5dbdb74233ac391603c8b6b37ded5b042e0f2330aba2b`）。该镜像基于切换前的 Gateway 镜像和包含未提交修改的工作树构建，尚未推送到仓库，也不是可复现的正式发布版本。它保留原数据库、Kubernetes、container-runtime 配置及端口，重启策略为 `unless-stopped`；浏览白名单仅允许 `192.168.31.2:5443`。仓库 CA 在镜像内，证书变更时需更新镜像；不要关闭 TLS 校验。

通过 `http://127.0.0.1:5173` 的已认证只读检查，仓库目录、标签、manifest 摘要及用户、集群、container-runtime 集群、Job 查询均成功。这只是本地试运行，不代表完整回归、任务提交或重启/开机恢复验收；Vite UI 自身仍是开发进程，不是持久化部署。需要回滚时应先协调维护窗口，将当前容器保留为未占用的其他名称，再将已停止的 `rlark-gateway-rollback-20261105-registry` 恢复为 `rlark-gateway` 并启动，最后验证服务。回滚后的旧 Gateway 不含浏览接口；在验证恢复之前不要删除回滚容器或旧镜像。

机器人 Kubernetes Job 已使用显式的 `imagePullSecrets` 拉取测试镜像；测试凭证名为 `ir-a818414375f542f8`，且只对分发到的命名空间有效。RLark 当前源码包含按镜像前缀匹配凭证的逻辑，但**当前部署版本的自动注入及 RLark Job 端到端尚未验证**，需要检查最终 Pod。开发机 `cailiang` 已将 containerd CRI 信任路径改为单目录并重启；显式凭证的 Kubernetes Job 成功拉取机器人新构建的 1 MiB 随机镜像层并校验内容。开发集群其他节点及云端运行时尚未完成信任验证。原配置备份及维护记录见[设计与进度记录](../design/shared-storage-registry-plan.zh-CN.md)；不要未经协调重启繁忙的 containerd 或替换线上工作负载。

## 排查与恢复

```bash
docker ps --filter name=rlark-ks3-registry
docker inspect rlark-ks3-registry --format '{{.State.Status}}'
curl --noproxy '*' -sS -o /dev/null -w '%{http_code}\n' \
  --cacert "$HOME/.config/rlark-registry/server.crt" https://192.168.31.2:5443/v2/
```

- 未认证请求返回 `401` 是预期的；登录失败需区分账号错误、权限和网络问题，不要把密码放进诊断输出。
- `x509: certificate signed by unknown authority`：检查**实际拉取节点的实际运行时**及其 CA；Docker 成功不代表 Kubernetes containerd 已信任。证书到期或地址变化也需要更新证书和客户端配置。
- `connection refused` / 超时：检查仓库容器、开发机地址、客户端路由和防火墙；仓库仅绑定该内网地址，云端 A 直连曾超时，临时隧道不代表持久可达。
- 推送或拉取慢、失败：检查 KS3 连接、容量及费用；不要反复并发推送大镜像，也不要直接删除对象或执行垃圾回收。镜像层和 Docker/containerd 缓存可能占用本地磁盘。
- 必须重启时仅重启独立服务 `docker restart rlark-ks3-registry`；已验证容器重启，但整机重启后的地址就绪和恢复仍未验证。不要删除容器配置、证书、凭证或 KS3 数据作为恢复捷径。

更多原理、现有试验和安全限制见[设计与进度记录](../design/shared-storage-registry-plan.zh-CN.md)。
