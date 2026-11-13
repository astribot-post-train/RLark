# Station 网关引导组件（试验性）

> **名字（2026-11-12）**：本目录的对象在界面和对外接口里叫**场站节点（Site Node）**：一台机器在场站里的长期身份和入网记录。"Station"留给 Fleet 一个 Run 里的 station（论文的定义）。CRD 组 `station.rlark.io`、kind `Station*` 和可执行文件名是沿用的代号，迁移方案和入网设计见 [Station v2](../../docs/design/station-v2.zh-CN.md)。

这个独立组件通过 Kubernetes DaemonSet 引导一个 WireGuard 站点接口，不是 Station 控制器、不提供 RLark 界面和独立 Peer 功能。使用前必须核对拓扑和网段；尚未在生产节点验证。不依赖 systemd 或 Ansible。

在本目录构建 WireGuard 镜像，并由 `extensions/station/deploy/station-agent.yaml` 的网络模板部署。节点通过 `station.rlark.io/network=enabled` 标签选择网络模板，配置由 `station-wireguard-config` 提供，私钥位于节点统一目录 `/var/lib/station/wg/private.key`。通过独立于此隧道的 Kubernetes API 安装。确认宿主机内核支持 WireGuard；转发流量时另行配置并验证 IP 转发、防火墙和回程路由，本组件不会修改宿主机全局转发或防火墙。

首次启动时各节点在 `/var/lib/station/wg/private.key` 生成权限为 0600 的私钥，Pod 日志显示公钥。先分别用空 peers 启动，再通过受信任渠道交换并核验公钥，然后更新双方 ConfigMap；不要复制私钥。限制宿主机、Kubernetes 管理权限、ConfigMap 写权限和私钥目录访问。各节点必须使用自己的 hostPath 和密钥，ConfigMap 不保存私钥。

由节点操作管理的条目可在顶层包含 `nodeUID`、`publicKey`，并在 peer 项中包含 `nodeUID`。这些是非敏感归属元数据；网关会校验但不将其作为认证凭证。自动剔除只会删除 UID 与当前 Kubernetes Node 实例匹配，或公钥/地址与 Node UID 绑定的最新 Station 观测完全匹配的配置。旧条目无法验证归属时保持不动，并阻止自动剔除。

出站端点需要指定源地址时，可用 `uplinkRoutes` 声明 IPv4 主机路由，例如 `{"destination":"10.11.14.128/32","gateway":"192.168.0.11","source":"192.168.31.250","interface":"enp2s0"}`。源地址必须已属于该接口。仅当不存在完全相同目标的路由时添加；已有路由须与保留协议号、metric 和配置完全一致，否则报冲突。配置更改或 Pod 停止时均不会删除上联路由，须通过独立管理通道人工清理；中途失败可能保留已添加路由。使用前必须验证选源和回程。

程序每 10 秒协调一次，并检测 ConfigMap 的变化；对于没有 `rlark-station-gateway/v1` 别名的已有接口会拒绝接管。它管理该接口的 WireGuard peers，并为路由使用私有协议号 242 和 metric 49321。这两个标记不足以证明路由归属，因此程序不会删除任何路由；从 ConfigMap 移除路由前，管理员须通过独立管理通道手动删除对应路由，否则协调会在修改 Peer 前停止。参与节点应预留这两个值并检查冲突。程序不安装默认路由；路由冲突时报错，不覆盖现有路由。接口已有不同 IPv4 地址时，协调会在更新 Peer 或上联路由之前拒绝变更，不会删除地址。迁移地址须使用新接口及独立验证的管理路径。当前四个网络节点已部署包含该检查的私有仓库 WireGuard 镜像；其他安装须重新构建镜像并使用双容器 Station Pod 模板。不要让其他网络管理器同时操作同一接口。无效配置会记录错误并重试，保留上次成功的内核状态；但执行中途失败可能造成部分更新。当前没有自动回滚或连通性验证。必须保留独立 SSH/Kubernetes 管理通道，必要时恢复上一个 ConfigMap；节点、K8s API、镜像或 ConfigMap 不可用时不保证能自动恢复。退出或卸载不会删除接口。如果程序在接口创建后、写入别名前崩溃，会拒绝接管未标记接口；管理员须经独立通道确认并删除该接口后再重试。

