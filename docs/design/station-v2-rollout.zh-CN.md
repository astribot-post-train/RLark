# Station v2 上线步骤（现场：2026-11-12 的六台节点）

设计见 [station-v2.zh-CN.md](station-v2.zh-CN.md)。本文只写做什么、怎么验、怎么退。

## 0. 上线记录（2026-11-12 16:27–17:17）

**已上线**：六个节点（`cailiang`、`alexfu`、`wendychen`、`10-0-2-57`、`10-0-2-196`、`s1u-01-x86`）第 1–5 步全部完成，网关用的都是算出来的配置。机器人 17:04–17:19 整机断电，开机后补的第 3–5 步。

**从现在起不再手改 `station-wireguard-config`。** 对端表 = `SiteNetwork default`（管理面 `station-system`）+ 各节点自己的登记，由 station-controller 算出来；改拓扑只改 `SiteNetwork.spec`。ConfigMap 只剩兜底：节点登记不到时网关退回它。加机器人见"之后：加一台机器人"。

| 步 | 时间 | Pod 重建 | 结果 |
|---|---|---|---|
| 1 管理面 | 16:27–16:31 | station-controller、station-api | 第一次换 controller 镜像崩溃循环（hostNetwork + 固定探针端口 + RollingUpdate 抢端口），回退，改 `Recreate` 后通过 |
| 2 本地 | 16:33 | 三个节点 | 登记 `.1/.5/.6`，网关仍读 ConfigMap |
| 2 机器人 | 17:03:25 | 一个 | 16:51 第一次失败并回退：DaemonSet 是 `imagePullPolicy: Never`，新镜像不在机器人的 containerd 里；预拉后重做通过 |
| 2 云端 | 17:05:12、17:05:14 | 两个 | 同样先预拉（两个一次性 Pod，`IfNotPresent`），再做 |
| 对比 | 17:07 | — | 五个节点算出来的配置相对手写的：+8、+4、+4、+2、0，**删除 0** |
| 3 本地、云端 | 17:09:13–17:10:46 | 五个 | 启动 10 s 后 `using the computed configuration`；新增的路由（管理机到 `10.0.2.57`、`10.0.2.196`）通 |
| 4 本地、云端 | 17:12 | 五个 | 管理机 `st-wg0` 路由前后一致 |
| 5 station-api、本地 | 17:15:26、17:16:10 | station-api、三个节点 | 监听 `*:18443`；本地集群的 agent 改走内网地址，登记来源变成各自的内网地址 |
| 3–5 机器人 | 17:25:05 | 一个 | 17:19:25 开机后隧道自己恢复；对比 +0 / 删除 0；一次重建同时改用计算配置、开删路由、`api-url` 走内网；10 s 后 `using the computed configuration`，两个方向各路 0 丢包 |

每一步之后：在线节点全部 Bound、心跳连续，管理机到各隧道地址和 3090 到各隧道地址 ping 0 丢包（重建后头 2–3 s 偶有 1–2 个丢包），HG 集群云端两个 Ray 节点一直 alive。备份都在开发机 `/var/backups/station-v2-20261112-162720/`（第 5 步在其下 `step5/`，只有 root 可读）。

现场和演练不一样、文档原来写错的地方：

- **镜像要先到节点上**。机器人和云端的 DaemonSet 都是 `imagePullPolicy: Never`，节点上的镜像是事先按摘要拉好的。机器人：`ctr -a /run/containerd-robot/containerd.sock -n k8s.io images pull --hosts-dir /etc/containerd-robot/certs.d 10.254.240.1:5443/rlark/<镜像>@<摘要>`（集群用的是第二个 containerd，配置在 `/etc/containerd-robot/`）。云端两个节点没有 ssh：在 `station-system` 里给每个节点起一个一次性 Pod（两个容器用这两个镜像、`IfNotPresent`），事件里看到 `Pulled` 后删掉。
- **回退用 `rollout undo`**，不是 `replace -f 备份.yaml`（备份带 `resourceVersion`，会冲突）。
- **station-api 的证书没有单独的 CA**：原来那张是自签的、带 `CA:TRUE`，各集群的 `station-api-ca` 信任的就是它。第 5 步用它当 CA 签了一张服务证书（`CN=station-api`，SAN `10.254.240.1`、`10.11.14.128`，10 年），放进 `station-api-tls`；原证书和私钥另存为 Secret `station-system/station-api-ca-signer`，以后续签用。各集群的 `station-api-ca` 没动。
- **办公室节点的端点先写死在声明里**：第 2 步时节点经隧道连 station-api，登记来源是隧道地址，不能当端点。第 5 步后本地集群经内网登记，来源已是内网地址；"端点留空、跟着登记来源走"随下一个版本启用（agent 改成上报默认路由的源地址）。
- **机器人的监听端口改成了 51830**（声明里已写）。机器人 16:39 非正常重启后仍用固定源端口 51820，实验室路由器上旧的 NAT 映射一直被对端的保活包续着，三个对端的回包被吃掉；换端口后 30 s 内全通。下一个版本：NAT 后的叶子节点不用固定源端口，枢纽对"等对方拨入"的对端不发保活。

集群向场站登记用 `extensions/station/hack/admit-member.sh`，两个子命令：`install`（把 agent 和 gateway 装进集群）、`credential`（登记管理面怎么访问这个集群的 API：给的是控制面在 SiteNetwork `names` 里的名字；凭据里的地址由 station-controller 按这个名字现在的地址维护，控制面换机器只改 `names` 那一行，每个集群只登记一次）。“机器人自带单节点集群、在 NAT 后面”的入网形态（原来的 `bootstrap → rebind → finish` 三个子命令）11-14 起不再支持，已删除：机器人都是场站集群的 worker，入网用 `admit-node.sh`。

## 0. 结论

