# Notebook Fleet 完整交付计划

状态：实施中。本文是交付清单，不代表端到端 Fleet 已受支持。每项验收必须标明
被测试的部署，区分自动化测试与真实基础设施结果。

## 已确定的架构

A/B/C 是同一个 RLark 逻辑容器 Cluster 中的现有 Notebook 容器，使用 Raw 运行时，
不需要 Docker socket、systemd、TUN 或再次安装 Kubernetes。A 是网关兼计算节点，
B/C 是 Worker。RLark 通过 SSH 安装 A，A 使用自己预配置的 SSH 身份安装 Worker；
私钥不经过页面。

机器人 x86 保持在原有 Kubernetes Cluster。一个原生 Job 及扩展绑定拥有云端和
机器人 Task。RLark 将唯一 Ray head 与 B 的 rollout 调度到同一个 Raw 执行单元；
Env worker 加入同一个 Ray 集群。停止 Job 时清理这个 head 和后代，不接管或停止
现有 Job 及独立 head。

```text
RLark -- TLS 管理 :30000 --> A -- 私网 --> B/C
                             ^           B: head + rollout
                             |
                     TLS 多路复用 :30001
                             |
                      机器人 Env Pod --> S1U
```

推理数据绕过本地 RLark。SSH 只用于安装和控制，不使用 HTTP proxy、SSH 隧道、
WireGuard 或全局路由修改。A 公网使用管理 30000 和数据 30001，暂不使用 30002。
内部 Ray、Gloo 和 Fleet Driver TCP 复用数据网关。首版每个网关只接纳一个活动的
跨集群 Job。checkpoint 和模型设置属于工作负载配置，不属于集群或模型目录配置。
不实现 GPU 保活，也不实现 Notebook 电源管理。

## 实施顺序与验收

1. **基线收尾：**移除 GPU 保活的 API/UI/打包/服务行为，修复旧 UI 响应测试，
   重新生成 schema，补接入、安装器和服务管理器失败恢复测试。保留正常 GPU 发现
   和分配。Go 与 UI 检查全部通过。
2. **一键接入：**同一页面流程创建 Cluster、保存拓扑并固定版本，安装 A、建立
   管理链路，再经 A 安装 Worker。UI/CLI 共用操作 ID 和权威资源。分别显示 SSH、
   Agent、服务和数据就绪。Worker 部分失败保留已成功进度。真实验证 A/B 接入，
   C 可用时补新增节点验收。
3. **维护：**替换/退役绑定精确 UID，撤销凭据前检查 Job、allocation 和升级引用。
   清理不确定则禁止复用，不静默接管新实例/IP。管理器重启保留节点服务，整套包
   通过校验。Worker 空闲升级，A 转发活动 Job 时也视为忙碌。凭据按精确身份续期，
   破坏性传输变更等待空闲。
4. **托管网络：**Job 拥有网络会话，固定参与者、Task/执行身份、地址与内部端口。
   私下分发凭据，自动为 Raw 进程树和机器人 Pod 注入 Linux/glibc TCP 连接组件。
   机器人使用共享 Pod 网络的传输 sidecar，明确配置 IPv4/Gloo 网卡。断线使既有
   流失效，重连不重放数据，也不承诺 Ray 无损恢复。先验证机器人实际网络到
   A:30001 出站，再验合成流量、Ray Actor/对象和双向 RLinf Channel。
5. **混合 Job：**预留 B、确定 head、创建 Env、配置传输、启动 head、加入 worker、
   验证成员及 Channel，最后启动 Fleet。分别上报放置、传输、head、成员、Channel、
   业务和清理条件。Env 资源仍由 Kubernetes 调度；不承诺跨运行时原子 gang 调度。
   超时清理部分启动，关键成员失败停止兄弟成员，显式重试等待旧执行清理，不无限
   自动重试整组，也不全局杀进程。
6. **页面：**新增混合/Fleet 专用提交表单，选择云端/机器人目标并提交各角色资源、
   命令、环境和配置。模型参数作为业务输入传递。展示唯一 head、成员关系、启动
   进度、有界日志和停止清理结果；API/CLI 创建的 Job 同样展示。本轮不要求额外
   暴露完整远程 Fleet Dashboard。
7. **真实验收：**重新读取当前 Notebook 实例/IP 与机器人状态，不假设历史地址
   仍在线。验证 B CPU/CUDA Job 生命周期、模型加载、同一 Ray cluster ID、Channel
   和真实 S1U Passive 推理。至少 10 分钟且完成 20 次观测/预测往返，记录加载耗时、
   延迟、失败和丢帧。停止后证明所属资源清理，保留 checkpoint，再次提交成功。

