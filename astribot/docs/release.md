# 构建和上线

发布在办公室管理机（10.11.14.128，ssh 别名 `cailiang_rlark`，用 sudo）上构建和上线，不经 CI。

## 文件

| 文件 | 是什么 |
|---|---|
| `astribot/release/build.sh` | 在工作站执行：把提交的树（`git archive`）送到管理机 `/var/lib/rlark/release/builds/<名字>/`，由树里的 `run.sh` 构建；构建完把发布文件拷回 `releases/` |
| `astribot/release/run.sh` | 在管理机上构建（见下） |
| `astribot/release/images` | 要构建的镜像：仓库、Dockerfile、它是发布里的哪些组件、BuildKit 参数 |
| `astribot/release/mirror` | 构建要用的公网底座镜像，按摘要拷进办公室仓库的 `mirror/…` |
| `astribot/release/pinned.yaml` | 不由本树构建的镜像（按摘要）和各集群都一样的 Addon 取值；`installer` 列出打进安装包的镜像 |
| `astribot/release/releases/<名字>.yaml` | 一个发布：`source`（RLark 和 Fleet 的提交）、`images`（组件 → 摘要）、`addons`（版本、取值、`after` 顺序） |
| `astribot/release/environments/live.yaml` | 现网：用哪个发布、管理面身份、控制面 compose 的位置和取值、Station 所在集群、安装器底座、两个仓库、运行记录目录 |
| `apps/rlark/astribot/release/stages.yaml` | 上线的阶段、顺序、检查 |
| `astribot/release/control-plane/rlark.compose.yaml` | 控制面四个服务 |
| `astribot/release/member-rbac.yaml` | 每个成员集群里发布和只读账号的 RBAC |

## 整套构建

```bash
astribot/release/build.sh <RLark 提交> <Fleet 提交（40 位）>   # 发布 ci-<r7>-<f7>
astribot/release/build.sh base <RLark 提交>                      # 安装器底座（换 Kubernetes 版本时）
```

`run.sh release` 依次：
1. 缺的底座镜像拷进办公室仓库 `mirror/…`；
2. 跑测试（`Dockerfile.test` 和 `s1u-robot` 的 test target），按 `images` 构建并推进 `127.0.0.1:5443`；
3. 从 rlark-server 镜像里取出这次的 `rlarkctl`；
4. 安装包和升级包（`installer-live`、`rlark/node-bootstrap`），底座是 `live.yaml` 的 `installer.base`；
5. Fleet 的镜像（`fleet-robot`、`fleet-run-gpu`）从金山云仓库按摘要拷进办公室仓库；Fleet 提交和上一发布相同就沿用；
6. `rlarkctl release candidate` 写发布文件；
7. 这次构建的镜像按摘要拷进金山云仓库（`rlarkctl release copy --to source`）。

- 同一时间只有一个构建（锁 `/var/lib/rlark/release/builds/lock`）。日志 `<构建目录>/build.log`，每步还有自己的 `*.log`。
- 构建在 BuildKit 容器里，默认 6 GB 内存、4 核、并行 2；内存不够时等最多 120 分钟。`BUILD_MEMORY`、`BUILD_RESERVE`、`BUILD_CPUS`、`BUILD_PARALLELISM`、`BUILD_WAIT` 可改。
- 镜像里的时间戳都是 0：内容不变，镜像摘要就不变。
- 构建时间长，在 tmux 里跑。

## 快速上线（只换改了的镜像）

改动只涉及一两个程序时，不跑整套构建。改动碰到 `astribot/station/bootstrap/` 或 `pinned.yaml` 的 `installer` 时不适用：`installer-live` 只有整套构建会重做，快速上线沿用上一个发布的安装包。

前两步在工作站，其余在管理机上以 root（发布身份的 kubeconfig 在 `/home/cailiang/…` 下，见 `live.yaml`）：

