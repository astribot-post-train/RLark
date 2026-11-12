# Station v2：场站节点和自动入网（2026-11-12）

分支 `feat/station-enrollment`（基于 `feat/optimization` 1deb4ef）。上线步骤见 [station-v2-rollout.zh-CN.md](station-v2-rollout.zh-CN.md)。

## 0. 结论

1. **Station 这个名字只留给一个 Run 里的 station**（论文和 Fleet 的定义）。RLark 这边的对象改叫**场站节点（Site Node）**：一台机器在场站里的长期身份和入网记录，可选带上它代表的机器人和工位。两者是引用关系：Run 的 station 引用场站节点。
2. **节点按身份自动入网**：节点自己生成密钥，agent 用已有的 Pod 绑定 token 把公钥登记给 station-api，station-controller 从声明的拓扑分配地址、算出每台节点的对端和路由，网关容器取回执行。加一台机器人不改任何一份配置。
3. **拓扑是"叶子拨枢纽，枢纽之间互联，场站枢纽转发叶子之间的流量"**。96 台机器人 + 工作站 + 3 台云节点是 387 条隧道，每台机器人 4 个对端；全互联是 4950 条，而且机器人各在自己的 Orin NAT 后面，全互联建不起来。
4. **专线通了以后同一套模型去掉 WireGuard**：`mode: Routed` 保留入网登记和地址表，不下发隧道配置。
5. **epoch 和设备控制权不进 station-api**。机器人安全不依赖中心服务在线。
6. 今天五台节点手写的配置，算出来的结果只多不少（单测逐条对过），所以切换不动内核里的会话，运行中的 Job 不断。

## 1. 概念和名字

**结论：Station = Run 的 station，归 Fleet；RLark 的对象叫场站节点，归 RLark；界面、新接口和新 CRD 现在就按这个名字，已存的 CRD 组名以后迁移。**

### 1.1 两个对象

| | Run 的 Station（Fleet） | 场站节点 Site Node（RLark） |
|---|---|---|
| 是什么 | worker 组里的一个固定位置，绑定一台节点、它的设备，环境 worker 还绑定一台机器人 | 一台机器在场站里的长期身份 + 入网记录 |
| 寿命 | 一个 Run | 机器在场站里的整个时间，跨 Run、跨重装 |
| 基数 | 一台 8 卡节点上，一个 Run 可以有多个 station（训练一个、每条推理线一个） | 一台机器一个 |
| 状态 | 哪个 worker 占着、epoch、准入 / 退役 / 重新准入、旧回复隔离、设备租约 | 序号、Node UID 绑定和绑定版本、在线、地址、对端 |
| 时间尺度 | 设备租约 3 s，episode 级 | 心跳 30 s，90 s 判离线 |
| 谁写 | HotSwarm（Run 的 driver）和机器人本地的设备层 | station-api、station-controller、管理员 |
| 坏了的后果 | 机器人动作 | 新节点进不来；已建的隧道不受影响 |

### 1.2 为什么 Station 留给 Fleet

- 论文的定义就是 Run 的 station（"a fixed place in a worker group, bound to a placement"，存活期是一个训练 run）。Fleet 的代码、Dashboard、Run 文档、操作员用语都是这个意思。
- 基数不同。RLark 的对象一台机器一个；论文的 station 在一台 GPU 节点上有多个。把每台云节点也叫 Station，就没法再说"这台节点上的三个 station"。
- RLark 的对象覆盖所有机器（工作站、云节点、开发机），其中大多数不带机器人。"节点"是它本来的意思。
- 现在界面上的中文是"工作站"，和 Fleet 的工作站（场站那台控制面机器）又撞了一次。

"Station 归 RLark"（11-12 的约定）指的是这套身份 + 网络组件归 RLark 维护，这一点不变；改的只是它叫什么。

### 1.3 改名范围

