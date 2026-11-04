# 容器网关与托管接入计划

状态：部分实现。独立扩展已经新增可选的 A/Hub mTLS 固定 Agent API 转发组件及
测试，并完成下面所述、已授权金山云 A 开发机的接入验收。UI 接入/退役流程、自动
证书续签、Docker Engine 执行以及云端到机器人的数据通路仍未完成。

控制器级[混合 Job 预览](mixed-container-job-preview.md)现在能在一个原生 Job 下
生成 Raw rollout 和显式绑定的 Kubernetes Env Task，同时保持 A/B/C 属于同一个
逻辑容器 Cluster，机器人保留原有 Kubernetes Cluster。各运行时完成观测和停止
恢复已有自动化测试。Fleet 启动屏障、成员失败自动取消、进程中继注入及 Lumo2/S1U
验收仍未完成，不能视为云端到机器人 rollout 通路已经可用。

## 2026-11-02 A 接入验收

保留的 `ksyun-containers` 集群现在包含绑定原生 Raw Node 的 ContainerNode
`ksyun-a`。A 通过公网 TCP 30000 入口使用专用、固定指纹的 mTLS 网关连接 RLark
Hub；运行时不使用 SSH 转发。经过认证的 ContainerNode 心跳报告了授权的 5 张 H100、
100 CPU 和观测到的内存容量。只重启本地 Hub 后，连接先断开又恢复，并产生新鲜心跳，
重连行为通过验证。

A 是没有 Docker Engine、systemd 或 `CAP_NET_ADMIN` 的现有 Notebook 容器，因此本次
验收使用 Raw 进程执行，不创建 Docker 容器，也不提供 Kubernetes 网络。网关、Raw
supervisor 和 Agent 由三个新增的 supervisord 程序管理，不会重启 Notebook 原有服务。
由于现有 Ray/Fleet 工作负载正在使用这台机器，原生 Node 仍保持
`spec.unschedulable=true`，没有下发测试 Job。Agent 包当前使用 72 小时节点证书，
过期前必须手动重新生成并安装。本次节点证书于北京时间 2026-11-05 19:41:27
（UTC 11:41:27）过期，未启用自动续签；Agent 日志读取和滚动升级端点也未启用。
A 仅安装节点专用叶证书/私钥、服务端 CA、网关叶证书/私钥及 Hub 公共 CA；管理
kubeconfig 和 CA 签发私钥保留在控制面主机上。

包中使用规范 Python 路径
`/root/fleet/python/bin/python3.11`；`/opt/conda/bin/python3.11` 是符号链接，
会被 Raw supervisor 的路径校验有意拒绝。

扩展控制器现在会根据经过认证的 ContainerNode 观测发布严格绑定的原生 Node 和
Cluster heartbeat Lease，因此核心控制器会继续提供与 Kubernetes 集群相同的
`Online`、`Downgraded`、`Offline` 聚合状态。适配器还会投影核心 Raw Lease 校验所需的
Raw 能力和凭据身份，并绑定精确的原生对象 UID 和附属对象 UID；不会修改
`Node.spec`、Raw reservation 或 Docker reservation。Online 只表示连接新鲜，不代表
节点可调度；最终可调度判断仍检查原生 Node 的 `spec.unschedulable` 和 allocation 状态。
Raw supervisor 空闲也不代表现有 Ray/Fleet 占用的 GPU 空闲。

控制面主机通过已启用的用户 systemd 服务运行 `rlark-container-controller`、
`rlark-container-api` 和 `rlark-container-hub`，并启用 user lingering，避免退出
登录停止服务。API 只监听 `127.0.0.1:9444`；A 的私网 Agent 入口为
`10.1.3.100:9444`。A 新增的三个 supervisord 程序为 `rlark-raw-supervisor`、
`rlark-container-gateway` 和 `rlark-container-agent`，仅对这些名称执行定向
`status`、`start`、`stop` 或 `restart`。重启 Agent 时不能停止受信任的 Raw supervisor。
Notebook 实例替换仍须确认旧实例退役并重新接入。