1. 跑全量测试（[README](../README.md) 的命令）。
2. 只构建改了源码的镜像，推进管理机的 `127.0.0.1:5443`，记下摘要。一个程序的改动可以把新编译的二进制叠在现网镜像上（`FROM <现网镜像>@sha256:…` 加一行 `COPY`）。注意 `rlark/agent` 一个镜像是六个组件（见 `images`），只给改了的组件换摘要。
3. 树送到管理机并解开（发布文件的 `source.rlark` 必须是这个提交，`apply` 会核对）：
   ```bash
   W=/var/lib/rlark/release/builds/<名字>
   ssh cailiang_rlark sudo mkdir -p $W/rlark
   git archive <提交> | ssh cailiang_rlark sudo tee $W/rlark.tar >/dev/null
   ssh cailiang_rlark sudo tar -C $W/rlark -xf $W/rlark.tar
   ```
4. 按这个提交编译 `rlarkctl`（`cd apps/rlark && go build -o rlarkctl ./cmd/rlarkctl`），拷到 `$W/rlarkctl`。Addon 目录编在 `rlarkctl` 里，用旧的会发布旧的 Addon 版本或报某版本没发布过。
5. 在管理机 `$W/rlark/astribot/release` 下，从上一个发布生成新的：
   ```bash
   $W/rlarkctl release candidate --base releases/<上一个>.yaml --tree $W/rlark.tar --name ci-<r7>-<f7> \
     --fleet <Fleet 提交> --image <组件>=registry.rlark.internal:5443/<仓库>@sha256:<摘要> ...
   $W/rlarkctl release copy -e environments/live.yaml --to source <新镜像>...
   ```
   没给 `--image` 的组件（包括 `installer-live`）沿用上一个发布的镜像；Addon 版本取树里的。
6. 同一目录照常 `apply`（下一节），发布文件拷回工作站提交。

## 要和 Fleet 一起上线的改动

`s1u-robot` 的 `bodies.json`（每种机体有哪几路相机）是相机服务和 Fleet 设备层共用的清单：设备层发现自己 `configs/robots` 的相机和它不一样就拒绝启动。所以改了相机清单或相机接口的 `s1u-robot` 版本，必须和对应的 Fleet 设备层镜像（`deviceImage`）在同一次 `apply` 里上线，先上哪一边，另一边都会起不来。

- `s1u-robot` v1.7.0（相机归 RLark）：`bodies.json` 加了 `torso`，`video` 容器换成相机服务（端口 18891），`/services/s1u-video.sock`、`/viewers`、WebRTC 和 `orin` 读的 `/run/rlark/s1u/camera-reset-request` 都没有了。它要的 Fleet 是相机那次改动之后的 main：`torso` 是录制相机，设备层不再重启相机驱动、不再调 `camera_control_command`，Dashboard 经 Gateway 看相机。发布文件里 `deviceImage` 换成按那个 Fleet 提交构建的镜像、`--fleet` 写那个提交，和这个 Addon 版本一起 `apply`。Fleet main 还在 50d1eb37 时不要上这个版本。
- `s1u-robot` v1.7.1（相机恢复按相机走）：每路相机各自的恢复阶梯；USB 复位后 5 分钟内又停即疑似硬件、直接 stuck；stuck 的相机只在机器人空闲（`/run/rlark/in-use` 为空）时每小时重启一次驱动，`video` 容器因此只读挂载 `/run/rlark/in-use`；stuck 带 `orin` 采的诊断，每路相机带 USB 位置和改接建议。清单变了（新挂载），所以 `rlarkctl` 和 agent 镜像要按这个提交重建（Addon 目录编译在里面）。不依赖 Fleet 的改动。