| 范围 | 做法 | 状态 |
|---|---|---|
| 界面：导航、页面标题、表头、说明 | "Station 身份 / 工作站" → "场站节点"，"Stations" → "Site Nodes"，页面说明里写明它不是 Run 的 Station | 已改 |
| 给外部用的只读接口 | 新路径 `/api/v1/site-nodes/{node}`，字段 `site_node_id` | 已实现 |
| 新 CRD | `SiteNetwork`、`NetworkEnrollment`，名字里不带 Station | 已实现 |
| 文档 | 本文、两份 README 和 API 契约开头的说明 | 已改 |
| CRD 组 `station.rlark.io`、kind `Station*`、可执行文件 `station-*`、namespace `station-system`、标签 `station.rlark.io/network`、Gateway 路径 `/api/v1/station/*` | 不动：改了就丢已存对象、断他的界面和三个集群的 DaemonSet。作为代号保留到 API 升 v1beta1 时一起迁 | 迁移方案见下 |

迁移方案（满血版之前做一次，约半天，不影响隧道）：

1. 新组 `site.rlark.io/v1beta1`，kind `SiteNode`、`SiteNodeObservation`、`SiteNodeRebind`、`SiteNodeAuditEvent`、`SiteNodeSequence`、`SiteNetwork`、`NetworkEnrollment`，字段不变。
2. controller 和 station-api 双读单写：读两个组，只写新组；一次性任务把旧对象按原名、原序号、原绑定版本拷到新组（`metadata.name` 是确定性 ID，不变；`metadata.uid` 会变，所以 `NetworkEnrollment.spec.stationUID` 和地址分配表的键在同一个任务里换成新 UID）。
3. Gateway 加 `/api/v1/site/*`，旧路径保留一个版本；界面切到新路径。
4. 可执行文件和镜像改名（`site-api`、`site-agent`、`site-gateway`），DaemonSet 换镜像；节点标签加新键 `site.rlark.io/network`，DaemonSet 同时认两个键，再去掉旧键。
5. 删旧组 CRD。回退点：第 2 步之前任何时候删新组即可。

### 1.4 Fleet 需要的唯一接口

`GET /api/v1/site-nodes/{nodeName}?cluster_id={cluster}`，只读，登录用户可用。

```json
{
  "cluster_id": "rlark-robot-s1u-01", "node_name": "s1u-01-x86",
  "site_node_id": "stn-…", "sequence": 3, "binding_version": 1,
  "phase": "Bound", "online": true, "last_seen_time": "2026-11-12T08:00:00Z",
  "network": {"state": "Enrolled", "site": "lab", "role": "Leaf",
              "address": "10.254.240.3", "tunnel_address": "10.254.240.3"},
  "robot_id": "s1u-01", "workstation_id": ""
}
```

- `site_node_id` 重装不变；`binding_version` 在管理员重绑后加一，缓存了旧值的一方据此知道机器换过。
- `network.address` 是"别的节点用哪个地址找它"的唯一答案：有 `nodeAddress` 的节点（云上的 VPC 地址）是它，其余是隧道地址，Routed 模式是节点自己的地址。Ray 的节点地址就用它。
- `network` 只在节点按当前绑定入网后出现：重绑以后旧地址不会再被返回。
- `robot_id` 取节点标签 `rlark.io/robot-id`（s1u-01 已有），`workstation_id` 取 `rlark.io/workstation-id`。
- 接口不含这台机器上在跑什么。Run、worker、epoch 它都不知道。

Fleet 的用法：Run 的 station 配置只写 `cluster` + `node`（以及 Fleet 自己的工位、任务），地址、在线状态、机器人编号在 Run 开始和每次准入时从这里读；`binding_version` 记进 Run 文档，准入时和当前值不一致就拒绝准入并提示。

### 1.5 不进 station-api 的东西

**epoch 的发放、设备控制权的闸门和 3 s 租约、准入 / 退役、旧回复隔离，都不进 station-api，留在 HotSwarm 和机器人本地的设备层。**