- 五步。第 1、2 步不改任何节点的隧道行为；第 3 步才让网关读算出来的配置，逐个集群做，每个集群做完都验；第 4 步收尾；第 5 步让以后的新节点零配置入网。
- 镜像已经构建并推送（第 0.5 节），整套步骤在隔离环境里从线上现在的镜像起步演练过一遍（第 6 节）。
- 每一步的回退是一条命令，不需要到节点上手工清理（第 3 步以后回退时由新网关自己删多出来的路由）。
- 对运行中的 Run 的影响（第 6 节的演练实测）：每一步都会重建各节点的 Station 网络 pod，**pod 重建期间隧道不断**。从线上现在跑的那两个镜像起步，走完第 1–4 步和一次回退，5 条业务路径各 300–500 个 ping（间隔 0.2 s）**一个没丢**。原因：WireGuard 接口和会话在内核里，不随 pod 消失；新网关在配置和内核一致时不调用设备配置，有差别时只改有差别的那个对端。Ray 的 150–180 s 容忍度远大于实测的 0 s。所以第 1、2 步可以在两个 episode 之间做；第 3、4 步同样没有测到中断，是否也在 Run 中做由主会话定，稳妥的做法是放在 hold 时。
- 演练和线上的差别（没有被演练覆盖的）：管理面的两个 Deployment 在演练里是单独的容器，没有经过 Deployment 的更新（线上第一次换 controller 镜像就因此失败：hostNetwork + 固定探针端口 + RollingUpdate，新旧 pod 抢端口；回退后改成 Recreate 通过，仓库里的清单已改）；节点之间没有真实的 NAT 和 50 ms 时延；没有机器人经 Orin 的上联路由（只有内核单测覆盖）；节点共用一个内核（6.8，云节点线上是 5.15）；只测了 ICMP，没有跑 Ray。
- 所有命令在开发机 `cailiang`（10.11.14.128）上执行，需要 root（docker、三个集群的 kubeconfig 只有 root 可读）。每一步写成一个脚本放 `/tmp/`，由用户执行。

约定：

```bash
REPO=/home/cailiang/workspace/dev/RLark            # 合入本分支后的工作树
KCP=/home/cailiang/.local/share/rlark-runtime/kcp-admin.kubeconfig   # 管理面
LOCAL=/etc/kubernetes/admin.conf                   # rlark-local-dev-gpu
CLOUD=/dev/shm/rlark-cloud-kubeconfig              # rlark-ksyun-gpu
ROBOT=/dev/shm/rlark-robot-kubeconfig              # rlark-robot-s1u-01
TAG=enroll-20261112
BK=/var/backups/station-v2-$(date +%Y%m%d-%H%M%S)  # 每步开头 mkdir -p，先把要改的对象 -o yaml 存进去
```

## 准备（不需要 root，liangcai 做）

1. 镜像已经构建、演练并推送（第 0.5 节），上线不需要再构建。源码 = 本分支的树 + 他工作树里已经部署在线上的那一处未提交修复（`network_summary.go`、`node_network_handler.go` 和它们的测试，共 4 个文件；现场的 `station-services:wg-unreported-20261112` 就是带着它构建的）。他 11-12 14:29 起在写的 `NodeOperation`（未提交）不在这些镜像里，线上镜像里也没有。
   把 `feat/station-enrollment` 合进他的工作树时有三处要手工处理：`extensions/station/api/v1alpha1/groupversion_info.go`（两边各加了一行类型注册，都保留）、`zz_generated.deepcopy.go`（合并后用 controller-gen v0.18.0 重新生成）、两份 `extensions/station/README*.md`（开头的说明和他加的段落都保留）。
2. 跑测试：

   ```bash
   (cd extensions/station && GOWORK=off go test ./...)
   (cd extensions/station-gateway && GOWORK=off go test ./...)
   (cd apps/rlark && GOWORK=off go test ./pkg/stationidentity/ ./pkg/gateway/)
   ```

3. 核对声明文件 `extensions/station/deploy/site-network.live-20261112.yaml` 和现场一致：六个节点名、三个集群 ID、地址、端点。它对应 11-12 16:10 读到的现场（15:5x 有人手工加了第六台 `wendychen`，`10.254.240.6`，六份配置都改了，声明文件和单测已经跟上）。之后如果又改过任何一份 `station-wireguard-config`，先把改动补进声明文件；第 2 步的对比会把漏掉的查出来。

## 0.5 镜像（已构建、已演练、已推送）

**上线前要重新构建一次**：下表的镜像是 16:05 演练用的那一批，之后加了隧道探测、重启留痕、`/site-nodes` 的状态字段和"只连场站入口"的拓扑选项（设计文档 1.7、2.4），station-agent、station-api、station-controller 三个镜像变了，网关镜像没变。新加的部分只有单测；重新构建后要在隔离环境里再跑一遍第 6 节，才算可以上线。隔离环境不再放在 liangcai 的机器上（它是线上的控制面），等 3090 把 `fs.inotify.max_user_instances` 调到 8192。

| 镜像 | 标签 | 仓库里的摘要 |
|---|---|---|
| `localhost:5555/station-services`（station-api） | `enroll-20261112` | `sha256:3f79779e03ba287dc9c334c69ee71de9fc09bb60270cdef0cecb42547c46214a` |
| `localhost:5555/station-controller` | `enroll-20261112` | `sha256:c3e547a1e4d4add2b8c77d09d63cab65b90f89435cf34887f049e0e8197523fe` |
| `rlark/station-agent`（`10.11.14.128:5443` 和 `10.254.240.1:5443` 是同一个仓库） | `enroll-20261112` | `sha256:6c31551dcf42d7a49b108367e6ff948d67db11674e4ce6c591857bd4bedc3aba` |
| `rlark/station-wireguard` | `enroll-20261112` | `sha256:4fedb44376bdddf7df909aba8201661a5c0511df1c9c6444ef6ef08302b4bc4b` |