- `s1u-robot` v1.7.2（Run 里重启、时钟）：电源容器执行认领者经 Gateway `POST nodes/{name}/robot-restart` 要的重启，只在每个在用标记都写着该 Run（`... Run <id> ...`）时做，所以要和在用标记写明 Run 的 Fleet 设备层（`deviceImage`）同一次 `apply`，旧设备层的标记会让重启以“in use by others”失败（不动机器人）。`clock` 容器只读挂载 `/run/rlark/in-use`，只在驱动停、没认领、没在用时跳；`timeSource` 由 `pinned.yaml` 带来（`time.rlark.internal`，每个站点在 SiteNetwork `names` 里指到本站 Hub；`clock` 容器挂机器自己的 `/etc/hosts`，Hub 换地址只改 `names`，不用重启 Pod）。新的 Gateway 和 agent（`rlark/agent`）同一次上线：agent 把管理面的 `rlark.io/robot-restart` 和 Station 标记 `rlark.io/station-mark-set` 带到成员集群的节点上（机器人灯的停用/维修从这里读）。重启在设备层还有会话时（`s1u-device` 标记在）失败，Fleet Runner 先排空会话再请求。

- `s1u-robot` v1.7.3 提供本体快照与相机采集时间戳。与包含本体接口的 Gateway 和控制台配套发布。旧 Addon 仍可提供相机画面，页面在缺少采集时间戳时将端到端延迟显示为「—」。正式发布保持现有按空闲逐台替换策略，首台验证关节、末端位姿、两路相机、FPS 和延迟后再继续观察其余节点。

- `s1u-robot` v1.7.4（发件箱；v1.7.3 这个版本号 11-26 14:55 已被一次没进仓库的构建发布到机器人集群，内容和 v1.7.2 相同，一个版本号只有一份内容，所以跳过）：多一个 `episodes` 容器（`deviceImage` 的 `/device/start --episodes`，Fleet 952cd5e9 起才有这个入口），所以 `deviceImage` 要换成那之后的 Fleet 镜像，同一次 `apply`；场站的 `fleet-episode-relay`（下一节）也在这次里装上，机器人的发件箱把 episode 发给它。Pod 的内存上限合计 5888 MiB，正好是节点规格给 Pod 的 6144 MiB 减去留给集群自己 Pod 的 256 MiB：`episodes` 的上限因此是 832 MiB（编码峰值 0.55 GiB）；再给它加内存要先提高节点规格的 `S1U_KUBERNETES_MEMORY_MIB`（每台机器人重启一次 kubelet）。

## 站点自己装的 Addon（`live.yaml` 的 `addons`）

发布只更新一个集群里已经装了的 Addon。要由发布在某个集群里装上、并一直按站点的取值保持的 Addon，写在环境文件里：

```yaml
addons:
  <集群>:
    <Addon>: {<参数>: <站点的取值>, ...}
```

发布里有这个 Addon（`releases/<名字>.yaml` 的 `addons`，带版本和随发布走的取值，比如镜像）时，`addons` 阶段在这个集群里装上它（计划里一行 `<集群>/<Addon>: install <版本>`），之后每次运行把这里写的取值和发布的取值对到现网。控制台里装的 Addon 不用写在这里。

- `fleet-episode-relay`（场站 episode 中继，Fleet 的 `fleet.workers.relay_service`）：镜像是发布的 `fleet-robot`；站点的取值是所在节点（`nodeSelector`）、卷大小（`volumeSize`，建好后不能改）、拉镜像的账号。现在在办公室集群的管理机上（机器人经 `10.254.240.1:8030` 访问）；搬到场地缓存机时改 `nodeSelector`（和那台机器所在的集群），原节点上的卷和里面没转走的 episode 留在原地。

## 提高文档级别的改动（分两次 apply）

阶段 `controller` 的门 `gateways-understand: N` 在 `station-agents`（换网关）之前：新级别的 controller 要等所有网关先认得新文档。所以给文档加字段的提交（`documentLevel` 加一）分两次上线，两次用同一个提交（同一棵树、同一个 `rlarkctl`）：

1. 只换 `station-wireguard`（快速上线 `--image station-wireguard=…`），`station-controller` 沿用上一个发布：`controller` 阶段没有变化，门不查；`station-agents` 按树换掉各集群的 Station DaemonSet，网关报到新级别。
2. 所有有文档的成员的网关都报到新级别后（NetworkEnrollment 的 `status.observation.gateway.documentLevel`；离线的节点上线后会换），从第一次的发布再生成一个只多 `--image station-controller=…` 的发布，照常 `apply`。