- 安全不能依赖中心服务。station-api 是单副本，跑在一台开发机上，过渡阶段云上和机器人到它要经过隧道。它停了，后果必须是"新机器进不来"，不能是"机器人停不下来"或"旧 worker 的动作被执行"。设备租约由机器人本地判断：3 s 收不到续约就自己放手，不问任何人。
- 时间尺度差一个量级：这里心跳 30 s、90 s 判离线；租约 3 s。把后者放进前者的服务，要么租约变慢，要么这个服务要按控制回路的要求来建。
- epoch 属于 Run。Run 结束它就没有意义，Run 之间也不可比；放进长期对象里，就多了一个要和 Run 文档对账的真相源。
- 它是算法的一部分：什么时候退役一个 worker、重新准入后从哪个 episode 接，是 Runner 的决定。

他文档里列为"以后做"的"Worker 租约、epoch、旧指令隔离"：**不做**（已由 Fleet 的 `fleet/swarm` 和设备层实现）。

### 1.6 机器人和工位

- 机器人编号是机器的固定属性：管理员在节点上打标签 `rlark.io/robot-id`，接口原样返回。**沿用。**
- 工位号是 Fleet 的观测（机器人开机读桌上的二维码），机器人推到另一张桌子它就变。RLark 只做显示：满血版由 agent 读设备层写在节点上的一个文件，随心跳上报；过渡阶段**不做**，接口字段先留着，取标签。

## 2. 入网

**结论：节点的密钥、地址、对端全部由"身份 + 声明的拓扑"推出来，管理员只声明拓扑和给节点打标签；网关在 station-api 不可达时沿用最后一次的配置。**

### 2.1 现状和代价

每台节点的隧道配置是所在集群 ConfigMap `station-wireguard-config` 里的 `<节点>.json`，手写。加一台节点：

1. 在节点上建密钥目录，打标签，写一份不带对端的配置，等 pod 起来；
2. 从 pod 日志里抄公钥；
3. 在每个要和它通的节点的配置里加对端（3090 入网改了 5 份，分布在 3 个集群的 3 个 ConfigMap），再把所有对端的公钥抄回它自己的配置。

代价：N 台全互联时加一台要改 N 份；改错一个字段，那台节点的协调整个停住；网关不删路由，去掉一个对端要到节点上手工删路由，否则协调也停住；每次改对端，网关整组替换内核里的 peer，那台节点所有会话重新握手；3090 的地址是 DHCP 的，写死在机器人的配置里，一变就断。

### 2.2 新流程

```
节点（pod station-agent-network）                    管理面
┌ wireguard 容器 ┐  ┌ station-agent 容器 ┐
生成私钥（只在节点上）
写 identity.json（公钥） ──► 读公钥
                          POST /v1/network/enrollments ──► station-api：TokenReview 核对 Pod→Node→Station 绑定
                          （Pod 绑定 token）                 写 NetworkEnrollment.spec（公钥、节点自报地址、看到的源地址）
                                                           station-controller：读 SiteNetwork，分配地址，
                                                           算全网每台的对端和路由，写各 NetworkEnrollment.status
                          ◄── 200 {state, config, hash} ── station-api 返回这次请求对应的 status
                          校验 hash，原子写 config.json
读 config.json，逐个对端更新内核
另存 last-applied.json（和私钥同目录）
```

- 身份沿用已有的那条链：Pod 绑定的 ServiceAccount token → 成员集群 TokenReview → Pod → Node → 当前 Station 绑定。入网请求必须对上已绑定的 Station（否则 409，agent 下个周期再试；首个心跳会让 controller 建出 Station）。公钥不是凭证，谁的请求由 token 决定。
- agent 每 30 s 在心跳之后登记一次，同时取回当前配置。别的节点加入、地址变化，最迟一个周期加网关的 10 s 协调后生效。
- 网关的配置来源按顺序取第一个能解析的：算出来的 `config.json` → 本机上次执行过的 `last-applied.json` → 管理员的静态文件（ConfigMap，可以没有）。
- 对端逐个增删改，不再整组替换：加一台机器人，别的会话不动。加了 `--prune-routes` 后网关自己删掉不再需要的路由（只删自己建的接口上、带自己标记的）。