- 构建目录留在开发机 `/var/tmp/station-v2-final/`：`build.sh`（构建脚本）、`station-v2-tree.tar`（源码树）、`liangcai-deployed-fix.diff`（上面说的那 4 个文件的修复）。重新构建：`sudo bash /var/tmp/station-v2-final/build.sh <标签>`。
- 开发机的 docker 没有 buildx，`apps/rlark/Dockerfile` 的缓存挂载用不了，脚本用旧构建器构建一份去掉缓存挂载的 Dockerfile（阶段和产物相同）；buildah 在 root 下缺 netavark，用不了。
- docker 不信任 `10.11.14.128:5443` 的自签证书，agent 和网关镜像是用 `skopeo --insecure-policy copy --dest-tls-verify=false docker-daemon:… docker://10.254.240.1:5443/rlark/…` 推的。
- 演练用的是同一批镜像（本地镜像 ID：agent `85ec50f571d1`、网关 `a4dfb2584e39`、station-api `3f79779e03ba`、controller `c3e547a1e4d4`）。

## 第 1 步：管理面（不影响任何节点）

改什么：管理面装两个 CRD 和 RBAC；本地集群换 station-api、station-controller 的镜像；建 `SiteNetwork`。

```bash
cd $REPO
# 镜像已在仓库里（第 0.5 节），TAG=enroll-20261112

# 管理面：必须是已经有五个 Station CRD 的那个 workspace
kubectl --kubeconfig $KCP get crd stations.station.rlark.io            # 必须成功，否则 kubeconfig 指错了 workspace
# 两个新 CRD，加上改过的两个（Station 多了 status.reboots，StationObservation 的 probes 上限从 64 提到 256）
kubectl --kubeconfig $KCP apply -f extensions/station/config/crd/bases/station.rlark.io_sitenetworks.yaml \
                                -f extensions/station/config/crd/bases/station.rlark.io_networkenrollments.yaml \
                                -f extensions/station/config/crd/bases/station.rlark.io_stations.yaml \
                                -f extensions/station/config/crd/bases/station.rlark.io_stationobservations.yaml
kubectl --kubeconfig $KCP apply -f extensions/station/deploy/management-rbac.yaml

# 本地集群：两个 Deployment 换镜像
kubectl --kubeconfig $LOCAL -n station-system get deploy station-api station-controller -o yaml > $BK/deployments.yaml
# controller 是 hostNetwork、探针端口固定：必须先停旧的再起新的，所以策略改成 Recreate 和镜像一起换
kubectl --kubeconfig $LOCAL -n station-system patch deploy station-controller --type=json -p '[
  {"op":"replace","path":"/spec/strategy","value":{"type":"Recreate"}},
  {"op":"replace","path":"/spec/template/spec/containers/0/image","value":"localhost:5555/station-controller:'$TAG'"}]'
kubectl --kubeconfig $LOCAL -n station-system set image deploy/station-api station-api=localhost:5555/station-services:$TAG
kubectl --kubeconfig $LOCAL -n station-system rollout status deploy/station-controller deploy/station-api --timeout=120s

kubectl --kubeconfig $KCP apply -f extensions/station/deploy/site-network.live-20261112.yaml
```

station-api 是 `Recreate`，换镜像时停几秒：agent 的心跳退避重试，90 s 内恢复就不会有节点被标成 Offline；隧道不经过它。

验：

```bash
kubectl --kubeconfig $KCP get crd sitenetworks.station.rlark.io networkenrollments.station.rlark.io   # Established
kubectl --kubeconfig $KCP -n station-system get sitenet default -o jsonpath='{.status.conditions}'    # Ready=True
kubectl --kubeconfig $KCP get stn -A                                   # 五个 Station 仍是 Bound，心跳在走
kubectl --kubeconfig $LOCAL -n station-system logs deploy/station-controller --tail=50   # 没有 forbidden
```

管理面的 RBAC 如果实际是绑在 kcp 的用户而不是这份清单里的 ServiceAccount 上（他的部署用的是两个 kubeconfig Secret：`station-management-kubeconfig`、`station-management-kubeconfig-controller`），controller 日志会报 `forbidden`：把 `management-rbac.yaml` 里新增的三条规则加给那两个 kubeconfig 的用户。

退：`kubectl --kubeconfig $LOCAL apply -f $BK/deployments.yaml`。CRD 和 `SiteNetwork` 留着无害（旧 controller 不认识它们）。

## 第 2 步：节点登记，网关仍读 ConfigMap

改什么：三个集群的 DaemonSet `station-system/station-agent-network` 换两个容器的镜像；agent 加登记参数；网关加"发布公钥"。网关**不**加 `STATION_GATEWAY_COMPUTED_CONFIG`，所以它仍只读 ConfigMap。

```bash
# 镜像已在仓库里（第 0.5 节）：
AGENT_DIGEST=sha256:6c31551dcf42d7a49b108367e6ff948d67db11674e4ce6c591857bd4bedc3aba
WG_DIGEST=sha256:4fedb44376bdddf7df909aba8201661a5c0511df1c9c6444ef6ef08302b4bc4b
# 云和机器人集群经 10.254.240.1:5443 拉同一个仓库（同一份 KS3 后端），按摘要引用。
```

对每个集群（顺序：`$LOCAL` → `$ROBOT` → `$CLOUD`；镜像前缀本地集群用 `10.11.14.128:5443`，另外两个用 `10.254.240.1:5443`）：