一次带上两个镜像的发布会停在 `controller` 的门前，不改任何东西。

## 发布删掉的手装对象

`release.RetiredNamespaces`（`apps/rlark/astribot/release/site.go`）是站点曾在 Station 旁边手装、现在由 Station 自己做的东西所在的命名空间。带树的运行在 `station-agents` 阶段、换 Station DaemonSet 之前从每个成员集群删掉它们（计划里一行 `<集群> namespace <名字>: present -> (none)`；`--back` 不删也不恢复）；发布账号只对这几个名字有 `delete`（`astribot/release/member-rbac.yaml`）。不手工 `kubectl delete`。

- `rlark-station-system`：办公室集群里的 `rlark-station-nas-forwarder`（station-forwarder，`imagePullPolicy: Never` 的手装镜像，只放 `10.254.240.4` 到 NAS 的 445）。删掉 Pod 不动它装的 iptables 规则（它从不删自己的规则），所以在新的转发生效前 196 照常能挂 NAS；声明了 `forward` 之后，cailiang 的网关删掉这些带注释 `rlark-station-forwarder-v1` 的规则。机器上的 `/var/lib/rlark-station-forwarder/` 和容器运行时里的镜像 `docker.io/library/rlark-station-forwarder` 不再有人用，留着无害，节点重装时没了。

## 上线

在管理机上，以 root，在 `astribot/release/` 下：

```bash
rlarkctl release apply -e environments/live.yaml -r releases/<名字>.yaml --tree <rlark.tar> --dry-run
rlarkctl release apply -e environments/live.yaml -r releases/<名字>.yaml --tree <rlark.tar>
```

- `--dry-run` 只打印每个阶段会改什么。
- 不带 `-r` 用 `live.yaml` 的 `release:`。`--tree` 是发布那个提交的 `git archive`（CRD、RBAC、清单从这里取）。`--timeout` 是每阶段的等待（默认 10 分钟，乘 `timeoutFactor`）。
- 运行前把现状记在 `/var/lib/rlark/release/runs/<时间>-before.json`，日志在同目录 `<时间>-<发布>.log`。同时只有一个运行（kcp 里的 Lease）。
- 每个阶段改完就读回并跑检查，直到成立或超时；一个阶段不成立就停下，打印回退命令。原地再执行同一条命令会从停下处接着做（已经符合的阶段跳过）。
- 成功后写发布记录 `rlark-system/rlark-release-current`。
- 上线后提交 `releases/<名字>.yaml`，并把 `live.yaml` 的 `release:` 指向它。

### 13 个阶段

| 阶段 | 改什么 | 检查 |
|---|---|---|
| `images` | 发布里办公室仓库缺的镜像，从金山云仓库按摘要拷进来 | 逐个读回 |
| `prewarm` | 控制面的镜像预先拉到管理机 | `nodes-unpressured` |
| `crds-rbac` | `rlinf.io`、`station.rlark.io` 的 CRD；RLark 组件（含集群升级所需的 Gateway 和 Controller Manager 角色）和 Station 的管理面 RBAC；各成员集群的 Station RBAC | — |
| `gateway` | 控制面 gateway 容器（和 compose、env 文件） | `clusters-online`、`addons-ready` |
| `station-api` | Deployment station-api；安装器放到 Station API 的机器上 | `enrollments-enrolled`、`observations-fresh` |
| `controller` | Deployment station-controller（先等所有网关认得新文档级别） | `documents-unchanged`、`enrollments-enrolled`、`network-connected` |
| `cluster-agents` | 每个集群的 agent，一个集群一个集群来 | `clusters-online`、`addons-ready`、`nodes-ready`、`nodes-unpressured` |
| `node-agents` | 每个集群的节点 agent（Cluster `spec.nodeAgents.image`） | `clusters-online`、`nodes-ready`、`nodes-unpressured` |
| `station-agents` | 各成员集群已有的 Station DaemonSet 改成这棵树的 `station-agent.yaml` 写的样子（整个 `spec`，selector 除外），镜像和拉取账号在同一次写入里换成发布的，模板和它启动的二进制始终出自同一个提交。计划里逐个字段列出差异（和集群 API server 对这份清单 dry-run 的结果比，补的默认值不算差异）。DaemonSet 是 OnDelete，由集群 agent 按 `rlark.io/rollout` 换 Pod，这个阶段等它们换完。不带树的运行（`--back`、`--to`）只改镜像和拉取账号，模板其余部分不退回 | `observations-fresh`、`enrollments-enrolled`、`links-connected`、`documents-unchanged`、`network-connected`、`nodes-unpressured` |
| `server` | rlark-server、rlark-controller-manager 容器 | `clusters-online`、`nodes-ready` |
| `console` | rlark-ui 容器 | — |
| `addons` | 版本或取值和发布不同的 Addon，按 `after` 顺序 | `clusters-online`、`nodes-ready` |