### 2.3 拓扑模型

管理员写一个 `SiteNetwork`（示例：`extensions/station/deploy/site-network.live-20261112.yaml`，就是今天的场站）：

- `pool`：隧道地址池。地址按 Station 身份分配，重绑不变，身份退役才收回；已有的五台用 `address` 钉住。
- `sites`：场站列表，每个场站写它包含哪些成员集群；`reaches` 写它能主动连到哪些场站的内网（实验室能连办公网，反过来不行）；`mesh` 写本场站的枢纽之间要不要走隧道（办公室要，云上两台走 VPC 不要）。
- `nodes`：只列"不只是叶子"的节点。没列的节点都是叶子，什么都不用写。
  - `role: Hub`：有别人能拨的端点。端点分 `Public`（谁都能拨，云节点的公网入口）和 `Site`（本场站和 `reaches` 到它的场站能拨）；`Site` 端点不写地址就用节点自报的地址（3090 的 DHCP 地址由此自动跟随）。
  - `relay: true`：这台枢纽转发拨它的叶子之间的流量（场站工作站）。
  - `nodeAddress`：别人用来找它的地址不是隧道地址时写（云节点的 VPC 地址）。
  - `advertise`：经它可达的其他网段（NAS、Pod 网段），可以限定给哪些场站。
  - `uplink`：它拨出去要走指定网关和源地址时写（机器人经 Orin）。

连接规则，四条：

1. 两台之间有隧道，当且仅当至少一台是枢纽，且至少一方能拨到另一方。叶子之间没有隧道。
2. 同一场站的两台枢纽只在 `mesh: true` 时建隧道。
3. 能拨的一方写对端端点和 keepalive 25 s；被拨的一方不写端点，等对方拨入。
4. 每个对端的 AllowedIPs = 它的隧道地址 + 它的 `nodeAddress` 和 `advertise`（只给别的场站）；转发枢纽对拨它的叶子再加整个地址池。

controller 每次算完都沿着算出来的配置检查任意两台之间有没有路，没有的列在 `SiteNetwork.status.unreachable`，条件 `FullyConnected` 置假。"Ray 要求任意两节点互通"由此变成一个看得见的状态。会把自己拨的端点路由进隧道的网段、两个对端都声明的网段，算的时候丢掉并记在该节点的 `status.problems`。

### 2.4 为什么不是全互联，也不是单一中心

96 台机器人（各在自己的 Orin NAT 后面）+ 场站工作站（转发枢纽）+ 3 台云节点（公网端点），数字来自 `extensions/station/network` 的单测：

| | 全互联 | 单一中心 | 本方案 |
|---|---|---|---|
| 隧道数 | 4950 | 99 | 387 |
| 每台机器人的对端 | 99 | 1 | 4 |
| 每台机器人的配置 | 约 8 KB | 约 0.3 KB | 1.0 KB |
| 最大的一份配置 | 约 8 KB × 100 台 | 8 KB（中心） | 8.1 KB（工作站，99 个对端） |
| 加一台机器人，别人改几份 | 99 | 1 | 4（都是枢纽，叶子一份不改） |
| 机器人 ↔ 云 的路径 | 直连 | 绕中心，多一次解密加密 | 直连 |
| 机器人 ↔ 机器人 | 建不起来：两边都在 NAT 后 | 经中心 | 经工作站 |
| 中心停了 | — | 全断 | 只断机器人之间；机器人 ↔ 云不受影响 |

