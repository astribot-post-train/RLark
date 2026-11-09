# 私有镜像仓库操作手册（试运行）

更新时间：2026-11-05。此手册记录当前环境的操作，不代表生产验收完成；不加入 MkDocs 导航。设计、历史测试和待办见 [共享存储与镜像仓库计划](../../apps/rlark/docs/zh/developer-guide/shared-storage-registry-plan.md)。

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

这些名称是约定示例，不表示镜像已经上传。推送前确认空间、网络和 KS3 费用；本机已有 Fleet GPU 镜像，Docker 列表显示约 15.5 GB，实际传输量取决于压缩、层复用等情况。一次约 256 MiB 随机层推送和机器人拉取成功，不代表 15.5 GB 镜像的耗时、可靠性或可恢复性；详见[设计与进度记录](../../apps/rlark/docs/zh/developer-guide/shared-storage-registry-plan.md)。仓库后端放在 KS3 不等于节点不占磁盘：构建、Docker/containerd 缓存和解压仍使用本地空间。

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

机器人 Kubernetes Job 已使用显式的 `imagePullSecrets` 拉取测试镜像；测试凭证名为 `ir-a818414375f542f8`，且只对分发到的命名空间有效。RLark 当前源码包含按镜像前缀匹配凭证的逻辑，但**当前部署版本的自动注入及 RLark Job 端到端尚未验证**，需要检查最终 Pod。开发机 `cailiang` 已将 containerd CRI 信任路径改为单目录并重启；显式凭证的 Kubernetes Job 成功拉取机器人新构建的 1 MiB 随机镜像层并校验内容。开发集群其他节点及云端运行时尚未完成信任验证。原配置备份及维护记录见[设计与进度记录](../../apps/rlark/docs/zh/developer-guide/shared-storage-registry-plan.md)；不要未经协调重启繁忙的 containerd 或替换线上工作负载。

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

更多原理、现有试验和安全限制见[设计与进度记录](../../apps/rlark/docs/zh/developer-guide/shared-storage-registry-plan.md)。
