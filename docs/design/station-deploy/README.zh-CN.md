# Station 网络部署快照（试验性）

2026-11-12，通过一次性恢复网络控制器处理了 SiteNetwork generation 4，`s1u-02-x86` 已以 `10.254.240.7` 完成 Enrolled。随后已恢复正常的 NodeOperation 控制器；其当前镜像不包含网络调谐器，在该能力重新合入前，后续拓扑变更不会自动调谐。从 `cailiang` 到 .7 的路由使用 `st-wg0`，3 次 ping 均成功；新机器人到 .1 的 3 次 ping 和到 .2、.4、.5、.6 的各 1 次 ping 均成功。`cailiang` 到旧机器人 .3 的 2 次 ping 也成功。SiteNetwork 报告 Ready，但 FullyConnected=False，列出两台机器人 Leaf 之间双向无路径；本次测试未证明该路径。新节点因 Calico 跨节点 Pod TCP 问题仍保持 cordon。


在上述控制器恢复之前，已在管理面和成员集群将 `s1u-02-x86` 标记为 robot。身份专用 Station Deployment 已由双容器 Station 网络 DaemonSet 替换；该节点生成了自己的 WireGuard 密钥并上报 `st-wg0` / `10.254.240.7/32`。已在 SiteNetwork `default` 中加入期望的 Leaf，并在本地静态 ConfigMap 中加入后备 peer。注册仍为 pending：当前运行的控制器未启动 SiteNetwork 调谐器，SiteNetwork 当前 generation 4 的状态只观测到 generation 3。本地网关使用动态计算配置，因此尚无到 .7 的路由，也未验证握手。新节点因跨节点 Pod 网络问题仍保持 cordon；不能把地址上报或 2/2 Ready 当作网络连通。已通过 192.168.0.11、以 192.168.31.251 为源地址，添加到各 hub 端点的节点专用主机路由。


2026-11-12，笔记本 `NB260706069`（`10.11.107.154`）通过 kubeadm v1.31.14 加入集群。由于正式 node-agent DaemonSet 只选择 `rlark.io/containerd=fleet`，而该主机使用 `/run/containerd/containerd.sock`，当时必须手动创建两个固定到该节点的临时 Deployment 才达到 RLark readiness；Station Deployment 仅运行身份容器，不含 WireGuard。Join 操作对精确 Node UID 报告 Succeeded，但这不能证明 Agent 自动安装或 WireGuard 接入。

随后真实 Remove 操作（`node-op-212b9173335dfdeebe473c91124ee6b0`）依次完成 `Preflight`、`Drain`、`RemovePeer`、`Reset`、`VerifyRemoval` 和 `CleanupCredential`。Drain 开始前必须手动删除两个临时 Deployment：其 `kubernetes.io/hostname=nb260706069` 选择器使替代 Pod 在 Node cordon 后仍回到该节点。操作控制器不会删除这些工作负载定义。第一次 Remove 因 SSH 主机指纹校验而安全停止；提交的 Ed25519 指纹与 SSH 实际选择的 ECDSA 密钥不同。匹配的 ECDSA 指纹已存在于受信 `known_hosts`；失败操作及凭证在重试前已删除。

机器人集群现有仅允许对 `station-system/station-wireguard-config` 执行 `get`/`update` 的 peer-cleaner ServiceAccount，其操作 kubeconfig 已按机器人集群 ID 存入管理面；本地和金山云操作凭证也已核验。第一次 Remove 成功后，成员集群 Node 为 NotFound，操作记录的 UID 匹配；`/etc/kubernetes/kubelet.conf` 与 `bootstrap-kubelet.conf` 均不存在，kubelet 已停止，短期 SSH 凭证已回收。随后测试重新加入（`node-op-69969a21ce3df09cb15308de8d07cf8e`）成功运行 kubeadm，并创建 Ready 的新 Node，UID 为 `ca409bb3-2bf9-43f6-9aa7-1d65a5e93b0b`；但 RLark 验证按预期阻止操作，因为已有 Station 仍绑定已删除的旧 UID `622cef99-1069-47b6-ad9a-242b7a1ecb5d`。Station 身份重绑必须作为显式身份操作，Join 不会静默接管新的 Node UID。第二次 Remove（`node-op-d2b1e481196dfdfc14ee303afd1331f1`）随后 reset 并删除精确的新 UID；删除 Blocked Join 操作后，其 SSH 凭证由 finalizer 清理。Notebook 当前再次退出集群。RLark Node 和 Station 按长生命周期库存语义保留为 Offline，并仍绑定原 UID。本次验证了本地开发集群上的操作生命周期和失败清理，但没有验证 Station 自动重绑、任意固定节点工作负载清理、空白主机 Agent 自动安装、跨节点 Pod 网络或 WireGuard。