- 全互联在这里不成立：机器人之间没有任何一方可拨。而且同一集群 96 份 8 KB 的配置放在一个 ConfigMap 里约 770 KB，贴着 1 MiB 的上限。
- 单一中心让所有机器人 → 云的图像流在工作站上多转一次，工作站成为全场的单点。
- 本方案的保活和握手负载可以忽略：每台机器人 4 个对端 × 每 25 s 一个保活包 = 0.16 包/秒；每台云节点收 96 ÷ 25 ≈ 3.8 包/秒；会话每 2 分钟重新握手一次，全场 387 ÷ 120 ≈ 3.2 次/秒。算一次全网配置 28 ms。
- MTU 统一 1380：1500 − 60（IPv4 20 + UDP 8 + WireGuard 32）= 1440 是上限，留 60 字节给运营商侧的封装。转发枢纽是解密后再加密，不嵌套，所以经不经转发都是同一个值。

转发枢纽要宿主机放行 `st-wg0 → st-wg0` 的转发（开发机的 docker 把 FORWARD 默认策略设成了 DROP）。网关不改宿主机防火墙，这一条在上线步骤里由管理员加。

### 2.5 引导：到不了 station-api 的节点

**规则：不经隧道就能连到 station-api 的节点零配置入网；连不到的节点留一份只有一个对端的静态文件。**

- 办公室、实验室、机器人：直接连 station-api 的内网地址，零配置。96 台机器人都属于这一类。
- 云节点（过渡阶段）：办公室出口只出不进，云上连不到 station-api。每台云节点做两件事，各一次：在 `SiteNetwork.spec.nodes` 的声明里写上它的公钥（`publicKey`，从 pod 日志抄一次）和钉住的地址，别的节点据此先认得它；在 `station-wireguard-config` 里留一份只写场站入口一个对端的静态文件。隧道起来以后它经隧道登记，之后用算出来的配置和它自己登记的公钥。一共 3 台。
- 专线通了以后云节点也能直连 station-api，静态文件不再需要。

### 2.6 轮换、重装、移除、审批

| 事情 | 怎么发生 | 管理员做什么 |
|---|---|---|
| 新叶子入网 | 打标签后 pod 起来，自动登记、分配地址、下发 | 给节点打 `station.rlark.io/network=enabled`（这就是审批） |
| 新枢纽、新端点、新网段、转发 | 在 `SiteNetwork.spec.nodes` 声明 | 审：这些会改别的节点的路由，节点自己报不了 |
| 新场站、新成员集群 | 在 `SiteNetwork.spec.sites` 声明；集群注册沿用现有流程 | 审 |
| 密钥轮换 | 删节点上的 `private.key`，重启 pod；新公钥用同一身份登记，controller 重算，各对端最迟约 40 s 换成新公钥 | 不用审：能删这个文件的人本来就拿得到旧私钥 |
| 节点重装（Node UID 变了） | Station 进 Conflict，入网请求认证不过，全网把它的旧公钥去掉；管理员重绑后新公钥按新的绑定版本入网，**地址不变** | 重绑（已有流程） |
| 移除 | Station 退役：全网去掉它，地址回池 | 退役 |
| 只是离线 | 什么都不变：离线的节点留在别人的对端表里，回来就能连 | — |

### 2.7 各部件停了会怎样

| 停的是 | 后果 |
|---|---|
| station-api | 已有隧道不变（内核状态 + `last-applied.json`，节点重启也能恢复）；新节点进不来，地址变化不传播 |
| station-controller | 同上；入网请求一直是 pending |
| 某台节点的 agent | 它的隧道不变；它拿不到别人的变化 |
| 某台节点的网关容器 | 内核里的接口和会话还在；不再协调 |
| 场站转发枢纽 | 机器人之间不通；机器人 ↔ 云、机器人 ↔ 其他枢纽不受影响 |

### 2.8 专线通了以后

`SiteNetwork.spec.mode: Routed`：节点照常登记（不带公钥），controller 不分配隧道地址、不算对端，`NetworkEnrollment.status.address` 直接是节点的可路由地址（声明的 `nodeAddress`，没有就取节点自报的地址）；DaemonSet 去掉 wireguard 容器。场站节点、在线状态、`/site-nodes` 接口、Fleet 的用法都不变，WireGuard 整个拿掉。