本次清理删除了原生 Node `docker-local-test`、对应本地
`rlark-docker-agent-test` Pod、Pending 的 `ksyun-gpu-smoke-20261031` Job，以及
旧 `ksyun-test-1` ContainerNode/原生 Node。三个真实集群和两个已有 Lumo2 Job
均保留，A 上没有创建 allocation。剩余容器节点为 `ksyun-a`。删除通过控制器完成
finalizer 清理，没有强制移除 finalizer。

本次验收不代表 Lumo2 推理、Ray 多机通信、Kubernetes Pod 网络或 S1U 连接已经打通。

## 2026-11-03 Ray 中继可行性验证

在开发机上使用隔离的 Ray 进程完成了双节点 RLinf/Ray 2.59.0 测试，双向
`Channel` 分别传输了 1 KiB、1 MiB 和 4 MiB 数据。测试使用 loopback 地址和
专用临时进程，没有设置或继承 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`；清理
仅针对本次测试创建的进程组。现有 Fleet Ray 作业仍在运行。这证明在 Ray 可使用
足够端口的条件下 RLinf Channel 能工作，但不代表跨主机或经过 A 中继已经通过验收。

当前 Docker Ray 执行器固定使用 9 个 Ray 服务端口，另加可配置的 worker 端口范围，
最少 1 个，默认 256 个。Ray 节点会直接连接多个分别监听的服务，因此 A 上 TCP
30000 的现有 Hub API 中继无法承载 Ray 流量；仅在 A 开放 3 个 TCP 端口也不满足
当前执行器的直连端口契约。单端口方案需要 Ray 感知的多路复用传输及每个节点上的
重定向或连接器；目前尚未作为 RLark 功能实现。方案不需要设置 HTTP 代理环境变量。

随后进行了隔离的跨主机 PoC：在 A:30001 启动双向 TLS `smux` 网关，在两台 Ray
节点运行仅作用于测试进程的 TCP 连接器。两节点成功加入同一个 Ray 集群，网关记录到
25 条双向流量，全部经过这一个公网监听端口，包含 Ray 服务端口、worker 端口和 Gloo
动态选择的端口。进程级适配器使用了 `LD_PRELOAD` 和临时凭据；这只证明传输层多路复用
可行，不是生产实现，也不代表 RLark 已支持该能力。

A 的 SSH 恢复后，通过在两台 Ray 节点上固定 Gloo 网卡完成了跨主机 RLinf `Channel`
验收。Head Actor 使用 `enp5s0`，地址为 `192.168.31.2`；Worker Actor 使用 `eth0`，地址为
`10.1.3.100`。启动 Ray 前，两端都把 `RLINF_COMM_NET_DEVICES` 和 `GLOO_SOCKET_IFNAME`
设为本机对应网卡；两个 Actor 均报告了预期 IPv4 地址。Gloo 双向初始化成功，1 KiB、
1 MiB、4 MiB 三种 payload 分别完成 10 轮 observation/action 请求响应。这解决了本次
测试环境中的 IPv4/IPv6 地址族不匹配。A:30001 网关记录 33 条流量、目标端口共 18 个，
跨主机测试流量全部通过这个单一公网监听端口。本次没有测试 Lumo2 推理或机器人连通性。

两端 Ray 均为 2.59.0，但开发机 Python 为 3.11.16，A 的 Fleet 环境 Python 为 3.11.14，
Ray 输出了 patch 版本不一致警告。PyTorch 版本也不同（`2.11.0+cu130` 和
`2.10.0+cu128`）。当前传输和 Channel 交换在此组合下通过；正式部署前应统一 Python 和
PyTorch 构建版本，不应把此次测试组合视为已支持的生产组合。

对已授权 A Notebook 的只读检查发现存在 `/dev/net/tun`，但没有 `CAP_NET_ADMIN`，
内核也未加载 WireGuard 模块。因此当前 Notebook 环境不能在 A 上创建基于 TUN 的
WireGuard 接口。如果后续平台授予所需能力，结论可以重新评估；当前安装不能宣称可
提供该 Overlay 网络。

三端口跨主机 Ray/RLinf 流量仍未验证，当前验收不支持该能力。单端口传输 PoC 已通过本次
隔离的 Ray/RLinf Channel 测试；未设置代理环境变量，也没有修改现有 Fleet 集群。该实现
仍是依赖 `LD_PRELOAD` 和临时凭据的进程级 PoC，不是 RLark 生产能力。

此前因 A:22 在 SSH key exchange 阶段重置连接而无法检查 Actor 网卡配置。主机重启后 SSH
恢复，测试已完成。所有测试 Ray 进程、连接器和 A:30001 网关均已停止，测试临时目录已
删除。A 上原有 Fleet Ray head（PID `592376`，端口 `6390`）仍运行。清理后 A:30001 已释放；
本次未操作 A:30000，但测试后检查时该端口没有监听，需在主机重启后另行检查对应服务。

2026-11-02 原生连通性验收通过：应用控制器 RBAC 并只重启扩展控制器后，`ksyun-a`
变为 `Online / AgentLeaseActive`，`ksyun-containers` 变为 `Online`，原生节点在线数为
1/1。仅暂停本地 Hub 后，两份 Lease 过期，原生 Node 和 Cluster 都转为 `Offline`，
Node 原因为 `AgentLeaseExpired`。启动 Hub 后恢复新鲜观测，两者自动回到 Online。
A 全程保持 `spec.unschedulable=true`，没有创建 allocation 或测试 Job。两节点的
Online → Downgraded → Offline → Online 聚合由控制器测试覆盖，不代表已完成云端多机验收。

## 原生连通性投影

独立控制器始终运行新增的连通性适配器。它响应附属资源变化，并每 15 秒按当前状态修复。
发布前会检查当前原生/附属对象 UID、租户、权威的 `Cluster.spec.agentNamespace`、运行时、
当前 generation 的校验和 Ready 条件，以及观测凭据是否仍为有效凭据。仅登记节点不会产生
心跳；忙闲、目标版本就绪及禁止调度与连接状态分别判断。

每次新鲜且经过认证的观测会维护 `node-heartbeat-<原生节点名>` 和该命名空间的
`heartbeat` Lease。两者有效期均为 45 秒，使用**原始服务端观测时间**，不会用适配器当前
时间给旧观测续命。Cluster Lease 使用有效成员最新的观测时间。核心控制器据此生成 Node
Online/Offline，以及 Cluster Online（全部已登记原生 Node 在线）、Downgraded（部分离线）
或 Offline（集群心跳过期）；Cluster 禁止调度/排空状态仍优先。状态收敛还取决于核心控制器
的调谐和 UI 刷新周期。

Lease 通过原生对象的 controller owner reference，以及记录精确附属对象 UID 的
`containers.rlark.io/native-heartbeat-projection` annotation 证明归属。适配器拒绝未知
Lease、被替换的原生 UID、歧义命名空间/绑定和外部 Raw 投影，不删除或接管外部资源。
Raw Node 只补写适配器拥有的 `status.raw` 与 `raw.rlark.io/credential-id` 兼容投影；
已存在且冲突的 Raw 能力须显式恢复。这不会签发原生 Raw 凭据，也不会授权旧工作负载执行。
凭据轮换后，必须收到新凭据认证的观测才会继续续租；容器实例替换仍需退役并重新登记，不能
修改旧实例绑定来接管新实例。

Agent/Hub/API 失联、身份失效、附属对象删除或适配器停机后，停止续租并保留 Lease 等待
自然过期，reservation 和正在运行的进程保留。下一次有效心跳恢复在线，但不会解除禁止调度。
该投影不需要 SSH 隧道或重装 Agent。重启控制器前先应用更新后的控制器 RBAC：除独立的选主
Role 外，新增 agent 命名空间的原生 Node metadata/status patch 和 heartbeat Lease 的
get/list/watch/create/update/patch 权限。主 UI 刷新即可看到状态。

```bash
kubectl --kubeconfig=/path/to/management.kubeconfig apply -f \
  extensions/container-runtime/config/rbac/container-runtime-controller.yaml