在后续替换网络 DaemonSet 之前，2026-11-12 已在保持 cordon 的新 worker 启动 `rlark-node-agent-s1u-02`，使用已有缓存 Agent 镜像，并将 `rlark-server` 指向可达的 10.11.14.128。Kubernetes Node 已设置 `rlark.io/enabled=true`，RLark 控制器报告 `s1u-02-x86` 为 Online/AgentLeaseActive。临时 host-network Deployment `station-agent-s1u-02` 仅运行身份容器；Station `Station-0002` 已绑定新 Node UID 并报告 RLarkLinked。该节点尚未配置 WireGuard 容器或 peer；替换为网络 DaemonSet 时必须删除身份专用 Deployment，避免重复 Agent。跨节点 Pod 网络尚未解决，worker 继续 cordon；Pod Ready 不代表工作负载端到端已验收。


2026-11-12 发现新 x86 没有到内网镜像仓库 `10.11.14.128:5443` 的路由。已在其活动 NetworkManager 配置中持久添加经 `192.168.0.11`、源地址为 `192.168.31.251` 的仓库主机路由；已为专用 containerd 配置仓库 CA，指纹与已有受信 CA 一致，并成功从新节点通过 TLS 校验和认证拉取 `busybox:latest`。该路由仅覆盖此仓库，不代表内网其他地址可达。机器人网络接入脚本尚不配置此路由或 CA。跨节点 Pod 网络仍未解决，节点保持 cordon。


地址迁移后、上述 Agent 部署前的现场状态：`s1u-01-x86`（192.168.31.250）和 `s1u-02-x86`（192.168.31.251）均为 Kubernetes Ready，Calico 和 kube-proxy 均 Running。新节点没有默认路由，内部网卡同样拥有 192.168.0.10，因此持久配置了 Service 网段路由和仅针对 API Service 的 SNAT 规则。不同节点测试 Pod 之间的 TCP 连接超时，尽管 IPIP 包已到达目标网卡，原因尚未解决。测试 Pod 已删除；新节点已 cordon 以阻止普通工作负载调度，尚未部署 RLark Node Agent 和 Station Agent，不能视为已通过工作负载验收。旧节点 RLark 和 Station Pod 正常运行；重启和跨节点恢复尚未验证。


2026-11-12 旧机器人 Kubernetes API 和 Node 已迁移至 192.168.31.250；API 证书包含该地址，kube-proxy 与 kubeconfig 已更新，etcd peer 和客户端端点改为回环地址，Calico 改为使用 Kubernetes Internal IP 自动选址。API 健康，kube-proxy 和 Calico 滚动更新完成，旧 Node 为 Ready。新 worker 尚未加入，跨节点 Pod 通信和重启恢复尚未验证。新 API 证书暂时保留 192.168.0.10 兼容 SAN。备份和 etcd 快照保留在仅 root 可访问的 `/var/backups/rlark-node-address-20261112`。


2026-11-12 11:10 UTC 已在同一受保护备份目录保存新的 etcd 快照和当前控制面 ConfigMap，并检查快照状态；包含 192.168.31.250 的新 API 证书已在备份目录生成并通过现有 CA 验证，尚未安装。新 x86 启用了 2 GiB swap，尚无 containerd 或默认路由；现有集群 kubelet 配置为 failSwapOn=false、工作负载 NoSwap，新节点的兼容性仍需验证。现场控制面尚未迁移，新节点尚未加入。


2026-11-12，旧机器人控制面迁移到独立局域网地址尚未完成；Kubernetes API、节点和 etcd 仍使用 192.168.0.10。旧 x86 的 `/var/backups/rlark-node-address-20261112` 保存了仅 root 可访问的配置备份和 etcd 快照；快照曾恢复到临时目录验证，验证后临时目录已删除。隔离 Kubernetes v1.31.0 集群演练了 API 证书与地址切换及回退、etcd peer 迁移及回退；这不能证明现场 v1.31.14 集群迁移已验证。经两台 Orin 的 IPv4 协议 4 数据包已在双向目标网卡观察到，但尚未验证 Calico 跨节点 Pod 网络。新 x86 尚未加入集群。备份包含敏感凭据，应妥善保护和管理。


## 将机器人 x86 接入局域网

`robot-x86-network.py` 通过 Orin 的 IPv4 转发和代理 ARP 为 x86 配置局域网 /32 地址，并非二层网桥。管理机需要 Python 3 和 OpenSSH；两台设备需要 Python 3、NetworkManager、sudo 和 SSH，Orin 还需要 systemd 与 iptables。脚本使用 SSH 主机密钥校验和交互式认证，不保存密码；首次连接时应通过可信渠道核对主机指纹。

先预留唯一的 x86 地址，确认没有分配给其他设备且不在路由器 DHCP 地址池中。ping 不通不能证明地址可用。例如：

```bash
python3 docs/design/station-deploy/robot-x86-network.py check --orin 192.168.31.5 --external 192.168.31.251
python3 docs/design/station-deploy/robot-x86-network.py apply --orin 192.168.31.5 --external 192.168.31.251 --address-reserved
ping 192.168.31.251
ssh astribot@192.168.31.251
```