```bash
kubectl --kubeconfig $K -n station-system get ds station-agent-network -o yaml > $BK/ds-$NAME.yaml
kubectl --kubeconfig $K -n station-system patch ds station-agent-network --type=json -p "$(cat <<EOF
[
 {"op":"replace","path":"/spec/template/spec/containers/0/image","value":"$REG/rlark/station-agent@$AGENT_DIGEST"},
 {"op":"replace","path":"/spec/template/spec/containers/1/image","value":"$REG/rlark/station-wireguard@$WG_DIGEST"},
 {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--network-identity-file=/var/run/station-observation/identity.json"},
 {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--network-config-file=/var/run/station-network/config.json"},
 {"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"wireguard-computed","emptyDir":{}}},
 {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts/-","value":{"name":"wireguard-computed","mountPath":"/var/run/station-network"}},
 {"op":"add","path":"/spec/template/spec/containers/1/volumeMounts/-","value":{"name":"wireguard-computed","mountPath":"/var/run/station-network","readOnly":true}},
 {"op":"add","path":"/spec/template/spec/containers/1/env/-","value":{"name":"STATION_GATEWAY_IDENTITY_FILE","value":"/var/run/station-observation/identity.json"}}
]
EOF
)"
kubectl --kubeconfig $K -n station-system rollout status ds/station-agent-network --timeout=300s
```

打补丁前先看一眼 `$BK/ds-$NAME.yaml`：容器 0 必须是 `station-agent`、容器 1 必须是 `wireguard`（快照里三个集群都是这个顺序）。

这一步 pod 会重建。接口 `st-wg0` / `st-hub0` 和内核里的会话不随 pod 消失；新网关起来后读的还是同一份 ConfigMap，和内核状态一致，不下发任何改动（新网关在状态一致时不调用设备配置）。

验（六台都要）：

```bash
kubectl --kubeconfig $KCP get netenroll -A
# 六行，PHASE 都是 Enrolled，ADDRESS 是 10.254.240.1 / 10.0.2.57 / 10.254.240.3 / 10.0.2.196 / 10.254.240.5 / 10.254.240.6
kubectl --kubeconfig $KCP -n station-system get sitenet default -o jsonpath='{.status.members} {.status.unreachable}{"\n"}'
# 6，后面为空
kubectl --kubeconfig $K -n station-system logs ds/station-agent-network -c station-agent | grep enrollment
# network enrollment: enrolled

# 逐台对比手写的和算出来的。删除项必须为 0 才能进第 3 步。
for each (K, NS, NODE):
  kubectl --kubeconfig $K   -n station-system get cm station-wireguard-config -o jsonpath="{.data.$NODE\.json}" > /tmp/live-$NODE.json
  kubectl --kubeconfig $KCP -n $NS get netenroll $NODE -o jsonpath='{.status.config}' > /tmp/computed-$NODE.json
  python3 extensions/station/deploy/compare-config.py /tmp/live-$NODE.json /tmp/computed-$NODE.json
```

`$NS` 是该集群在管理面的 `Cluster.spec.agentNamespace`（现场三个集群的 agentNamespace 和集群 ID 同名）。按 16:10 读到的六份配置，预期输出：

| 节点 | 输出 | 多出来的 |
|---|---|---|
| `cailiang` | `8 added, 0 removed` | 两个 VPC 地址各在对端和路由里算一项；对 3090 和 `wendychen` 的端点和保活 |
| `10-0-2-57` | `2 added, 0 removed` | NAS 网段 |
| `alexfu`、`wendychen` | `4 added, 0 removed` | 两个 Pod 网段 |
| `s1u-01-x86`、`10-0-2-196` | `0 added, 0 removed` | 无 |

出现任何 `-`：把它对应的对端或网段补进 `SiteNetwork` 的声明（`advertise`、`endpoints`），重新 apply，再比。

另外确认隧道没动：各节点 `wg show` 的 latest handshake 在 2 分钟以内，机器人 ping `10.0.2.57`、`10.0.2.196` 通，Job 的三个 Ray 节点都在。

退：`kubectl --kubeconfig $K -n station-system rollout undo ds/station-agent-network`。旧镜像在节点上有缓存。

## 第 3 步：网关读算出来的配置（逐个集群）

改什么：给 wireguard 容器加一个环境变量。先不开删路由。

顺序和时机：`$LOCAL`（随时）→ 验 → `$ROBOT`（Run 在 hold 时）→ 验 → `$CLOUD`（Run 在 hold 时）→ 验。

```bash
kubectl --kubeconfig $K -n station-system set env ds/station-agent-network -c wireguard \
    STATION_GATEWAY_COMPUTED_CONFIG=/var/run/station-network/config.json
kubectl --kubeconfig $K -n station-system rollout status ds/station-agent-network --timeout=300s
```

pod 重建后的过程：网关先按 ConfigMap 协调（和内核一致，无改动）；agent 第一次心跳后登记并写下算出来的配置（几秒内）；网关下一个 10 s 周期改读它，日志打 `using the computed configuration`，只把有差别的对端发给内核（按上表：`cailiang` 上两个云对端各加一个网段、3090 和 `wendychen` 两个对端加端点和保活；`10-0-2-57` 上 cailiang 对端加一个网段；`alexfu`、`wendychen` 上两个云对端各加一个网段；机器人和 `10-0-2-196` 没有任何改动），再加对应的路由。没开删路由时，如果算出来的配置少了内核里已有的路由，网关在动设备之前就报错停下，内核保持原样。

验：

```bash
kubectl --kubeconfig $K -n station-system logs ds/station-agent-network -c wireguard --tail=20
# 有 "using the computed configuration"，之后没有 "reconcile failed"
kubectl --kubeconfig $K -n station-system exec ds/station-agent-network -c wireguard -- /station-gateway --status
# 每个对端的 lastHandshake 在 2 分钟内
kubectl --kubeconfig $K -n station-system get pods -l app=station-agent-network   # 2/2 Ready
```

再从 3090 和机器人各 ping 一遍五个隧道地址和两个 VPC 地址；确认 Job 的 Ray 节点数没变。节点上多了一个文件 `/var/lib/station/wg/last-applied.json`。