依次检查 Pod 日志、`wg show`、明确的路由、双向握手、业务流量和 MTU；在非生产环境测试 Pod 和节点重启、无效配置、路由冲突、回程和管理通道恢复。AllowedIPs 应限制为必要网段。不要把秘密写入 ConfigMap 或日志；网络可达不等于已授权。

源码提供 `/station-gateway --status`，不依赖 `wg` 命令即可只读诊断。它从配置 JSON 读取接口名并查询内核，输出 JSON，包含接口及 peer 公钥、peer 最近握手时间（UTC；从未握手为 `null`）和收发字节数，不输出私钥、预共享密钥、端点或允许网段。命令需要在网关网络命名空间内具备查询 WireGuard 的权限；配置或设备不可读时返回失败。近期握手不代表端到端业务健康。该命令需要重新构建镜像；当前现场使用的私有仓库 WireGuard 镜像已包含并验证该状态命令。

本模块独立于根 Go workspace，测试命令为 `GOWORK=off go test ./...` 和 `GOWORK=off go vet ./...`。真实内核测试只能在一次性网络命名空间中运行：`unshare -Urn env GOWORK=off STATION_NETNS_TEST=1 go test -v -run TestKernelReconciliation ./...`。根目录测试目标不会自动覆盖本模块。

2026-11-11 在禁用网络、仅额外授予 `CAP_NET_ADMIN` 的一次性 Docker 网络命名空间中通过内核协调和状态测试。覆盖重复协调、拒绝地址迁移和冲突路由、保留外部路由/接口、恢复原配置后的重试、从真实内核 WireGuard 设备读取状态，以及设备不存在时失败。加入 `status.go` 后，镜像也在 `--network=none` 下完成构建。宿主环境禁止 `unshare --net`，因此测试改在 Docker 隔离网络命名空间中运行；这仍只是内核隔离测试，不是生产节点重启或恢复演练。

示例包含 exec readiness 探针（`/station-gateway --check`）。进程在协调成功后写入容器本地状态，探针要求状态不超过 30 秒且与当前配置内容一致；协调失败会清除状态，配置变化后须重新协调成功才能就绪。状态发布要求容器 `/tmp` 可写。该探针只表示近期本地协调成功，不能证明 peer 握手、端到端路由或业务可用；如果清除状态失败，旧状态可能在过期前仍有效。它不是自动回滚或 liveness 探针。现有部署需要使用新构建镜像并显式添加探针，旧缓存镜像不支持 `--check`。

## Station Pod 部署（示例）

`extensions/station/deploy/station-agent.yaml` 提供两个互斥的 DaemonSet。没有 `station.rlark.io/network=enabled` 标签的节点只运行身份容器；带标签的节点在同一个 host-network Pod 中运行身份和 WireGuard 容器。两个容器分别使用私有仓库镜像，只有 WireGuard 容器获得 `NET_ADMIN`。变更标签会替换 Pod，可能中断服务；切换不会删除接口或路由。

源码支持通过 `STATION_GATEWAY_OBSERVATION_FILE` 将快照写入共享 `emptyDir`。协调成功后写入所管理接口的公钥、内核地址、采集时间，以及每个 Peer 的公钥、最近一次握手时间和收发字节数：Leaf 掉线后，Hub 上的这些计数是唯一的旁证。不包含私钥、预共享密钥、Endpoint 或 AllowedIPs。身份容器使用 `--wireguard-observation-file` 读取；快照缺失、无效或超过 30 秒时清除 WireGuard 观测，身份心跳继续。共享卷不含私钥。Station 的上报时间是服务端接收时间，不是快照采集时间。该链路需要重新构建镜像并更新 DaemonSet，本身不提供面向用户的地址 API 或界面。