- 一个阶段只允许它声明的 Pod（`recreates`）被重建；旁边有别的 Pod 新起，阶段会保守地停下。看清是无关的 Pod 后，再执行同一条命令。
- 换控制面的一个服务时，依赖它的服务也会重启（控制台的 nginx 记着 gateway 容器的地址）。
- 发布带的 Addon 跟着发布走；机器人上发布之外的取值（`powerOff`、`reboot` 等）保持站点设的。

## 上线后看什么

- 日志最后一行 `release <名字>: every stage done`。
- 控制面：`docker ps` 看四个容器的镜像；打开 `http://10.11.14.128/` 能登录。
- 各集群：`rlarkctl release kubectl -e environments/live.yaml --cluster <集群> -- get pods -A`，看 `rlark-system`、`station-system` 没有异常 Pod；节点都 Ready。这条命令用每个集群里的只读账号 `rlark-system/rlark-reader`（看得到工作负载、日志、事件，读不了 Secret），机器上不留管理员 kubeconfig。
- 机器人：`s1u-robot` Pod 全就绪，`rlark.io/power-state`、`rlark.io/self-check-state` 正常。

## 发布之外的现网改动

上线可以不走 `release apply`（最快的方式）。这样改的现网就和 `rlark-release-current`、`live.yaml` 记的发布不一样，下一次从上一个发布文件生成新发布时要把这些取值带上，否则会被改回去。

