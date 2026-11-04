# 混合 Raw 与 Kubernetes Job 预览

状态：控制器级预览，已有自动化测试。云端到机器人 Fleet 验收、自动中继注入以及
统一 UI 提交流程尚未完成。Agent 心跳或这些测试不能证明 Lumo2 推理已经就绪。

## Cluster 与 Ray 身份

A、B、C 属于同一个原生 RLark 逻辑容器 Cluster。每个 Notebook 在现有容器内
运行 Raw Agent 和 supervisor，不要求 Docker Engine、特权容器或再安装一套
Kubernetes。A 另外承载管理网关，并在配置后承载 Ray TCP 网关。机器人 x86
继续属于原来的独立 Kubernetes Cluster。

一个集群级原生 Job 同时拥有容器 Cluster Agent namespace 中的 Raw rollout Task
和机器人 Cluster Agent namespace 中的 Kubernetes Env Task。B 不加入机器人的
Kubernetes 集群。两种工作负载角色必须加入同一个 Ray cluster/head。此预览不会
创建 Ray head；选定 head 的生命周期和 Raw 进程命令仍由管理员配置。

## 显式调度契约

原生 Job 设置注解 `rlark.io/controller-owner: containers.rlark.io`。扩展控制器
启用 `--enable-job-reservations=true` 后，由 namespaced 的
`containers.rlark.io/v1alpha1` ContainerJob 按名称和 Kubernetes UID 绑定 Job
及 Raw Cluster。其不可变的 `spec.kubernetesTargets` 列表为每个 Kubernetes
模板显式指定原生 Cluster 实例：

```yaml
# ContainerJob.spec 片段；根据当前 API 查询替换名称和 UID。
runtime: Raw
cluster:
  name: ksyun-containers
  uid: REPLACE_CONTAINER_CLUSTER_UID
job:
  name: REPLACE_JOB_NAME
  uid: REPLACE_JOB_UID
tenantID: REPLACE_TENANT_ID
kubernetesTargets:
  - template: env
    cluster:
      name: rlark-robot-s1u-01
      uid: REPLACE_ROBOT_CLUSTER_UID
```

绑定资源必须位于 Raw Cluster 权威的 `spec.agentNamespace`。目标引用不填写
namespace，由控制器从当前 Cluster 状态解析其 Agent namespace。缺失、重复、
多余、被替换、跨租户、删除中、cordon、drain 及存在歧义的目标均被拒绝。
目标 namespace 被容器 Cluster 占用时也被拒绝。调度和 Raw 下发共用同一份
当前绑定校验；过期的 Validated 条件不能授权启动。

混合预览允许 2–16 个名称唯一的模板，至少包含一个 Raw 进程和一个原生
Kubernetes Ray worker。每个 Kubernetes 模板使用单副本 Deployment，并显式
填写单播 IPv4 `headAddress`（不含端口，原生适配器使用 6379）、`headTaskName`、
`totalNodes` 及非负节点 rank。多个 Kubernetes worker 必须使用一致的 head 身份
和总节点数，以及不同的 rank。拒绝 head 模板、Docker 模板、旧式绑定、模板级
suspended 和环境 add-on。Raw 命令须配置为同一 head；控制器不会从任意进程参数
中推断这一关系。

先创建 suspended 原生 Job，查询其 UID，再创建精确绑定，最后使用
UID/resourceVersion 前提激活 Job。通过匹配实际原生 Node 标签的 Node selector
选择 B；Raw 分配仍使用整节点 GPU 独占。现有 UI Raw Job 表单尚不提交此混合契约。
使用新字段前安装重新生成的 ContainerJob CRD，并一起更新扩展控制器和执行 API。
Ray group plan 成员使用以 `rank` 为键的 Kubernetes map list；需重新生成并安装
对应 CRD，不能仅复制新的目标字段。

## 结果、停止与恢复