退：`kubectl --kubeconfig $K -n station-system set env ds/station-agent-network -c wireguard STATION_GATEWAY_COMPUTED_CONFIG- STATION_GATEWAY_PRUNE_ROUTES=true`。网关回到 ConfigMap，并自己删掉这一步多加的路由。（只去掉第一个变量、不开删路由，网关会因为"路由需要手工删除"而停止协调，内核保持切换后的状态，隧道仍然通，但之后改 ConfigMap 不再生效。）

## 第 4 步：开删路由，收掉手写配置

三个集群都过了第 3 步并观察一段时间（建议跑完一个完整的采集批次）之后：

```bash
kubectl --kubeconfig $K -n station-system set env ds/station-agent-network -c wireguard STATION_GATEWAY_PRUNE_ROUTES=true
```

之后 DaemonSet 的定义和仓库里的 `extensions/station/deploy/station-agent.yaml` 只差两处：hostPath 类型（`Directory` → `DirectoryOrCreate`，新节点不用先手工建目录）和 ConfigMap 卷的 `optional: true`。用仓库里的清单替换镜像地址后整体 apply 一次，把这两处带上。

`station-wireguard-config`：

- 本地集群和机器人集群：保留到第 5 步做完，然后可以删（ConfigMap 卷是 optional）。在那之前它是回退手段。
- 云集群：两台各留一份引导文件，只保留 `cailiang` 这一个对端和到 `10.254.240.1/32` 的路由（现在文件里的第一个 peer）。云节点重启后先用 `last-applied.json`，这份引导文件只在节点重装、`/var/lib/station/wg` 丢了的时候用得上。同时在 `SiteNetwork.spec.nodes` 里给这两台补上 `publicKey`（各自现在的公钥）：节点重装后它的登记会失效，有了声明的公钥，重绑之前别的节点仍认得它的引导隧道。

退：去掉 `STATION_GATEWAY_PRUNE_ROUTES`。

## 第 5 步：让新节点零配置入网

现在 station-api 只监听隧道地址 `10.254.240.1:18443`，一台还没有隧道的新节点连不到它。办公室、实验室、机器人都能连开发机的内网地址，所以：

1. station-api 的证书加上 SAN `10.11.14.128`（和现在的 `10.254.240.1` 并列），更新 Secret `station-api-tls`；CA 不变，各集群的 `station-api-ca` 不用动。
2. Deployment `station-api` 的参数 `--addr=10.254.240.1:18443` 改成 `--addr=:18443`，两个探针的 `host` 去掉。重建时停几秒，影响同第 1 步。
3. 本地集群和机器人集群的 ConfigMap `station-agent` 的 `api-url` 改成 `https://10.11.14.128:18443`，重启 DaemonSet。云集群不改（它只能经隧道连）。

验：从 3090 `curl --cacert ca.crt https://10.11.14.128:18443/healthz` 返回 ok；三个集群的 agent 日志仍是 `enrolled`。

退：`api-url` 改回去；`--addr` 改回去。

## 之后：加一台机器人

前提只有两条，都是机器本身的事：它已经是某个成员集群的节点（现在的做法：每台机器人自带一个单节点集群，已注册进 RLark）；它的 containerd 里有 Station 的两个镜像（上面"镜像要先到节点上"）。

1. 这台机器人的集群第一次入网时，在 `SiteNetwork default` 的 `sites` 里把它的集群 ID 加进 `lab` 的 `clusters`（机器人并进同一个集群以后，这一步没有了）；它的集群里 `station-agent` 的 `api-url` 是 `https://10.11.14.128:18443`。
2. 默认路由出不去、要经 Orin 的机器人，在 `nodes` 里给它写一条 `uplink`（照 `s1u-01-x86` 那条）。默认路由能出去的不用写任何东西。
3. `kubectl --kubeconfig $K label node $NODE station.rlark.io/network=enabled`。

之后不用做任何事：agent 向 station-api 登记自己的公钥，controller 从地址池分一个地址、算出它和各枢纽的对端表，各枢纽的网关自己把它加进去。演练实测打标签后 16 s 连上第一台枢纽，28 s 连上全部。看结果：`kubectl --kubeconfig $KCP get netenroll -A` 多一行 `Enrolled` 和它的地址，`get stn -A` 里它是 `Bound`。不改任何 ConfigMap，不抄公钥，不动别的节点。

第二台机器人入网时还要做一次（只一次）：把场站入口声明为 `relay: true` 并在那台机器上放行转发（下面"要额外声明的情况"），否则机器人之间不通。

## 之后：加一台其他节点

机器人、办公室或实验室的一台机器（已经是某个成员集群的节点，containerd 能拉 Station 的两个镜像）：

```bash
kubectl --kubeconfig $K label node $NODE station.rlark.io/network=enabled
```

演练实测：打标签后 16 s 连上第一台枢纽，28 s 连上全部四台枢纽。`kubectl --kubeconfig $KCP get netenroll -A` 多一行 `Enrolled` 和它的地址。不改任何 ConfigMap，不抄公钥。

要额外声明的情况（改 `SiteNetwork.spec` 一处）：

- 新的成员集群：加进某个 `sites[].clusters`。每台机器人自带一个单节点集群的做法下，每台新机器人要加一行；机器人改成同一个集群后就不用了。
- 机器人的默认路由出不去、要经 Orin：在 `nodes` 里给它写 `uplink`。
- 第二台机器人入网时：把场站入口（现在是 `cailiang`，以后是场站工作站）声明为 `relay: true`，并在那台机器上放行转发，否则机器人之间不通（`SiteNetwork.status.unreachable` 会列出来）：

  ```bash
  iptables -I FORWARD -i st-wg0 -o st-wg0 -j ACCEPT    # 并写进开机脚本；net.ipv4.ip_forward 在 k8s 节点上已经是 1
  ```