- 2026-11-27 20:31：Fleet dd5e62b 的机器人镜像 `fleet/robot@sha256:9f948f819969b4a09b7593260f4a94f896a88e1b1391641c145940543488cbc7`（底座 f3c2e900 未变）经 Gateway `PUT /api/v1/clusters/<集群>/addons/<名字>` 只换两处：`rlark-robot-s1u-01/s1u-robot` 的 `deviceImage`、`rlark-local-dev-gpu/fleet-episode-relay` 的 `image`（都从 `cb6d0171…` 换来），Addon 版本不变（v1.7.4、v0.1.0）。没走发布：dry-run 会把控制面 gateway、rlark-ui、astribot-iam 换回发布里的镜像，而它们跑着 liangcai 当天 20:08 的热部署。两集群 site-registry 的 `prewarm` 仍列旧摘要。按这次的现网生成的发布文件在管理机 `/var/lib/rlark/release/builds/ci-13b2594-dd5e62b/rlark/astribot/release/releases/ci-13b2594-dd5e62b.yaml`（没 apply，所以不进 `releases/`）。
- 同一晚同样只经 Gateway 换 `rlark-robot-s1u-01/s1u-robot` 的 `deviceImage`（底座 f3c2e900 未变，`fleet-episode-relay` 仍是 9f948f81）：21:17 Fleet 2a21d7c `fleet/robot@sha256:7f82d7ddfa0b…`（generation 46）；21:22 Fleet b20be42 `fleet/robot@sha256:f3e2de290170182ff6a4c2b2ec623482d37fdd20b8632024198f9cdde1ba2412`（47）；22:5x Fleet b7a3965 `fleet/robot@sha256:3286630ca30088ff1dde73713dd7ea77dfb8bb534703dda4822965c3129d92d6`（48，记录文件由单独线程分批 fsync）。都在没有 Run、机器人没人占用时换。
- 2026-11-28 00:0x：控制台 `rlark-ui` 换成按 061c9faf 构建的 `localhost:5555/rlark-ui:portal-061c9faf`（`sha256:3127a3b5fcf36214ad3085a2249d0895a69306eec3f2810a28453aea3a356910`，在管理机上用 `rlark-ui.Dockerfile` 构建，底座同 `images` 里的两个 mirror 摘要）。原因：liangcai 11-27 21:45 热部署的控制台（f8f2503，页面目录 bind 挂载进容器）构建时漏了 `VITE_IAM_PORTAL=1`，门户守卫没开，带会话打开 `/`、`/admin` 出现 RLark 原生登录页；061c9faf 起生产构建一律走门户。只改了 `/var/lib/rlark/rlark.compose.yaml` 里 `rlark-ui` 的 `image` 后 `docker compose -p rlark … up -d --no-deps rlark-ui`（备份 `rlark.compose.yaml.bak-20261128-console-portal`），不再挂页面目录；gateway、astribot-iam 仍是 liangcai 的热部署。回退：把 `image` 改回 `localhost:5555/rlark-ui:scheme-b-20261127-124706` 再 `up`（那会是不带 bind 挂载的 12:49 版本页面，不是 21:45 的）。
- 2026-11-27 22:53 / 23:50：断网自愈（`feat/node-netheal`，并入 `astribot/main` 的提交见 git log）叠在现网镜像上只换一个文件构建：`rlark/station-wireguard@sha256:d9f3c55a988f8b77ca7eed18ba32e163389b62e3d1ab3b521690285288dc9ce9`（`FROM` 现网 `682eaf02…`，换 `/station-gateway`），`rlark/s1u-robot@sha256:ee1298ad22bf25050c5d3b0a094c5e9a9d7827b0a1c2e895619804d1e74d2def`（`FROM` 现网 `9bbf6913…`，换 `/s1u/tuner/node_tuner.py`）。`rlark-robot-s1u-01` 上：`station-system/station-agent-host` 的 `gateway-program` 镜像用 `kubectl set image` 换（generation 30；`station-agent-network` 和其他集群没换）；`s1u-robot` 的 `image` 经 Gateway PUT 换（generation 49，`deviceImage` 仍是 Fleet b7a3965 的 `3286630c…`）。两个摘要都只在办公室仓库，没拷金山云。下一次发布要带上这两个镜像，或者整套构建（构建出来的同一份代码）。

## 回退

```bash
rlarkctl release apply -e environments/live.yaml --to /var/lib/rlark/release/runs/<时间>-before.json --back
```

`--back` 倒序走标了 `back` 的阶段（先 server，再各 agent）。CRD 和 Station 的放置不退；一个发布删掉的 CRD 字段或枚举值，旧版本不再接受，有这类对象在用时不要退过这个发布。

## 不在发布构建里的镜像

- `csi-driver-rclone` 的 KS3 版：
  ```bash
  astribot/csi-rclone-ks3/build.sh /abs/new/dir   # 单测 + 静态编译出 rcloneplugin
  docker build -f astribot/csi-rclone-ks3/Dockerfile --build-arg BASE=<仓库>/csi-rclone/csi-driver-rclone:v0.2.0 \
    -t <仓库>/csi-rclone/csi-driver-rclone:v0.2.0-ks3.3 /abs/new/dir
  ```
  新摘要写进 `pinned.yaml` 的 `csi-rclone`，在 `addons` 阶段上线。替换插件 Pod 会断开该节点所有 rclone 卷。Addon 设了 `nodeUpdateStrategy=OnDelete` 的集群，确认没有 Pod 在用卷后逐个节点替换；默认的 `RollingUpdate` 下，新的 `rcloneImage` 会自己滚完每个节点、断开所有挂载，所以发布新摘要前先看各集群的取值。