工作负载验收使用
`/ks3-regular/ss/checkpoints/test/lumo2_1022_exp1_pack_anything_pack_the_suitcase_20261023142019`。
processor、权重、归一化和接口由 Fleet 校验。Passive 不开启 Active、不下发执行
动作。保留机器人已有 Job，资源无法安全并存时等待。

## 实现与兼容边界

运行时、接入、网络和控制器集中在独立 `extensions/container-runtime` 模块，UI
组件留在 RLark 主界面。核心只增加必要且经过测试的通用适配，包括原生 Kubernetes
Agent 消费扩展 Task 前验证目标 Cluster UID。持久拓扑、一次性启动/替换操作和观测
回执分开，所有入口及直接管理 API 都校验归属、租户、UID 和 generation。

扩展现有类型化 Cluster/Node/Job 附属资源和版本合同，不开放任意远程命令。
从类型源生成 CRD/deepcopy。保留单进程 Raw 提交、Agent 单独发布和旧 Gateway
读取兼容。对应验收通过前均标为预览，基础设施阻塞与代码缺口分开记录。

## 验证与进度

实施中运行聚焦失败恢复测试，最终运行扩展 Go 测试/vet、受影响原生 Go 测试、UI
测试/构建、仓库 lint/tests、文档构建和示例/schema 校验。真实云测试需要明确输入，
跳过云测试不能算验收通过。

2026-11-04 已验证：扩展两个 Go 模块、受影响的原生 Task 包及全部 155 项 UI
测试通过，UI 生产构建通过。接入已补 SSH 中断恢复、保留操作重试、身份冲突、
安装器与服务管理器重启的专门测试。新安装包在启动或接管服务前阻止旧实例复用。
本地受限测试用户不能读取 `/proc/1/ns/pid`，确定性测试使用临时 fixture 覆盖 PID
namespace、PID 1 启动时间及 boot ID 变化。
自动传输注入与云端到机器人完整 Passive 验收尚未完成。

### 2026-11-04 部署检查点

管理 API 重启后的认证已恢复，已部署新版 Notebook CRD、接入控制器和启用接入
入口的原生 Gateway。保留操作通过严格 SSH 安装 A，探测容器私网地址，启动管理
网关和 Raw supervisor，随后收到 `preview-enrollment-20261104` 的认证心跳。
原生 Node `ksyun-a` 和 Cluster `ksyun-containers` 已观测为 Online。A 上已有独立
归属的 Fleet/Ray 工作，因此保持禁止调度，未接管或停止已有工作。这只证明 A
管理接入成功，不证明 Ray 数据就绪、云端作业启动、升级验收或持续可用性。

B 的平台 SSH 地址 `10.0.3.85` 曾通过 A 登录成功，容器内部地址为 `10.1.2.82`，
可见一张 H100。但安装完成前，平台已在北京时间 18:26 报告 B 为 stopped。
B 没有新的注册 Node 或 Agent 心跳。保留的 A/B 操作保持 Waiting，B 显示
`SSHUnavailable`，不能报告为集群已完整接入。不会自动启动已停机 Notebook。
运维人员重新启动后须重新读取两层地址和容器身份；不可变目标变化时创建新操作。
增加或启动其他参与节点时，须保留已有归属节点的精确 UID。

真实安装暴露并修复了 A/B Python 路径差异、私有升级目录缺失及 API 服务叶子证书
过期地址问题。profile 已支持探测地址占位符和按节点配置规范路径白名单，服务
管理器在启动前检查路径。必须先应用匹配的 CRD，再启动新版控制器：旧 schema
可能裁掉节点角色等字段。被旧 schema 裁剪的注册须经过正常归属/清理检查后退役
并显式重建，不能修改不可变身份字段或移除 finalizer 绕过冲突。

该检查点原生完整 Go 测试、扩展两个 Go 模块、扩展 vet、全部 155 项 UI 测试、UI
生产构建及双语文档构建已通过。尚未完成 B rollout、云端到机器人 Channel、
十分钟 Passive 运行及停止/再次提交验收。已有 Job `jo-a12e2db6627847af` 不属于
此次接入操作的管理范围。

当前合同见[控制台](container-runtime-console.md)、[网关](container-gateway-plan.md)
和[混合 Job 预览](mixed-container-job-preview.md)。