Station 身份服务源码还包含内部网络摘要读取器：查询时重新核对成员集群当前 Node UID 与有效 Station 绑定，要求观测的集群、Node、Station 和绑定版本归属一致，服务端接收时间超过 90 秒时标记为过期。旧观测缺少绑定版本时会被拒绝，直到新的上报写入。内部接口 `POST /v1/internal/node-network` 默认关闭；通过 `station-api --gateway-credential-file=PATH` 启用，使用每次请求从文件读取的独立 Bearer 凭证，长度为 32 至 4096 个非空白字符。Gateway 需配置 `--station-api-address`、`--station-api-ca-file` 和 `--station-gateway-credential-file`，凭证文件内容与 Station API 一致。凭证文件应按 Secret 保护，并使用独立随机凭证及经过校验的 TLS。示例不负责凭证创建或现场部署；未配置集成时界面显示网络数据不可用。

2026-11-11 已在本地 `cailiang` 节点部署此集成，并通过现有 5173 代理用认证用户请求验证。Gateway 容器由人工替换，已停止的 `rlark-gateway-before-wg-observation` 容器保留供回滚；本地 Station DaemonSet 的 revision 5 包含旧配置。随后云端和机器人网络 DaemonSet 均已升级，四个节点通过 5173 代理返回经验证的 WG 地址。云节点 1、云节点 2 和机器人运行时分别通过仅绑定 WG 的 Distribution 入口 `http://10.254.240.1:5443`，以 `imagePullPolicy: Always` 且不带拉取 Secret 成功拉取。该入口无仓库认证和 TLS，仅绑定本地 WG 地址，所有已连接 WG Peer 都可推拉；公司内网 `10.11.14.128:5443` 仓库保持原有 TLS 与认证。WG 入口仍依赖本机和隧道：机器人丢失镜像缓存或全新机器人在 WG 首次引导前须有独立镜像来源，整机重启恢复尚未测试。使用旧 Compose 定义重建 Gateway 会丢失新配置，应先同步部署定义再依赖自动重建。

使用前为每个网络节点在 `station-wireguard-config` 中提供 `<节点名>.json`，替换镜像地址，并将 hostPath 指向该节点现有 `private.key` 所在目录。示例要求所有网络节点使用相同宿主路径；已有路径不一致的集群必须先完成经验证的迁移或采用逐节点部署。旧网关和新容器不得同时协调同一接口。`STATION_GATEWAY_CONFIG` 同时为主进程和就绪检查选择配置文件。就绪仅表示近期本地协调成功，不代表端到端连通。当前站点清单使用仅限 WG 的仓库，首次引导依赖和恢复限制见上文。

网络模板使用各节点的 `/var/lib/station/wg/private.key`，Station 身份容器使用 `:18082` 提供探针，避开 host-network 控制器可能占用的默认 `:8081`。普通身份模板继续使用 `:8081`。agent 可通过 `--probe-address` 选择其他 TCP 地址，监听冲突会使 agent 在启动时退出。迁移前须确保节点已有正确密钥，不能静默替换身份。

2026-11-11，本地 `cailiang` 已迁移到双容器网络 Pod，现有密钥复制到统一目录并验证内容一致及权限 0600。Pod 重建后恢复 Ready，三个 Peer 有近期握手且隧道 ping 成功。早期远端镜像拉取故障已通过上文的 WG 专用仓库解决。整机重启和远端冷恢复尚未验证。

## 入网后的配置来源（2026-11-12）

设置 `STATION_GATEWAY_COMPUTED_CONFIG` 后，网关按顺序取第一个存在且能解析的文档：station-agent 从 Station API 取回并写入的配置 → 本机上次执行过的 `last-applied.json`（和私钥同目录，Station API 不可达时节点重启也能恢复同一条隧道）→ `--config` 的静态文件（引导和回退用，可以没有）。不设这个变量时只读静态文件，行为和以前一样。

- `STATION_GATEWAY_IDENTITY_FILE`：启动时把公钥写到这个文件，供 station-agent 登记，不用再从日志里抄。
- 对端逐个更新：只把有差别的对端发给内核，去掉的对端显式删除；没变的对端不碰，它们的会话和学到的端点保留。
- `STATION_GATEWAY_PRUNE_ROUTES=true`（默认关）：配置里去掉的路由由网关删除，范围只限它自己创建并打了别名的接口上、协议号 242、metric 49321 的路由。不开时仍是原来的行为：遇到这种路由就停止协调，等人工删除。上联路由（在别的接口上）任何时候都不删。

文档格式由 `extensions/station/network` 产出，两边共用 `testdata/computed-s1u-01-x86.json` 做契约测试。