make -C extensions/container-runtime build
# 当前用户服务安装方式：
systemctl --user restart rlark-container-controller.service
kubectl --kubeconfig=/path/to/management.kubeconfig -n ksyun-container-agents \
  get leases.coordination.k8s.io
kubectl --kubeconfig=/path/to/management.kubeconfig -n ksyun-container-agents \
  get nodes.rlinf.io ksyun-a
kubectl --kubeconfig=/path/to/management.kubeconfig get clusters.rlinf.io ksyun-containers
```

## 目标与约束

管理员在 RLark 控制台管理独立的金山云容器集群。网关 A 通过 SSH 为明确授权的
B/C 实例安装节点 Agent；每个节点独立拥有执行、凭据与 GPU 分配。RLark 主动连接
A 的 TCP 30000，B/C 通过私网连接 A。SSH 用于安装和修复，不作为长期传输。A 也可
运行自身的 Raw Agent。当前云端 Notebook 没有 Docker socket 或 `CAP_NET_ADMIN`；
Raw 在已有 Notebook 中执行进程，不创建容器。Docker Engine 执行需要本地 socket。

部署前必须验证云平台上 30000 端口的实际可达性。根据当前状态确认公网端口映射、
TLS 名称、连通性、已有 Notebook 的生命周期和权限。不干扰已有任务，不擅自变更
网络配置，也不假设本地控制面私网 IP 可从云端访问。首次模型测试为 Passive，
不下发机器人动作。

## 接口与所有权

### 首个可运行通信组件：A/Hub 受限转发

扩展新增独立的 `container-runtime-gateway` 二进制，支持两种模式：A 上的
`gateway` 接收私网 Agent TCP 连接和 Hub 的双向 TLS 连接；RLark 控制面上的
`hub` 主动连接 A，并且只把数据转发到一个固定的本地扩展 API 地址。它是 Agent
管理 API 的传输组件，不是 VPN、Kubernetes 网络、SSH 隧道或任意 TCP 代理。

两端都必须使用 TLS 1.3、专用证书、对端 CA 和对端叶证书的精确小写 SHA-256
指纹。A 的 Agent 监听只能绑定明确的私网或回环地址；Hub 的 API 目标由启动
参数固定，Agent 请求不能修改目标。连接数和单向空闲超时有上限，Hub 断线后
使用有界退避自动重连。Agent 到扩展 API 的 mTLS 保持端到端，A 的凭据不能冒充
节点。Hub 固定目标必须是会验证节点身份的扩展 Agent mTLS API。

构建：

```bash
make -C extensions/container-runtime build
```

A 上的启动示例：

```bash
extensions/container-runtime/bin/container-runtime-gateway --mode=gateway \
  --agent-listen=10.1.3.100:9444 --hub-listen=0.0.0.0:30000 \
  --server-cert=/etc/rlark/gateway.crt --server-key=/etc/rlark/gateway.key \
  --roots=/etc/rlark/hub-ca.crt --peer-cert-sha256=REPLACE_HUB_LEAF_SHA256
