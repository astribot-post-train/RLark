# 容器网关与托管接入计划

状态：部分实现。独立扩展已经新增可选的 A/Hub mTLS 固定 Agent API 转发组件及
测试。SSH 托管安装、UI 接入/退役操作和云端到机器人的数据通路仍待实现或验收，
本计划尚未全部完成。

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
  --agent-listen=10.1.3.100:30001 --hub-listen=0.0.0.0:30000 \
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
30001 是私网 Agent 入口，只允许 A/B/C 私网访问。这里的地址只是当前测试机只读
检查得到的示例；部署前要重新查询，因为平台分配地址和端口映射可能变化。B/C 上
Agent 使用的 HTTPS Endpoint 主机名必须能解析到 A 私网地址，且服务证书 SAN 必须
匹配该主机名。不能因为当前测试机有 SSH 连接就直接在那里启服务：本轮只读检查
发现该机没有 Docker socket、systemd、`CAP_NET_ADMIN`，30000/30001 也没有可用入口。

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
前台启动器。A 本身须先独立安装，不能依赖尚不存在的网关自动安装自己。

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