Raw 运行时效果仍经过独立启用的 mTLS Task API 和 Agent。原生 Kubernetes Agent
拥有其 Task 运行时和清理 finalizer；扩展不会为 Kubernetes Task 创建 Raw allocation。
Job 汇总各运行时的新鲜观测，不能仅凭 Task 已创建就判断运行成功。

原生 worker 自然成功或失败时，扩展先在自己拥有的注解中保存有界的
Task UID/generation 结果凭据，再持久设置 `spec.suspended=true`，等待原生 Host
完成运行时清理并上报当前 generation 的 Stopped，保留最初结果。Raw 清理需要
精确 supervisor/allocation 证明及 allocation 确认消失。终态 Task 保留，避免
控制器重启后重新创建一次执行。结果注解和
`containers.rlark.io/kubernetes-task-protection` finalizer 归扩展所有；管理员不能
通过手工修改它们来冒充清理确认。

Job 暂停、删除、重启或执行身份变化时，先退役整套旧 Task，再允许创建替代。
停止请求向所有成员展开，因此 Kubernetes 清理阻塞不会饿死 Raw 清理。扩展等待
原生 Host 移除自己的 finalizer 后，只移除扩展自己的保护，再确认子对象消失。
不修改未知 finalizer。清理不确定时阻塞替代执行，保留身份供管理员恢复。

成员目前独立启动：尚未实现 gang 预留、Fleet 启动屏障、成员失败后自动取消兄弟
成员，以及对任意 Raw 命令内部同一 head 的验证。Job Running 仅表示至少一个
成员已报告 Running，不代表所有 Ray 成员已就绪。原生 Kubernetes 执行仍通过
Agent namespace 消费 Task。更新后的原生 Agent 在认领和执行前校验规范注解
`rlark.io/target-cluster-uid`，要求精确 Cluster UID、租户及唯一的
`spec.agentNamespace` 归属一致。冲突的历史注解会被拒绝，未绑定目标的历史 Task
保留原兼容语义；删除仍使用原有的归属清理流程。使用此目标校验前必须部署更新后的
原生 Agent。退役目标 Cluster 或复用 namespace 前仍应先停止混合 Job。
不能把此预览当作完整 Fleet 编排能力。

## SSH 引导与运行时流量

A 授权 RLark 服务机的 SSH 公钥。B/C 在 root 的 `~/.ssh/authorized_keys` 中
授权 A 用于安装的 SSH 公钥，私钥保留在 A。Notebook 重启保留了授权时，无须
重复添加。替换实例接入时固定并验证 SSH 主机身份；私钥不进入 UI 表单、状态、
日志或工作负载进程。

SSH 用于安装和升级，不承担推理传输。Agent API 经过 A 公网 30000 管理网关。
可选 TLS/smux Ray 数据网关使用 A 的 30001，计算节点直接连接它，推理数据不经过
本地 RLark 服务。peer 配置及进程级中继注入仍由管理员管理，须完成真实双向
Ray/Gloo Channel 验收。peer JSON 配置可选 `localAddress`，只为到 A 的连接指定
已存在的单播源 IP。源地址不可用时连接失败并重试，不会回退到其他源地址。此设置
不会修改 Ray 目标地址或路由，也不能绑定 peer 所在网络命名空间之外的地址。中继
预览不要求 HTTP proxy、SSH 转发、WireGuard、TUN 或全局路由变更。保留现有
Running Job 和 Ray head；Lumo2/S1U 测试仅用 Passive。

原生 Job、Raw 集群和各 Kubernetes 目标集群必须具有相同租户身份。这是 RLark
授权边界，与 Ray 或网络要求无关；此预览不支持跨租户共享。改变测试资源归属需要
明确的管理迁移并重新签发绑定租户的节点凭据，只改标签并不充分。

## 2026-11-03 云端准备证据

