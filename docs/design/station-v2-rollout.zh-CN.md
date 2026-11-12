# Station v2 上线步骤（现场：2026-11-12 的五台节点）

设计见 [station-v2.zh-CN.md](station-v2.zh-CN.md)。本文只写做什么、怎么验、怎么退。

## 0. 结论

- 五步。第 1、2 步不改任何节点的隧道行为；第 3 步才让网关读算出来的配置，逐个集群做，每个集群做完都验；第 4 步收尾；第 5 步让以后的新节点零配置入网。
- 每一步的回退是一条命令，不需要到节点上手工清理（第 3 步以后回退时由新网关自己删多出来的路由）。
- 运行中的 Job（`jo-9911056bc1f54b53`，两台金山云 + s1u_01）：第 1、2 步随时可做；第 3 步对机器人集群和云集群的那两次，放在 Run 处于 hold 或两批之间做。依据：切换时内核里的对端、端点、已有网段都不变，只多几个网段，WireGuard 会话不重置；但 pod 会重建一次，选一个不心疼的时刻。
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

1. 把 `feat/station-enrollment` 合进他的工作树。他有 28 个未提交文件，其中和 Station 代码有关的只有 `apps/rlark/pkg/stationidentity/network_summary.go` 和它的测试，本分支没有动这两个文件，`git merge` 不冲突。现场的 station-api 镜像（`station-services:wg-unreported-20261112`）就是从这个未提交的工作树构建的，所以**必须从合并后的工作树构建**，不能从干净的分支构建，否则会丢掉他那处修改。
2. 跑测试：

   ```bash
   (cd extensions/station && GOWORK=off go test ./...)
   (cd extensions/station-gateway && GOWORK=off go test ./...)
   (cd apps/rlark && GOWORK=off go test ./pkg/stationidentity/ ./pkg/gateway/)
   ```

3. 核对声明文件 `extensions/station/deploy/site-network.live-20261112.yaml` 和现场一致：五个节点名、三个集群 ID、地址、端点。11-12 13:18 之后如果又改过任何一份 `station-wireguard-config`（加过对端或网段），先把改动补进声明文件；第 2 步的对比会把漏掉的查出来。

## 第 1 步：管理面（不影响任何节点）

改什么：管理面装两个 CRD 和 RBAC；本地集群换 station-api、station-controller 的镜像；建 `SiteNetwork`。

```bash
cd $REPO
docker build -f apps/rlark/Dockerfile -t localhost:5555/station-services:$TAG .
docker build -f extensions/station/Dockerfile -t localhost:5555/station-controller:$TAG .
docker push localhost:5555/station-services:$TAG
docker push localhost:5555/station-controller:$TAG

# 管理面：必须是已经有五个 Station CRD 的那个 workspace
kubectl --kubeconfig $KCP get crd stations.station.rlark.io            # 必须成功，否则 kubeconfig 指错了 workspace
kubectl --kubeconfig $KCP apply -f extensions/station/config/crd/bases/station.rlark.io_sitenetworks.yaml \
                                -f extensions/station/config/crd/bases/station.rlark.io_networkenrollments.yaml
kubectl --kubeconfig $KCP apply -f extensions/station/deploy/management-rbac.yaml

# 本地集群：两个 Deployment 换镜像
kubectl --kubeconfig $LOCAL -n station-system get deploy station-api station-controller -o yaml > $BK/deployments.yaml
kubectl --kubeconfig $LOCAL -n station-system set image deploy/station-controller controller=localhost:5555/station-controller:$TAG
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
cd $REPO
docker build -f extensions/station/deploy/Dockerfile.agent -t 10.11.14.128:5443/rlark/station-agent:$TAG .
docker build -t 10.11.14.128:5443/rlark/station-wireguard:$TAG extensions/station-gateway
docker push 10.11.14.128:5443/rlark/station-agent:$TAG
docker push 10.11.14.128:5443/rlark/station-wireguard:$TAG
# 记下两个 digest。云和机器人集群经 10.254.240.1:5443 拉同一个仓库（同一份 KS3 后端），按 digest 引用。
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

验（五台都要）：

```bash
kubectl --kubeconfig $KCP get netenroll -A
# 五行，PHASE 都是 Enrolled，ADDRESS 是 10.254.240.1 / 10.0.2.57 / 10.254.240.3 / 10.0.2.196 / 10.254.240.5
kubectl --kubeconfig $KCP -n station-system get sitenet default -o jsonpath='{.status.members} {.status.unreachable}{"\n"}'
# 5，后面为空
kubectl --kubeconfig $K -n station-system logs ds/station-agent-network -c station-agent | grep enrollment
# network enrollment: enrolled