```

RLark 控制面 Hub 示例：

```bash
extensions/container-runtime/bin/container-runtime-gateway --mode=hub \
  --gateway-address=REPLACE_A_PUBLIC_HOST:30000 \
  --server-name=gateway-a --api-address=127.0.0.1:9444 \
  --client-cert=/etc/rlark/hub.crt --client-key=/etc/rlark/hub.key \
  --roots=/etc/rlark/gateway-ca.crt --peer-cert-sha256=REPLACE_GATEWAY_LEAF_SHA256
```

30000 端口仅作为 TLS Hub 入口，必须由网络策略明确允许 RLark Hub 来源访问。
9444 是本次 A 部署使用的私网 Agent 入口，只允许 A/B/C 私网访问。这里的地址只是当前测试机只读
检查得到的示例；部署前要重新查询，因为平台分配地址和端口映射可能变化。B/C 上
Agent 使用的 HTTPS Endpoint 主机名必须能解析到 A 私网地址，且服务证书 SAN 必须
匹配该主机名。不能因为当前测试机有 SSH 连接就直接在那里启服务：本轮只读检查
发现该机没有 Docker socket、systemd、`CAP_NET_ADMIN`。随后短时启动监听探测已确认
TCP 30000 在有进程监听时可以从外部访问；不能把未启动监听时的拒绝当成平台阻断。

测试覆盖端到端节点 TLS、错误指纹、重连、空闲回收、有界配置和停机。这不能证明
Kubernetes Pod 网络、Ray 多机通信、Lumo2 推理或 S1U 机器人链路已打通。
证书轮换需要管理员更新 CA/指纹后重启对应进程；自动接入和续签尚不支持。

实现放在独立容器运行时扩展中，与核心代码的接线限制为小范围适配接口。新增
类型化的集群网关、节点接入意图及带身份的安装/退役操作。管理员选择 Cluster、
稳定 Notebook ID 或 SSH 地址、已固定的 SSH 主机身份、Secret 引用、显式运行时、
授权 GPU 和不可变 Agent 版本。期望状态、一次性操作与连接/节点观测分开存储。
API 写操作须在服务端验证管理员和租户。凭据及工作负载内容不写入事件、日志、
CRD 状态或 UI 响应。

网关和节点使用各自的节点级凭据。网关不是签发者或管理员，不能冒充其他节点。
节点 Agent 经 A 建立经过认证的连接；Hub 对每个操作验证当前 Cluster、Node、实例、
租户、凭据和 allocation 身份。网关不能提供无限制的网络代理；必须限制为已授权
Task 端点，并设置资源上限。自动证书接入、续签及动态地址恢复是待实现功能，不是
当前能力；现有 ContainerNode 的私网地址和实例 ID 不可变。实例替换时须确认旧
实例清理并重新登记，不得静默修改不可变绑定。

## 生命周期与验收

按持久操作身份安装：确认云端实例、获取当前地址、验证 SSH 主机密钥，使用指定
凭据连接，检查运行时与授权 GPU，然后在私有目录安装经过验证的版本。等待经过认证
的新鲜心跳；重试不得接管外部文件或进程。已有工作负载和结果不明确的操作保留
预留。主机支持 systemd 时使用 systemd；无 systemd 的 Notebook 使用显式管理的
前台启动器。A 本身须先独立安装，不能依赖尚不存在的网关自动安装自己。对于没有 systemd 的
Notebook，只能在 `/etc/kaic/services.d/` 新增唯一命名的配置，并通过现有
supervisord 的 `reread`/`add` 操作管理；不能重启无关的 Notebook 程序。

复用已有的可选空闲滚动升级和分配隔离：验证产物和清理完成后才进入下一节点。
退役时阻止新任务，等待精确清理，撤销节点凭据，只删除可证明属于本扩展的资源。
节点离线或清理结果不明时保持阻塞，不删除云端 Notebook 或其他工作负载。

UI/API 分别展示网关连接、Agent 心跳、运行时能力、可调度和网络就绪。测试覆盖
未授权访问、过期凭据、实例替换、响应丢失、竞争、重启、断连、安装中断、GPU
授权、删除和恢复；声明支持前还须验收公网 30000 和真实云端节点接入。

第一个数据面里程碑是明确授权、有界的 TCP 服务转发，用于云端 Lumo2 推理和 S1U
Passive 观测。Agent 心跳不证明该链路可用。当前 Raw 不是 RLark Domain 端点，
Fleet 交互 Env 也不会自动变成 WebSocket 客户端；必须先实现并验证适配器、端点
身份和双向通路，才能声称云端到真机已打通。多节点 Ray/RLinf 和完整 Pod 网络
互通属于后续独立验收。指定 30000 步 checkpoint 必须核对自己的 processor、BAT、
归一化和模型接口；Passive 测试不授权 Active 机器人运动。