机器人这边专线不解决的一件事：每台 x86 在自己的 Orin 后面，内网地址都是 `192.168.0.10`。Routed 模式要求每台 x86 在机器人 VLAN 上有一个唯一、可路由、绑在 x86 本机的地址（s1u-01 今天的 `192.168.31.250` 别名 + Orin 代答 ARP 就是这种做法）。这是机器人镜像和场站交换机的事，登记表只负责把这个地址记下来给别人查。

## 3. 迁移和回退

**结论：分四步，每一步都能单独回退，前两步不改任何节点的行为；切换靠"算出来的只多不少"保证不断。** 详细命令在上线文档。

1. 管理面：装两个新 CRD、新 RBAC，换 station-api 和 station-controller 的镜像，建 `SiteNetwork`。没有节点登记，什么都不发生。
2. 节点只登记不执行：换 agent 和网关镜像，网关发布公钥、agent 登记并把算出来的配置存在 pod 里，网关仍只读 ConfigMap。五台都登记后，用 `compare-config.py` 逐台对比"手写的"和"算出来的"，要求删除项为 0。
3. 逐个集群让网关读算出来的配置（先不开删路由）：内核里对端和已有网段不变，只增加几个网段，会话不重置。
4. 开删路由；ConfigMap 里办公室和机器人的条目可以删，云节点的缩成只剩一个对端的引导文件。

单测 `TestComputeCoversLiveSite` 拿 11-12 13:18 快照里的五份手写配置逐项核对：对端、端点、保活、网段、路由、上联路由一条不少。多出来的只有：

| 节点 | 多出来的 | 原因 |
|---|---|---|
| cailiang | 到两台云节点 VPC 地址的路由；对 3090 的端点和保活 | 云节点的 `nodeAddress` 对所有别的场站可见；两边都可拨时两边都拨 |
| 10-0-2-57 | 经 cailiang 到 NAS `10.11.5.12/32` | NAS 是给云场站的，原来只有 10-0-2-196 写了 |
| alexfu | 到云上两个 Pod 网段的路由 | Pod 网段是给办公场站的，原来只有 cailiang 写了 |
| s1u-01-x86、10-0-2-196 | 无 | — |

回退：第 3、4 步去掉网关的 `STATION_GATEWAY_COMPUTED_CONFIG`，网关回到 ConfigMap（开着删路由时它自己把多出来的路由删掉）；第 2 步 `kubectl rollout undo`；第 1 步换回旧镜像，新 CRD 留着无害。

## 4. 三个阶段

| | 现状（11-12） | 过渡（2–3 台机器人、云 GPU、公网 WireGuard） | 满血版（96 台、工作站、3 × 8 卡、专线） |
|---|---|---|---|
| 节点 | 5 台，3 个场站 | 工作站 + 2–3 台机器人 + 2–3 台云节点 + 开发机 | 约 100 台 |
| 名字 | 都叫 Station | 界面和新接口叫场站节点；CRD 组名仍是 `station.rlark.io` | 组名迁到 `site.rlark.io` |
| 入网 | 手改各集群 ConfigMap | 自动；云节点各留一份引导文件 | 自动，无引导文件 |
| 拓扑 | 手写的两两互联 | 叶子拨枢纽 + 工作站转发 | 无隧道（`mode: Routed`） |
| 地址 | 隧道地址手工分配 | 地址池分配，云节点用 VPC 地址 | 节点自己的可路由地址 |
| Fleet 怎么拿地址 | 写在 Fleet 配置里 | 查 `/site-nodes` | 同左 |
| station-api 入口 | 只在隧道地址上 | 内网地址 + 隧道地址 | 场站内网地址 |

liangcai 现有的件：