可通过 `--help` 查看局域网、内部地址及用户名参数。脚本分别识别内外网卡，保留已有地址和 x86 默认路由，并安装限定该地址的转发规则。两端原配置备份在 `/var/lib/rlark-robot-x86-network/`，不要编辑或删除。重复执行 `apply` 可恢复中断的操作；遇到不明配置或规则冲突时停止并人工检查。撤销时使用相同地址参数执行 `restore`；脚本不会覆盖执行后被修改的连接配置。如果某台设备暂时不可达，恢复连接后再次执行撤销。部署后检查服务连通性和防火墙策略；主机防火墙、网络隔离或后续防火墙重载可能阻断流量。配置会持久化，但重启恢复需要单独验证。

2026-11-12 已为 Orin `.5` 后的 x86 配置 `.251`：从实验室局域网三次 ICMP 均成功，SSH 登录成功；旧 x86 `.250` 也可 ping 和 SSH。尚未验证重启或防火墙重载后的行为。

Station 镜像和云上工作负载的仓库分工（金山云走 KS3，本地开发和机器人集群走 `local-registry`）见[当前仓库拓扑](../../operations/private-registry.zh-CN.md#当前仓库拓扑2026-11-12)。

裸金属 SSH 入口和当前 rclone CSI 覆盖情况见[金山云裸金属资产页](../bare-metal-assets.zh-CN.md)。

2026-11-11 最新现场结果：真实 Gateway 的 Station 路由已部署，管理员经前端代理查询返回 HTTP 200，`cailiang`、`s1u-01-x86`、`10-0-2-57`、`10-0-2-196` 四条 Station 均 Bound。四个节点现在由 `station-agent-network` DaemonSet 管理，每个网络节点的 Pod 包含身份和 WireGuard 两个容器；旧的身份、site 和 hub DaemonSet 已删除。镜像通过临时 Kubernetes Pod 导入各节点 containerd，并按摘要使用 `imagePullPolicy: Never`；节点重建或镜像回收后需要重新导入。Station API 与独立 PostgreSQL 使用长期开发证书并验证 TLS；管理/成员 token 尚未自动轮换，数据库备份恢复、实际重绑、公网镜像仓库和长期稳定性仍未验收。

较早部署记录（以本页最新结果为准）：2026-11-11 真实 Station 身份链路已开始部署：管理 kcp 的五份 Station CRD 均 Established，本地 `station-api`、`station-controller`、`station-agent` 均 Ready；本地真实 Pod-bound token 已产生 `cailiang` Observation 和 Bound Station。API 监听 WireGuard 地址 `10.254.240.1:18443` 并使用 TLS；独立 PostgreSQL 容器以 TLS 监听本地 `127.0.0.1:15432`，数据库角色独立，原 RLark PostgreSQL 未更改。三地已安装成员读取 RBAC，成员凭据位于管理 kcp Secret。机器人和金山云 agent Pod 已创建，因运行时以 HTTPS 访问 HTTP 引导仓库而 ImagePullBackOff，离线镜像导入尚未完成；Gateway/UI 的 Station 真实链路未部署。证书仅 30 天、管理及成员 token 为长期测试凭据且无自动轮换，数据库备份与恢复尚未验收。

旧的站点快照清单已删除，不再作为部署入口。当前唯一部署入口是 `extensions/station/deploy/station-agent.yaml`：普通节点运行身份容器，带 `station.rlark.io/network=enabled` 标签的节点在同一 Pod 中运行身份和 WireGuard 容器。各节点使用 `/var/lib/station/wg/private.key`，网络配置由 `station-wireguard-config` 提供。

2026-11-11 只读复测使用已记录的 SSH host key 和用户提供的 root 凭据：两个公网 SSH 转发端口可达，Kubernetes API 显示两台节点均 Ready；站点网关、hub、RLark Agent、CSI 和引导 registry 均 Running/Ready。开发机经 `10.254.240.2:6443` 访问节点 1 API，`/readyz` 返回 HTTP 200；节点 2 按 worker 角色不监听 API 端口。此结果证明当前可达和工作负载状态，不代表隧道长期稳定或空节点恢复已验证。

当前四个网络节点使用私有仓库中的 Station 和 WireGuard 镜像，均按 manifest digest 固定并使用 `imagePullPolicy: Never`。镜像通过临时 Kubernetes Pod 导入各节点使用的 containerd，机器人 socket 为 `/run/containerd-robot/containerd.sock`。节点重建或镜像被回收后必须重新导入；公网镜像仓库尚未建设。不要更改接口名或在另一节点复用密钥目录。

2026-11-11：四个网络节点切换到双容器 Station Pod，删除重建后均恢复 `2/2 Ready`；本地、机器人和两台裸金属节点的隧道地址复测无丢包。当前镜像和私钥迁移均按摘要、内容和权限核对完成。公网镜像仓库、整机重启和清空缓存后的恢复尚未验收。

断开开发机实验室 Wi-Fi 后复测：`wlp0s20f3` 为 DOWN，宿主机 `st-wg0` 保持 UP，路由仍指向 `10.254.240.2/.3/.4`。经 `.3:22` SSH 登录成功（目标报告主机名 `x86`）；机器人 Kubernetes API 改用本地 `/dev/shm` 临时 kubeconfig 副本指向 `10.254.240.3:6443` 后，节点 `s1u-01-x86` 为 Ready，站点网关、Cluster Agent 和 Node Agent 均 Running。管理面本地、机器人和金山云三个 RLark Node 均为 Online。隧道地址 `.2/.3/.4` 的 TCP 22 可达；`.2/.3` 的 TCP 6443 可达，`.4:6443` 未开放。此次只验证路径连通性及 Kubernetes/管理面状态；环境没有 `wg` 命令，未读取本次 peer 握手时间。当前网关接口累计计数存在 2 个 TX error 和 1 个 dropped，不能据此归因或判断本次测试期间丢包；持续稳定性仍需采样。

用户确认关闭 Wi-Fi 后的再次复测：`wlp0s20f3` 仍为 DOWN，HTTPS `https://10.254.240.3:6443/readyz` 返回 `ok`，`.3:22` 接受 TCP 连接，至 `.2`、`.3`、`.4` 各 3 次 ICMP 均无丢包。本轮 shell 未能复现先前 SSH 登录：当前配置的 key 被拒绝。没有验证或测试机器人密码；会话中可用的密码是用于开发机 `sudo`，并未确认是机器人凭据，因此本轮不将 SSH 认证记为已验证。当前本地 kubectl context 没有 RLark Cluster CRD，不能独立确认三地管理面 Node 心跳；更早的在线状态如上记录。WireGuard 握手时间和长期丢包仍未验证。

断开 Wi-Fi 后金山云只读复核（2026-11-11 00:22-00:23 UTC）：SSH `36.103.180.169:2222` 登录到 `10-0-2-57`，`:2223` 登录到 `10-0-2-196`。两节点均为 Kubernetes 1.31.14 Ready。Station site/hub、RLark Agent/Node Agent 和 CSI Pod 均 Running/Ready。节点 2 的 `st-hub0` 地址为 `10.254.240.4/32`，存在到 `10.254.240.1` 的路由并监听 UDP 51821；到 NAS `10.11.5.12` 的路由使用 `st-hub0`，TCP 445 可达。开发机通过 `10.254.240.2:6443` 访问 API `/readyz` 返回 HTTP 200；机器人 `.3:6443` 也返回 200。云端宿主机与 gateway 镜像均没有 `wg` 工具或 peer 握手状态接口，因此本轮未确认最新握手时间。没有修改云端 Kubernetes 资源或宿主机配置。

2026-11-11 01:15 UTC，本地控制面 controller-manager 最新分别于 `01:15:36Z`、`01:15:32Z`、`01:15:42Z` 报告以下 RLark Node 为 `Online/AgentLeaseActive`：`rlark-local-dev-gpu/cailiang`、`rlark-robot-s1u-01/s1u-01-x86`、`rlark-ksyun-gpu/10-0-2-196`。这确认断开 Wi-Fi 后，三台已注册数据面 Node 的管理心跳持续在线。KCE 节点 `10-0-2-57` 承担云端控制面/网关角色，本次 controller 日志没有把它报告为 RLark 计算 Node。

用户断开开发机实验室 Wi-Fi 后再次复测（2026-11-11 01:23 UTC）：`wlp0s20f3` 仍为 DOWN，至 `10.254.240.2/.3/.4` 的路由均使用 `st-wg0`。三个隧道地址各 3 次 ICMP 均成功；三端 TCP 22 可达，`.3:6443` 可达，`https://10.254.240.3:6443/readyz` 返回 HTTP 200。机器人 kubeconfig 文件仍指向已失效的 `192.168.31.250`；未改文件，通过 kubectl 临时指定 `.3` 并使用证书已有的 TLS 名称 `192.168.0.10`，由 kubeconfig CA 完成验证，结果为 `s1u-01-x86` Ready，RLark Agent、Node Agent 和 CSI Pod 均 Running/Ready。本地 controller-manager 至 `01:23:02Z` 持续报告三个已注册 RLark Node 为 `Online/AgentLeaseActive`。机器人 `.3:22` 仅验证 TCP 可达，SSH 登录未验证：现有 key 被拒绝，也没有收到机器人密码。环境缺少 `wg` 命令，且没有权限读取 debugfs peer 状态，因此本次无法核对最新握手时间。接口计数是累计值（2 个 TX error、1 个 dropped），不能据此认定本轮发生丢包。

网关源码现提供只读 `--status` 命令，无需安装 `wg` 即可检查 peer 握手时间和计数；单元、race、`go vet` 及真实内核 Docker 隔离测试通过，镜像也已离线构建。运行中的网关尚未部署该版本。详见[网关说明](https://github.com/astribot-post-train/RLark/blob/main/extensions/station-gateway/README.zh-CN.md)。

2026-11-11 01:51 UTC，使用本地构建的状态镜像启动一次性 host-network Docker 容器，以 `CAP_NET_ADMIN` 只读查询 `st-wg0`，未更改运行中的网关。公钥和地址现由 `station-wireguard-config` 管理；本地网关看到三个远端 Peer 有近期握手，随后到各隧道地址的 ICMP 均成功。这只是本地网关视角，不能据此证明整机重启、清空镜像缓存后的恢复。

2026-11-11 01:54 UTC，在机器人创建短时 host-network 诊断 Pod，使用节点已缓存的 RLark Agent 镜像和 `CAP_NET_ADMIN`，从容器内 `/tmp` 运行新构建的静态状态程序；没有更改宿主机配置或运行中的网关。机器人内核报告与本地 peer 最近握手为 01:52:43Z、与金山云节点 1 为 01:53:52Z；云 peer 累计接收 37,896 字节、发送 129,976 字节。查询后已删除诊断 Pod。该结果确认机器人到云的近期 WireGuard 握手及此前双向流量，不能证明本轮业务数据传输、长期稳定性或重启恢复。

2026-11-11 01:56 UTC，机器人现有 host-network RLark Agent 连接 `10.254.240.2:22` 并收到云端 SSH 协议标识；机器人路由表中 `10.254.240.2/32` 指向 `st-wg0`，开发机 Wi-Fi 仍为 DOWN。本轮验证了机器人经隧道到云的实时协议数据交换，没有认证或执行远端命令；持续流量、业务负载和重启恢复尚未由此验证。

补充短时网络采样：并发 40 次 ICMP，云节点 1、节点 2 隧道和共同公网地址各收到 36 次，机器人收到 40 次；80 次复测分别收到 77、77、78、80 次，云路径没有共同丢失序号。另一次 60 次采样公司网关收到 59 次，云公网收到 60 次。两次顺序执行的 8 MiB SSH 从云节点 1 经公网约 5.21 MiB/s、经隧道约 9.87 MiB/s。样本短且受瞬时条件影响，不证明长期带宽，也未确定丢包或此前镜像下载慢的原因。

私钥分别生成并保存在宿主机 `/var/lib/rlark-station/site` 或 `/var/lib/rlark-station/hub`，由 Pod 挂载，清单不含私钥。应安全保护并备份；丢失或克隆系统盘需要明确更换密钥并更新对端。变更时保留独立于隧道的管理通道。ConfigMap 投影和 10 秒协调周期会使配置生效延迟；应用后检查 Pod 日志、路由、公钥、握手及具体服务访问。

网关创建内核路由，退出时不会清除，包括机器人两条指定源地址的上联路由。组件不提供自动回滚、防火墙转发、路由删除、端到端健康探针或整机重启编排；exec 探针只检查近期协调和当前配置，不证明隧道或业务可达；部分失败可能留下部分状态。移除或更改路由前须通过独立管理通道核验归属，不能仅凭协议号或 metric 删除。重启时没有归属别名的接口会被拒绝接管。限制详见[网关文档](https://github.com/astribot-post-train/RLark/blob/main/extensions/station-gateway/README.zh-CN.md)。

云端 `rlark-system/rlark-server` Service 和 Endpoints 指向 10.254.240.1，是当前 RLark Agent 的依赖；本地按云端两个 Pod CIDR 将回程分配给两个 WireGuard Peer。本地 `rlark-station-nas-forwarder` Pod 仅为源 10.254.240.4 到 NAS 10.11.5.12 的 TCP 445 安装转发和 SNAT；详见[转发器文档](https://github.com/astribot-post-train/RLark/blob/main/extensions/station-forwarder/README.zh-CN.md)。Pod 删除后规则仍保留，需核对后准确手工清理。云端 worker Agent 镜像单独导入；5555 端口的开放测试仓库仅用于引导，不能等同于带认证的 Distribution。机器人 RLark 和 CSI 部署仍含旧镜像地址，目前依靠缓存启动，空节点恢复尚未验证。

云端节点 2 的引导仓库是 Kubernetes DaemonSet，仅监听 `127.0.0.1:5556`，使用节点本地持久目录。它没有认证：宿主机管理员和该节点的 hostNetwork 工作负载均可推拉；它不是租户隔离仓库或独立备份。启动仓库 Pod 前，其自身镜像必须已经存在于节点运行时。本地转发 Pod 的 exec readiness 检查固定配置、接口和精确防火墙规则，但不能证明 NAS 端到端访问或规则顺序。

`csi-pv-patch-rbac.yaml` 是现有旧版 CSI Addon 的临时 Kubernetes RBAC 授权：provisioner 需要对 PersistentVolume 执行 `patch` 才能移除自己的 finalizer。该授权允许现有 CSI 服务账号修改整个集群的 PV；当前仅本地和机器人集群保留该授权；云端 Agent 升级后，其 Addon 管理的 ClusterRole 已具备权限，云端临时授权已移除。本地和机器人升级并验证后也应删除这两个对象。它不解决 controller 调度或镜像恢复。

开发机本地恢复包位于 `~/.local/share/rlark-station-recovery-20261111/`，包含 `gateway-images.tar` 和相对路径 SHA-256 校验文件；它包括本地/机器人及云端当前网关的两个标签，OCI manifest 摘要为 `sha256:30ce8ff56390a2df27cdafaf7585590a4276dccac4e4739ed17e2e455e6677df`。在该目录执行 `sha256sum -c gateway-images.tar.sha256` 后，通过独立管理通道将归档导入相应节点的 Kubernetes containerd（机器人 socket 为 `/run/containerd-robot/containerd.sock`），再核对标签和摘要。该包仅存于开发机，不含私钥、Kubernetes 凭证或其他组件镜像，已在独立新建 containerd 内容库用 `ctr images import --no-unpack` 导入并验证两个标签和 manifest 摘要；在无特权用户命名空间尝试解包因 bind mount 权限不足失败，随后在机器人上以独立 containerd 数据目录和隔离网络完整导入并解包两个标签，启动镜像执行 `--check`，按预期因无配置退出；临时文件已删除。这不等于空节点 Kubernetes 启动或整机恢复；需另行保存到受控的独立备份位置。

同一目录的 `forwarder-image.tar` 和校验文件保存本地 NAS 转发器现用镜像，OCI manifest 摘要为 `sha256:2ca50876a23a2f5769f9b8c92faea8431be7aaf49f66766a8fc6a86b34019074`。归档和所有 OCI 层摘要已验证，并已导入独立空 containerd 内容库（未解包）；随后在机器人隔离网络与独立 containerd 中成功解包并启动 `--check`，因无配置按预期退出；临时文件已清理，未操作现有防火墙。空节点 Kubernetes 启动、规则恢复及 NAS 端到端访问仍未验证。该包只保存在开发机，需独立备份。

同目录另含 `agent-images.tar` 和 `csi-local-images.tar`，各有 SHA-256 文件。Agent 包包含本地、机器人及云端实际引用的旧 Agent 与云端 v3 Agent 标签；两个 manifest 及全部 OCI 内容摘要已验证，四个标签在独立空 containerd 内容库中导入成功；随后在机器人隔离网络中的独立 containerd 全部解包，并分别运行旧 Agent 和云端 v3 Agent 的 `--help` 成功，测试临时文件已清理。CSI 包包含本地仓库地址的四个 CSI 镜像，归档校验和及独立内容库导入通过；三地运行中的四种 CSI 镜像摘要已逐一核对一致，另生成 `csi-three-site-images.tar`（附 SHA-256），包含三地共 12 个现场标签，独立空 containerd 内容库导入通过；随后在机器人隔离网络中的独立 containerd 全部解包成功，临时文件已清理。以上包尚未验证空节点 Kubernetes 上的完整 Agent/CSI 启动，不含证书、私钥、NAS 凭据、Kubernetes 组件或业务镜像，也没有独立备份。

2026-11-11 云端 Agent 现场核对：`rlark-system/rlark-agent` Deployment 的容器镜像更新为 `10.254.240.1:5555/rlark-cloud-agent:wg-nodeport-1deb4ef-20261111`，并设置 `RLARK_WG_NODEPORT_ENABLED=true`；工作负载初始化参数 `--image` 仍引用 `10.254.240.1:5555/rlark-current:20261106-agent-67eaa39`。新 Agent 镜像已预载到节点 `10-0-2-196`，备份归档位于该节点的 `/var/backups/rlark-datatools-20261111-173832/agent-image.tar.gz`，尚未纳入独立恢复包。云节点直接从 5555 仓库拉取该镜像因 HTTPS 与 HTTP 不匹配而失败，不能假设空节点可拉取；恢复时须先验证归档校验、预载镜像及仓库配置。Agent 使用 `rlark-agent` ServiceAccount、`rlark-agent-cert` Secret 和 `https://rlark-server:8443`，并依赖此目录的云端 Service/Endpoints 清单；证书和私钥不在恢复包或快照中。管理面 `rlark-ksyun-gpu/csi-driver-rclone` 的 `controllerNodeSelector` 和 `nodeSelector` 均为 `kubernetes.io/hostname=10-0-2-196`，CSI 镜像引用依赖节点本地 5556 仓库。恢复时需先恢复独立管理通道、私钥与证书、节点镜像及仓库，再检查服务、Addon 和存储；仅导入镜像不足以恢复。

同日为 WG 端口预览将云集群 kube-proxy 的 `nodePortAddresses` 限制为 `10.254.240.0/24` 并重启两个节点的 kube-proxy。变更前该集群没有 NodePort 或 LoadBalancer Service；此限制对集群所有 NodePort 生效。临时测试 Service 在开发机访问两个 WG 地址均返回 HTTP 200，从云节点测试两个非 WG 地址均无法连接；测试 Service 已删除，不能据此推断其他来源的全部网络策略。变更前 kube-proxy ConfigMap、DaemonSet 和 Agent Deployment 快照保存在云控制节点 `/var/backups/rlark-datatools-20261111-173832/`。回滚前先关闭依赖 WG 隔离的端口及 Agent 开关，再通过独立管理通道恢复配置并逐节点验证。Data Tools 管理员已初始化；30085 曾短暂映射并可通过两个 WG 地址访问，但验证发现未登录也可读取数据集接口，已撤回映射并确认两个地址均无法连接。此前 30086 测试期间也曾短暂暴露服务；无法据此确认是否有其他 Peer 访问。重新开放前须修复并验证服务端认证或部署等效访问控制。端口配置变更还触发滚动更新，新副本因节点反亲和规则无法调度；恢复原配置后 Task 已回到 Running。

2026-11-11 只读核验证书有效期：本地与机器人使用 `rlark-system/agent-certs`，客户端证书分别于 2027-10-28 12:58:27 UTC、08:33:16 UTC 到期；云端 `rlark-system/rlark-agent-cert` 客户端证书于 2027-11-10 12:43:37 UTC 到期。三处 CA 证书均于 2046-10-21 03:02:38 UTC 到期。这只检查证书元数据，不证明私钥备份、自动续期、撤销或轮换可用；恢复与轮换方案需另行验证。

源码核对：Gateway 的管理员证书签发接口会返回证书和私钥，并尝试写入管理面 Secret；旧实现即使 Secret 写入失败也可能返回成功。源码已修复为写入失败返回错误、拒绝归属不明 Secret，并在冲突时重读重试；相关单元测试通过；在本地 Kubernetes 临时命名空间使用测试数据验证创建、更新和归属冲突拒绝，测试命名空间已删除。已用与当前提交一致、仅叠加证书修复的 Gateway 二进制启动独立本地预览容器，未认证证书列表返回 401；预览容器已移除。另用临时 CA、进程内签发服务和本地 Kubernetes 临时命名空间，调用修复后的 Gateway 签发两次，验证证书签名、私钥配对、Secret 内容和序列号变化；临时命名空间已清理。另新增 Server 真实 mTLS 集成测试：无客户端证书被 TLS 拒绝，非管理员证书获 403，管理员可签发且所签证书的签名、密钥配对与 Agent 身份元数据正确。该测试使用临时 CA，不覆盖现场 Server、Agent 重连或撤销。现场 Gateway 已切换至 `localhost:5555/rlark-gateway:cert-persistence-20261111`，未认证请求返回 401，管理员登录及证书列表通过；原容器以 `rlark-gateway-pre-cert-20261111` 停止保留供回滚，现场证书未轮换。管理面当前 7 个带 Agent 证书标签的 Secret 均有与名称一致的集群标识注解；该只读检查不证明其他无标签同名 Secret 可安全接管。Agent 创建 Server 客户端时从文件加载证书和 CA，不能据此假定 Secret 投影更新会使运行中客户端自动重载。续期设计需在隔离环境验证签发与持久化、证书和私钥配对、受控更新目标 Secret、Agent 重启/重连、旧证书撤销、失败回滚和审计；测试不能覆盖现有生产凭证。签发响应含私钥，不得写入终端记录、日志或设计文档。

2026-11-11 撤销上线前只读核查：现场 Server 启动参数指定数据库配置，该配置指向同 Docker 网络的 `postgresql:5432/rlark`；该库的 `revoked_certificates` 表存在，五列均非空，主键为 `(cert_type, serial_number, subject_key_id)`，与当前写入冲突约束一致。此检查未验证数据库故障恢复、备份或现场撤销，也不代表可直接切换；上线前仍须准备回滚并验证认证与重连。

撤销能力核查：现场 Gateway 的 `/api/v1/certificates/revoke` 尚未实现。源码现已转发管理员请求并要求 Server 明确返回持久化回执；旧 Server 响应会报告结果未知，可使用相同身份重试。该变更需要同时升级两个组件，尚未部署。Server 源码的撤销路径在未配置存储时只写进程内缓存；隔离测试验证重建 Server 后记录消失。现场 Server 当前会忽略数据库查询错误；源码已改为配置存储时查询失败拒绝新认证、不缓存未撤销结果，并在无持久存储时拒绝撤销请求，测试通过但尚未部署。隔离 PostgreSQL 15 测试中，先查询未撤销，再停止数据库，下一次查询返回错误；重启数据库并写入撤销记录后，新 Server 实例可读取该记录。测试数据库已删除，现场数据库未中断。现场多副本传播、缓存失效和现网撤销链路均未验证；不能据此声称旧证书可可靠撤销。

另一个隔离 PostgreSQL 测试直接调用修复后的 Server HTTP 处理器：一个 Server 撤销同一测试证书两次均成功，另一个共享数据库的 Server 随后拒绝该证书，新建 Server 实例也能读到撤销记录。测试数据库已删除；测试未通过真实 mTLS 传输或 Gateway，也未验证已有连接终止和现场多副本。

撤销标识兼容性：源码测试使用临时 CA 分别签发 X.509 和 SSH 证书，将实际序列号与 subject key ID/SSH key ID 传入撤销处理器，验证写入标识与所签证书一致，并由另一个 Server 实例读取测试存储。该测试使用内存替身；实际数据库的跨实例撤销另有隔离测试，尚未在现场启用。

新增隔离 mTLS 集成测试使用临时 CA、两个 Server 和共享测试存储：Agent 证书撤销前访问成功，管理员在另一 Server 提交撤销后新连接收到 401；新 Server 遇存储故障返回 503。随后在隔离环境组合真实 mTLS 与 PostgreSQL 15：两个 Server 共享数据库，重复撤销成功，撤销后新认证返回 401；数据库停止时新 Server 返回 503，重启后另一新 Server 仍返回 401。临时数据库已删除。进一步使用 Gateway 管理员 JWT、真实 mTLS Server 与隔离 PostgreSQL 测试完整链路：重复撤销成功，数据库停止时 Gateway 返回 502，恢复后相同请求重试成功，另一 Server 仍拒绝已撤销证书；临时数据库已删除。既有连接终止和现场多副本仍未验证；修复仍未部署。

## Station API 原型

历史原型说明（当前部署以本页最新结果为准）：`extensions/station/deploy` 中的 `station-api.yaml`、`management-rbac.yaml` 和 `member-rbac.yaml` 是 Kubernetes 示例清单，已通过 `kubectl create --dry-run=client` 解析，但没有应用。可用 `make -C apps/rlark station-api` 构建二进制，或通过 RLark Docker 构建生成镜像。工作负载挂载 `station-management-kubeconfig`、`station-api-tls`、`station-api-database` 和 `station-api-database-ca` Secrets；清单不包含其值。数据库 DSN 必须设置 `sslmode=verify-full`，并将 `sslrootcert` 指向 `/var/run/secrets/station-db/ca.crt`。TLS 证书必须覆盖 Service DNS。服务当前仅为 ClusterIP，跨站 Agent 的可达路径及证书 SAN 尚未确定。

管理 kubeconfig 的身份必须是用户 `station-api`，仅授予 `management-rbac.yaml` 中的权限。member kubeconfig Secret 放在管理 API 的 `station-member-credentials` namespace，名称为 `station-member-` 加 cluster ID 的 SHA-256 前 10 字节的小写无填充 base32；annotation `station.rlark.io/cluster-id` 必须与 Cluster 对象名称一致，data 必须包含 `kubeconfig`、`service-account-namespace`、`service-account-name`、`service-account-uid` 和 `audience`。member kubeconfig 使用 `member-rbac.yaml` 中受限的 `station-api-reader` 权限，且必须是验证 TLS 的 HTTPS、信任证书和凭据内嵌形式。API 会重读 Secret，内容变化后刷新 client；目前没有自动轮换。station-agent 使用独立 ServiceAccount，并须请求 audience 为 `station.rlark.io` 的 Pod-bound token。

`station-agent.yaml` 是原型 DaemonSet。需替换 ConfigMap 中的 endpoint 和已注册 Cluster 对象名，创建名为 `station-api-ca` 的可信 CA Secret 并写入 `ca.crt`；endpoint 必须匹配该 CA，并能从所有成员节点访问。Agent 每次请求都会重新读取 `station.rlark.io` projected token，因此能观察投射 token 轮换而无需重启 Pod。HTTP client 不使用环境代理并拒绝重定向，并从 `/proc/stat` 解析内核启动时间作为信息性观测上报。`btime` 缺失或无效会阻止本次上报并触发重试。Agent 不提交自报 Node 名，由 API 从绑定 Pod 解析 Node。当前尚不采集硬件标识、WireGuard peer 或主动探测遥测。健康接口监听 Pod 的 8081 端口供 Kubernetes 探针访问；readiness 反映最近一次 API 上报是否成功。本地 agent 已部署并通过真实 API 上报。

由独立迁移 owner 使用 `apps/rlark/config/station-api-challenges.sql` 预建 `station_api.challenges`。`station-api-database` Secret 的 `dsn` 使用仅有 schema usage 和表 SELECT/INSERT/UPDATE 的运行账号，不能授予在线服务 DELETE；过期 challenge 清理须使用独立 janitor 身份并另行调度。Gateway 管理员重绑接口已实现操作创建和查询，但尚未部署或接入现场 CRD/controller。生产 `SensitiveAuthorizer`、动作 adapter 和审计路径仍未实现，因此 challenge 与敏感路由继续 fail closed；当前 station-api 原型只接受已认证 Observation。这些清单只通过客户端解析，没有通过现场 RBAC、TLS、数据库或 CRD status 写入验证。