- 云节点（连不到 station-api，直到它的隧道起来）：
  1. 给节点打标签，pod 起来后从 wireguard 容器日志里抄一次公钥；
  2. 在 `SiteNetwork.spec.nodes` 里声明它：`role: Hub`、公网端点、`nodeAddress`、钉住的 `address`，以及 `publicKey`（就是抄来的那个）。场站入口和其他节点据此先把它加进对端表；
  3. 在云集群的 `station-wireguard-config` 里给它一份引导文件 `<节点>.json`：只有场站入口一个对端（公钥、`allowedIPs: ["10.254.240.1/32"]`，不写端点，等场站入口拨入）。
  隧道起来后它经隧道登记，之后用的是算出来的配置和它自己登记的公钥。3 台云节点各一次；专线通了以后不需要。

## Gateway 和界面（可选，随时）

`/api/v1/site-nodes/{node}` 和界面改名在 RLark Gateway 和 UI 里。Gateway 是手工 `docker run` 的容器、UI 是 vite 开发服务，按他现在换 Gateway 镜像的方式换一次即可；不换不影响入网。Fleet 开始用这个接口之前要换。

## 6. 隔离环境里的端到端演练（2026-11-12 14:30–16:05）

**结论：整条链路在真实的 kcp、真实的 TokenReview、真实的内核 WireGuard 上跑通；上线的每一步和回退都没有丢一个 ping；演练查出 2 个代码问题和 1 个原有流程的问题，都已修并重测。**

### 6.1 环境

`extensions/station/hack/e2e/e2e.sh up`：一次性的 kcp v0.30.0（和线上同版本）、PostgreSQL 15（TLS，verify-full）、station-api、station-controller 四个容器，加三个 kind 集群（v1.31）共 5 个节点，对应今天的三个场站：

| 演练里的节点 | 对应线上 | 角色 |
|---|---|---|
| `e2e-office-control-plane` | `cailiang` | 枢纽，场站内端点，替云场站代理一个"NAS"地址 |
| `e2e-office-worker` | `alexfu` | 枢纽，端点取自己上报的地址 |
| `e2e-lab-control-plane` | `s1u-01-x86` | 叶子 |
| `e2e-ksyun-control-plane` | `10-0-2-57` | 枢纽，公网端点，另有一个"VPC"地址 |
| `e2e-ksyun-worker` | `10-0-2-196` | 同上，接口 `st-hub0`、端口 51821 |

每个节点是一个容器，有自己的网络命名空间；DaemonSet 用仓库里的清单原样部署，WireGuard 接口由网关容器建在节点自己的命名空间里。地址取自 `198.18.0.0/15`（基准测试保留段）：真实网络上没有主机会回应，离开隧道的 ping 一定是丢的（一开始用 `10.99.0.0/24`，公司内网里有 `10.99.0.1` 回包，把一次断线盖住了）。

放在 liangcai 的开发机上跑：3090 的 `fs.inotify.max_user_instances=128` 只够起 2–3 个 kind 节点。和线上没有任何连接（容器名 `e2e-*`，docker 网络 `kind`，各自的 kcp 和 station-api），跑完全部删除；删除后线上 6 个 Station 仍是 Bound，隧道 ping 正常。

### 6.2 入网和变更

| 场景 | 做了什么 | 结果 |
|---|---|---|
| 五台入网 | 部署 DaemonSet，给节点打标签 | 75 s 内 5 个 Station Bound、5 个 `NetworkEnrollment` Enrolled；5 台互 ping 各自的唯一地址，0 丢包；同场站两台云节点的隧道地址互相不通（设计如此，它们用 VPC 地址） |
| 新机器人入网 | 新建一个单节点集群，加进 `lab` 场站，打标签 | 分到池里下一个地址。打标签到连上第一台枢纽 16 s，连上全部四台枢纽 28 s（修掉 pending 等待之前是 73 s） |
| 叶子之间 | 两台机器人互 ping | 不通，`SiteNetwork.status.unreachable` 列出这两对，`FullyConnected=False`；把办公室枢纽声明为 `relay: true` 后 50 s 内列表清空，互 ping 0 丢包 |
| 换密钥 | 删节点上的私钥和 `last-applied.json`，删 pod，同时机器人 → 云持续 ping | 新公钥自动登记；600 个 ping（0.2 s 间隔）丢 38 个，最长连续中断 7.6 s |
| 节点重装 | 删 Node 对象、删接口和密钥目录、重启 kubelet（新 Node UID） | Station 变 Conflict，入网变 Rejected，其他节点的对端表里去掉它（机器人 4 个对端变 3 个），旧地址不通 |
| 重绑 | 建 `StationRebind`（旧 Station、新 Node UID） | Applied；49 s 后机器人能 ping 通重装的节点；绑定版本 2，隧道地址和节点地址都不变 |
| 移除 | Station 置 `retired` | 25 s 后枢纽上它的对端和路由都没了（路由是网关自己删的）；地址回池 |
| station-api 和 controller 停掉 | 停两个容器，删一台节点的 pod **和接口** | 网关日志 `using the last-applied configuration`，隧道重新建起，ping 0 丢包 |
| 无隧道模式 | `mode` 改成 `Routed` 再改回 | 登记保持 Enrolled，地址变成节点自己的地址，不下发配置；已有隧道不受影响；改回后恢复 |

### 6.3 上线演练

> v2 已于 2026-11-12 上线。这一节用到的 `hack/e2e/migrate.sh`、`hack/e2e/station-agent.live-20261112.yaml` 和 `deploy/compare-config.py` 是那次从手写配置迁到 v2 的一次性工具，上线后已从仓库删除（见 git 历史）；下面是当时的记录。