| 件 | 过渡 | 满血版 |
|---|---|---|
| Station 身份链路（TokenReview、Pod→Node→Station 绑定、序号、重绑、审计） | **沿用**，入网直接建在它上面 | 沿用 |
| station-api、station-controller、station-agent | **沿用**，各加一块 | 沿用，改名 |
| station-gateway（WireGuard 容器） | **沿用**，改为读算出来的配置、逐对端更新、可删路由 | **不做**（Routed 模式没有它） |
| `station-wireguard-config` 手写配置 | **临时**：只作云节点的引导文件和回退手段 | **不做** |
| 观测上报的 WireGuard 地址（`/api/v1/node-networks`） | **沿用** | 沿用（显示节点地址） |
| `/v1/challenges`、`/v1/sensitive/*` | **沿用**（入网没用它：入网请求本身幂等，不需要一次性凭据） | 沿用 |
| station-forwarder（NAS 转发）、WG 专用镜像仓库入口 | **临时** | **不做**（专线直达） |
| 计划中的"Worker 租约、epoch、旧指令隔离" | **不做**（Fleet 已有） | 不做 |
| 界面"Station 身份"页 | **沿用**，改名"场站节点" | 并进节点页的两列 |

## 5. 实现状态

已实现并有单测（`GOWORK=off go test ./...`，三个模块都过）：

| 件 | 位置 | 测了什么 |
|---|---|---|
| 对端计算 | `extensions/station/network` | 今天五台只多不少；96+1+3 的隧道数和配置大小；不可达对；无效成员跳过；会绕进隧道的网段丢弃；地址分配 |
| CRD `SiteNetwork`、`NetworkEnrollment` | `extensions/station/api/v1alpha1/network_types.go`，CRD 用仓库固定的 controller-gen v0.18.0 生成 | — |
| 网络 controller | `extensions/station/controller/network_reconciler.go` | 入网、分配、第二台机器人加入时叶子配置不变、DHCP 端点跟随、重绑后旧公钥全网去掉且地址不变、退役回收地址、Routed 模式、声明了公钥的节点先于登记被对端认得、今天场站的声明文件 |
| 入网接口 | `apps/rlark/pkg/stationidentity/network_enrollment.go` | 必须是已绑定的已验证身份；公钥和地址格式；换公钥后不返回旧公钥的答案；别的集群不能接管 |
| agent 登记 | `apps/rlark/pkg/stationidentity/agent_enrollment.go` | 登记、校验 hash、原子写；pending / 拒绝 / 出错都保留上一份 |
| 网关 | `extensions/station-gateway` | 配置来源的先后；对端增删改是增量；真实内核（一次性网络命名空间，含他原有的两个内核测试）上加入、离开、删路由 |
| 两个模块之间的文件格式 | `extensions/station-gateway/testdata/computed-s1u-01-x86.json` | 计算侧必须产出它，网关侧必须能解析它 |
| 只读接口 | `apps/rlark/pkg/gateway/site_node_handler.go` | 身份 + 网络 + 机器人标签；重绑后不返回旧地址 |
| 界面改名 | `apps/rlark-ui` | 原有 246 个界面测试通过 |

只设计、没实现：

- 转发枢纽的防火墙放行（上线步骤里手工加一条规则）。
- 工位号上报。
- 按场站分别设 Tunnel / Routed（现在是整张网一个模式）。
- 密钥轮换的审计事件。
- CRD 组改名的迁移（1.3）。
- 入网状态在界面上的显示（现在用 `kubectl get netenroll -A` 和 `kubectl get sitenet`）。

没有在真实集群上跑过：没有碰任何在线的集群、ConfigMap、DaemonSet。

## 6. 动了哪些上游文件

分支相对 `feat/optimization` 只碰两个上游已有的文件，其余都在他新增的文件和目录里（`extensions/station*`、`apps/rlark/pkg/stationidentity`、`cmd/station-*`、网关的 `station_*.go` / `site_node_*.go`、界面的 Stations 页）：

| 文件 | 行数 | 内容 |
|---|---|---|
| `apps/rlark/pkg/gateway/router.go` | +1 | 注册 `/site-nodes/:name` |
| `apps/rlark/docs/api/swagger.yaml` | +46 | 这条路由的说明（`TestRegisteredRoutesAreDocumented` 要求） |

界面的 `constants.ts`、`i18n.ts` 各改 1–2 行文案（上游文件里他加的那几行）。