# 逐台对比手写的和算出来的。删除项必须为 0 才能进第 3 步。
for each (K, NS, NODE):
  kubectl --kubeconfig $K   -n station-system get cm station-wireguard-config -o jsonpath="{.data.$NODE\.json}" > /tmp/live-$NODE.json
  kubectl --kubeconfig $KCP -n $NS get netenroll $NODE -o jsonpath='{.status.config}' > /tmp/computed-$NODE.json
  python3 extensions/station/deploy/compare-config.py /tmp/live-$NODE.json /tmp/computed-$NODE.json
```

`$NS` 是该集群在管理面的 `Cluster.spec.agentNamespace`（`kubectl --kubeconfig $KCP get netenroll -A` 的第一列）。按 13:18 的快照，预期输出：`s1u-01-x86` 和 `10-0-2-196` 是 `0 added, 0 removed`；`cailiang` 是 `6 added, 0 removed`（两个 VPC 地址各在对端和路由里算一项，加 3090 对端的端点和保活）、`10-0-2-57` 是 `2 added`（NAS 网段）、`alexfu` 是 `4 added`（两个 Pod 网段），全是 `+`。出现任何 `-`：把它对应的对端或网段补进 `SiteNetwork` 的声明（`advertise`、`endpoints`），重新 apply，再比。

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

pod 重建后的过程：网关先按 ConfigMap 协调（和内核一致，无改动）；agent 第一次心跳后登记并写下算出来的配置（几秒内）；网关下一个 10 s 周期改读它，日志打 `using the computed configuration`，只把有差别的对端发给内核（按上表：`cailiang` 上两个云对端各加一个网段、3090 对端加端点和保活；`10-0-2-57` 上 cailiang 对端加一个网段；`alexfu` 上两个云对端各加一个网段；机器人和 `10-0-2-196` 没有任何改动），再加对应的路由。没开删路由时，如果算出来的配置少了内核里已有的路由，网关在动设备之前就报错停下，内核保持原样。

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

## 之后：加一台节点

机器人、办公室或实验室的一台机器（已经是某个成员集群的节点，containerd 能拉 Station 的两个镜像）：

```bash
kubectl --kubeconfig $K label node $NODE station.rlark.io/network=enabled
```

约 1 分钟后：`kubectl --kubeconfig $KCP get netenroll -A` 多一行 `Enrolled` 和它的地址；所有枢纽的对端表里多了它。不改任何 ConfigMap，不抄公钥。

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

## 给 liangcai：改了什么、为什么

分支 `feat/station-enrollment`，基于 `feat/optimization` 1deb4ef，6 个提交，没有改你未提交的任何文件。

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

没做、需要你定的：

- 转发枢纽的防火墙规则现在是手工一条 `iptables`。要不要让网关在声明了 `relay` 时自己加（它有 `NET_ADMIN`），你原来的原则是网关不碰宿主机防火墙，我没有改这个原则。
- 入网状态在界面上还看不到，只能 `kubectl get netenroll -A`。
- `station-api` 现在只听隧道地址；第 5 步要它也听内网地址，证书要加 SAN。
- 你文档里计划的"Worker 租约、epoch、旧指令隔离"建议不做：这部分在 Fleet 的 HotSwarm 和机器人本地设备层里已经有了，放进 station-api 会让机器人安全依赖一个单副本的中心服务（设计文档 1.5）。