`migrate.sh start` 造出上线前的状态：5 台节点跑**线上现在的两个镜像**（`station-agent@sha256:c5b1…`、`station-wireguard@sha256:b256…`，从仓库按线上 DaemonSet 的摘要核对过）和基准提交里的 DaemonSet 清单，读手写的 ConfigMap（形状和线上相同：算出来的配置去掉 6.2 表里那几项"多出来的"）；管理面没有 `SiteNetwork`。然后照本文的步骤逐条执行，同时 5 条路径各自每 0.2 s ping 一次（机器人 → 两台云节点的 VPC 地址、机器人 → 办公室枢纽、云 → 机器人、3090 → 机器人），每步前后各留 30 s。

| 步骤 | 重建的 pod | 每条路径发出的 ping | 丢失 | 最长中断 |
|---|---|---|---|---|
| 第 1 步：建 `SiteNetwork` | 无 | 302–304 | 0 | 0.0 s |
| 第 2 步：办公室集群换镜像 | 2 | 333–334 | 0 | 0.0 s |
| 第 2 步：机器人集群 | 1 | 312–313 | 0 | 0.0 s |
| 第 2 步：云集群 | 2 | 325–326 | 0 | 0.0 s |
| 第 3 步：办公室集群读算出来的配置 | 2 | 506–507 | 0 | 0.0 s |
| 第 3 步：机器人集群 | 1 | 306–307 | 0 | 0.0 s |
| 第 3 步：云集群 | 2 | 365–366 | 0 | 0.0 s |
| 第 4 步：三个集群开删路由 | 各自全部 | 307–322 | 0 | 0.0 s |
| 回退办公室集群（去掉变量并开删路由） | 2 | 324–326 | 0 | 0.0 s |
| 办公室集群再做一次第 3 步 | 2 | 316–318 | 0 | 0.0 s |

- 第 2 步之后 `compare-config.py` 的输出和单测预测的一致：`6 added / 4 added / 0 / 2 added / 0`，全部 `0 removed`。
- 第 3 步之后办公室枢纽的 `st-wg0` 上路由从 6 条变 8 条（两个 VPC 地址）；回退之后回到 6 条，是网关自己删的。
- 量具本身验证过：把机器人的接口手工关 3 s，同一套 ping 报出 5.8–6.0 s 的中断。

所以对"每台节点的 pod 重建时隧道断多久"的回答是：测不到（小于 0.2 s 的分辨率，19 次 pod 重建 × 5 条路径都是 0 丢包）。

### 6.4 演练查出的问题

| 问题 | 怎么发现的 | 处理 |
|---|---|---|
| 入网请求第一次总是 pending，agent 等满 30 s 才再问，新节点入网要 70 多秒 | 新机器人入网计时 | pending 时 5 s 后重试；重测 16 s / 28 s |
| 重装后的同名节点永远报不上来，Station 不会变 Conflict；重绑后又被旧快照卡死（原有流程的问题） | 节点重装场景：Station 停在 Offline，重绑 Applied 后 agent 仍然 401 | 见"给 liangcai"第 8 条；重测通过（上表"节点重装""重绑"两行是修后的结果） |
| agent 上报失败不打任何日志 | 上面那个问题排查不动 | 状态变化时打一行 |
| controller 把和 station-api 的写冲突记成 error | 演练时看 controller 日志 | 冲突时安静地重排队；最终镜像的日志里 error 为 0 |
| kcp 的证书只认它的容器地址和 `localhost` | 起环境时 | 环境脚本里按地址访问（线上的管理面 kubeconfig 也是这么写的） |
| 退役的 Station 占着节点名 | 重建演练集群后同名节点上报 | 没改，列在"给 liangcai"里 |

### 6.5 怎么重跑

```bash
cd extensions/station/hack/e2e
IMAGES_TAG=<标签> ./e2e.sh up            # 约 6 分钟；镜像名 station-v2/<四个名字>:<标签>，在本机 docker 里
./e2e.sh site_network                    # 声明三个场站
for c in e2e-office e2e-lab e2e-ksyun; do ./e2e.sh enable $c; done
./e2e.sh mgmt get stn,netenroll -A       # 管理面
./e2e.sh ping e2e-lab-control-plane -c 3 198.19.2.57
./e2e.sh down
```

需要 docker、kind、kubectl、openssl、python3，宿主机有 wireguard 模块，`fs.inotify.max_user_instances` ≥ 8192；`migrate.sh start` 还需要线上那两个镜像在本机 docker 里（`station-v2/station-agent:live-20261112`、`station-v2/station-wireguard:live-20261112`）。

## 给 liangcai：改了什么、为什么

分支 `feat/station-enrollment`，基于 `feat/optimization` 1deb4ef，没有改你 13:18 时未提交的任何文件；你 14:29 以后新写的 NodeOperation 和它在三处相碰（见"准备"第 1 条）。