空闲的云端测试资源已由管理员统一到机器人现有租户。迁移记录了精确对象 UID 和
resourceVersion，备份对象，暂停受影响控制器/Agent，确认没有 allocation 或 Job，
并在重启 B 前轮换节点凭据。机器人的归属及跨租户校验保持不变。这是运维恢复，
不是通用租户迁移 API；归属变更后不能复用旧的注册请求或证书。

A 的现有管理 gateway 已在同一进程中同时提供 30001 TLS/smux 数据监听。合成请求
通过 A 公网 30001 到达 B 私网 peer 并完成往返，之后已停止临时探测进程并撤销其
网关授权。此结果只验证已部署传输，不代表机器人 peer、Ray 成员、Gloo Channel
或推理请求通过。Agent 心跳及 TCP 端口监听不能替代就绪验证。

B 保留原有 Lumo2 环境，通过独立依赖目录提供 Ray 2.59.0 和 Fleet 缺失的 HTTP
依赖。云内镜像仓库没有该 Ray 版本，因此在管理机从官方 Python 包索引下载 wheel
后安装到 B，未修改全局 proxy 设置或已有环境。所选 checkpoint 的 step-30000
Hugging Face 目录、processor、BAT 权重和统计路径均通过存在性检查，Fleet rollout
及后端模块导入通过。随后有时限的独立 GPU 检查约用 102 秒成功加载模型，分配约
9.4 GB 显存。检查退出并释放模型，该模型检查已释放资源。随后有时限的独立预测使用
相同 checkpoint、合成的 29 维状态及三张合成 RGB 图像，约用 200 秒完成，返回
数值有限的 `32 x 29` 动作数组。这只验证云端模型预测：未连接机器人、未下发动作，
也尚未验证通过 RLark Job 执行。

平台停止并重启 B 的 Notebook 后，其私网地址变为 `10.1.3.201`，进程命名空间的
实例身份也发生变化。旧 Agent 正确拒绝以新实例身份启动。管理员确认空闲节点没有
Job、Task、allocation 或升级引用后，退役旧的原生/伴随 Node 实例，使用新凭据将
替换实例注册回同一个逻辑 Cluster。B 的 Agent 与 Raw supervisor 已运行，原生
Node 为 Online，仍保持 cordon。B 的传输 peer 已与 A 私网
`10.1.3.189:30001` 建立连接。这不能证明机器人 peer 已加入，也不能证明
Ray/Gloo 流量通过。

另一次有时限的 B 本机 Ray 2.59.0 检查成功启动本机 head，运行 GPU worker
进程，观测到 `CUDA_VISIBLE_DEVICES=0`，并验证 worker 继承了进程级中继
shim。检查约用六秒完成，随后关闭自己的 Ray 实例。这不验证远程成员或中继
数据路径。Ray 临时根目录需使用 `/tmp/rlk-ray-b-v2` 等短路径：将 session 放在
较长的部署目录下曾因 Linux Unix-domain socket 路径超过 107 字节而启动失败。
Ray 2.59.0 默认启用 token 认证，应向 head 和加入的 worker 配置相同的受保护
token，不能假设两端各自生成的 token 相同。token 不应出现在 Job 清单、日志
或文档中。

最后检查时，RLark 服务机无法访问机器人的 Orin/x86 地址，机器人 Cluster 为
Offline。尚未激活混合 Fleet Job，也未宣称其就绪。当时 B 继续保持 cordon。宣称可用前，需恢复机器人连通性并确认最新地址，对齐 Python/Ray
和 RLinf 依赖，验证已部署 Kubernetes Agent 的显式 head 契约，并完成双向
Ray/Gloo 和 Passive 推理验收。本次部署的专用传输凭据于 2026-11-10 到期；续签
仍需管理员更新 pin 并重启受影响的传输进程。

传输证据和剩余验收门槛参见[网关计划](container-gateway-plan.md)及
[Raw 运行时计划](raw-container-runtime-plan.md)。