1. **新增 `extensions/station/network`**（纯函数，无 k8s 依赖）：输入拓扑声明和已登记的节点，输出每台节点的网关配置（就是你 `station-gateway` 读的那个 JSON）。原因：加一台节点时手改 N 份配置这件事，换成一处声明 + 计算。`TestComputeCoversLiveSite` 用 11-12 你那五份手写配置做基准，保证算出来的只多不少。
2. **新增两个 CRD**（同一个组 `station.rlark.io/v1alpha1`）：`SiteNetwork`（管理员写：地址池、场站、不只是叶子的节点）和 `NetworkEnrollment`（一台节点的入网请求和答复，名字 = 节点名，放在集群的 agentNamespace，和 `StationObservation` 同一个约定）。`station-controller` 多一个 `NetworkReconciler`：整张网是一个协调键，所以地址分配不会并发。地址记在 `SiteNetwork.status.allocations`，键是 Station 的 UID，重绑不变、退役回收（和你 `StationSequence.status` 一样，是放在 status 里的持久状态）。
3. **`station-api` 加 `POST /v1/network/enrollments`**：走你已有的 `authenticate`（TokenReview → Pod → Node → Station 绑定），要求已绑定 Station；把公钥和节点自报地址写进 `NetworkEnrollment.spec`，返回 controller 针对这次请求算出的 status。没有用 challenge：这个请求是幂等的"报告我的公钥"，重放它得不到任何东西。
4. **`station-agent` 加登记**：心跳之后调上面的接口，把配置原子写到和网关共享的 emptyDir。pending、被拒、出错都不动已有文件。
5. **`station-gateway` 四处改动**：
   - 启动时把公钥写到共享目录（不用再从日志里抄）；
   - 配置来源三级：算出来的 → 本机上次执行过的（`/var/lib/station/wg/last-applied.json`）→ 你原来的 `--config`。不设 `STATION_GATEWAY_COMPUTED_CONFIG` 时行为和原来完全一样；
   - `deviceConfig` 从"整组替换 peer"改成"只发有差别的 peer + 显式删除"。原来任何一个对端变化都会让这台节点所有会话重新握手；100 台时每加一台机器人，每个枢纽的近百个会话都要重来。你的 `TestUnchangedPeerKeepsLearnedEndpoint` 按新语义改了断言（不变的 peer 现在根本不发给内核，学到的端点自然保留）；
   - `--prune-routes`（默认关）：只删自己创建并打了别名的接口上、协议号 242、metric 49321 的路由。你原来"不删任何路由"的理由是协议号和 metric 证明不了归属；加上"出接口是我们自己建的那个"以后可以证明，而且不删的话，任何一次去掉对端都会让协调停住。默认行为没变。
   - `checkReady` 的第一个参数从配置路径改成配置内容（因为来源不再是固定一个文件）。
6. **Gateway 加 `GET /api/v1/site-nodes/:name`**，界面"Station 身份 / 工作站"改叫"场站节点 / Site Nodes"。原因见设计文档第 1 节：Station 在论文和 Fleet 里是一个 Run 里的位置，一台 8 卡节点上有好几个；你这个对象一台机器一个，是"节点"。组名、kind、可执行文件名都没动，迁移方案在设计文档 1.3。
7. **清单**：`station-agent.yaml` 网络模板加了登记参数、共享卷、三个环境变量，hostPath 改成 `DirectoryOrCreate`，ConfigMap 卷改成 optional；`management-rbac.yaml` 加了两种新资源的权限；`Dockerfile`（gateway）多拷一个 `source.go`。

8. **重装后的同名节点能上报了**（端到端演练查出来的，原有流程的问题）。原来 `KubernetesStationBindingResolver.Resolve` 遇到"同名、Node UID 不同"直接报错，这台节点的任何请求都被拒：Station 只会变 Offline，永远不会出现你界面上"仅 Conflict 可重绑"所依赖的那条新 UID 的观测；手工建 `StationRebind` 即使 Applied，旧 UID 留下的 `StationObservation` 又让 `StationReconciler` 把它标成 Conflict，agent 继续被拒，解不开。现在：这种节点按"已验证、未绑定"返回（`ErrStationBindingNotFound`），只能发观测，不能发 challenge、敏感操作和入网；`KubernetesObservationSink` 在同一集群里发现快照的 Node UID 和当前已验证的不同，就删掉旧快照重建。之后就是你原来设计的流程：Conflict → 重绑 → Bound。你那条测试"same name changed UID 必须报错"仍然成立（返回的还是错误）。
9. **agent 上报失败会打一行日志**（原来完全不出声，演练里排查全靠它），入网 pending 时 5 s 后重问（原来等满 30 s）。
10. **端到端环境** `extensions/station/hack/e2e/`：`e2e.sh up` 在一台机器上起一次性的 kcp、PostgreSQL、station-api、station-controller 和三个 kind 集群，节点上跑真实的 DaemonSet 和内核 WireGuard；`migrate.sh` 是上线演练。

和你正在写的 `NodeOperation` 的关系：`EnableWireGuard` 的 `AllocateAddress`、`ApplyPeer` 两步和这里的入网是同一件事。建议 `NodeOperation` 只做它独有的（SSH 进新机器 `kubeadm join`、移除），`EnableWireGuard` 缩成"打标签，等这台的 `NetworkEnrollment` 变成 Enrolled、握手成功"。

11. **"在线"拆成三件事**（起因：3090 中午重启后仍显示在线）。agent 每次上报前经隧道探测配置里的每个对端（TCP 连对端 agent 的健康端口），结果放进你已有的 `probes` 字段（上限 64 → 256，枢纽的对端会超过 64）；`StationReconciler` 的 `NetworkVerified` 从"所有探测都成功"改成"连得上所有自己也在线的对端"；开机时间变化记成 `Rebooted` 条件和 `status.reboots`。`phase` 的含义没动，仍只看心跳。
12. **场站可以让叶子只连转发枢纽**（`SiteNetwork.spec.sites[].leavesViaRelay`）：机器人只有一个对端，云上的地址由枢纽带给它。

没做、需要你定的：

- 转发枢纽的防火墙规则现在是手工一条 `iptables`。要不要让网关在声明了 `relay` 时自己加（它有 `NET_ADMIN`），你原来的原则是网关不碰宿主机防火墙，我没有改这个原则。
- 入网状态在界面上还看不到，只能 `kubectl get netenroll -A`。
- `station-api` 现在只听隧道地址；第 5 步要它也听内网地址，证书要加 SAN。
- 退役的 Station 占着它的节点名：同名的新节点上报后只会让那个已退役的 Station 显示 Conflict，不会建新的（演练里重建一个集群时碰到）。退役机器人的主机名要复用时，现在只能删掉旧 Station 对象。要不要让"已退役"不参与同名判断，你定。
- 你文档里计划的"Worker 租约、epoch、旧指令隔离"建议不做：这部分在 Fleet 的 HotSwarm 和机器人本地设备层里已经有了，放进 station-api 会让机器人安全依赖一个单副本的中心服务（设计文档 1.5）。
