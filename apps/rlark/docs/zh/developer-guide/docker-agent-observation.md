# Docker 主机 Agent 与执行（预览）

独立 `extensions/container-runtime` 模块提供 Docker 主机观测与按需开启的单容器
执行。默认 Docker 安装包仍只观测。金山云测试 Notebook 没有 Docker socket，
已通过的 Raw GPU 测试不代表 Docker Engine 验收。此路径不使用 Docker-in-Docker，
也不把宿主机 socket 挂载进工作负载。

## 前置条件与所有权

使用专用 Linux Docker 主机、root、主版本 1 且不低于 1.42 的 Engine API、
NVIDIA Container Toolkit/runtime、私网 IPv4 连接，以及管理员授权的整卡 UUID
白名单。通过独立管理员 CLI 创建单独的 Docker Cluster。每台主机对应一个
ContainerNode，绑定原生 Cluster/Node UID、租户、私网地址/作用域、环境、凭据和
Engine ID（`docker info --format '{{.ID}}'`）。

容量来自 Engine CPU/内存总量及授权 GPU 清单，不代表空闲容量；GPU 可见性不扩大
授权。host network 与 root Agent 要求主机可信且专用。外部容器永不接管或删除，
RLark 不为它们预留 GPU 或端口。

## 安装包与启动

执行 `make -C extensions/container-runtime build`。使用
`container-runtime-admin cluster-create`、`node-register`、`node-package`，
携带管理员 `--kubeconfig` 和严格 JSON `--config`。以扩展目录下
`config/examples/provision-cluster.json`、`provision-node.json` 和
`provision-docker-package.json` 为起点，设置 `Docker` runtime，重试时保留操作
UUID，并使用返回的确切对象 UID。

包内包含 Agent、节点 mTLS 凭据、公开 API CA、校验文件、前台启动脚本及排在
`docker.service` 之后的 root systemd 单元，不含签发私钥、管理 kubeconfig 或 Raw
supervisor。通过私密方式复制到确切安装目录。自动注册/续期及 Docker Agent 升级
尚未完成。只支持规范的本地 `unix:///...` socket，不支持远程 TCP/TLS Engine。

只观测时保持 `enableExecution: false`，省略 `stateDirectory`。执行时设置
`enableExecution: true` 和独立于安装目录的
`stateDirectory: /var/lib/rlark-docker`。首次启动前由 root 创建模式为 0700 的持久化
目录。保持 `enableLogReads`、`enableUpgrades` 为 false，省略
`supervisorBinary`、`executables`、`workDirectories`、`logBytes`。不合法组合在写出
安装包前拒绝。

使用导出凭据的手动启动命令：

```bash
# 仅首次安装。后续必须保留该目录及记录。
install -d -m 0700 -o root -g root /var/lib/rlark-docker
/opt/rlark-docker/container-runtime-agent \
  --runtime=docker \
  --docker-host=unix:///var/run/docker.sock \
  --endpoint=https://container-api.example.internal:9444 \
  --tls-cert=/opt/rlark-docker/agent.crt \
  --tls-key=/opt/rlark-docker/agent.key \
  --server-ca=/opt/rlark-docker/server-ca.crt \
  --version=preview-v1 \
  --private-address=10.0.3.85 \
  --enable-task-execution=true \
  --state-directory=/var/lib/rlark-docker
```

systemd 主机安装生成的 `rlark-container-agent.service` 后，执行
`systemctl daemon-reload` 并启用/启动单元；它不负责安装或启动 Docker。
Raw 仍为单独的默认模式，要求 `--supervisor-socket`。

## 调度与生命周期

安装伴生 CRD 和控制器/API RBAC。除 Agent 开关外，还需开启控制器
`--enable-job-reservations=true` 和 API `--enable-task-execution=true`。使用
`config/examples/docker-container-job.yaml` 及对应 ContainerJob 绑定，替换租户、
Cluster UID、namespace 和预置环境后再解除 Job 暂停。独立控制台的 Job 提交表单
仍只接受 Raw。

对于普通容器 Job，控制器创建一个 Task 与独占整台主机的 ContainerAllocation，并写规范 Task target
注解，不写旧 DockerBinding 或按数量分配的 GPU reservation。Job 必须只有一个
`docker.rlark.io/container` v1 template/carrier。支持显式 image、entrypoint、
command、环境变量、工作目录、CPU/内存限额；拒绝 Pod 模板、卷、Secret、环境注入、
用户 GPU 分配及 Ray 专属字段。allocation 的全部 GPU UUID 通过 NVIDIA DeviceIDs
注入。一台主机同时最多运行一个 RLark Task。

`Ready=True` 表示新鲜的认证观测。只有显式开启执行的 Agent 才声明
`docker-container-execution-v1`，只观测节点保持不可调度。新分配还要求身份、目标
release、新鲜空闲观测有效，相关资源未 cordon。忙碌或失联不释放已有 allocation。
API 在 claim 和紧邻 create/start 前重新验证确切 Job/Task/allocation/Node、租户、
凭据、generation 与所有权。Agent 不持有 Kubernetes 凭据，Task 协议不能创建分配。

`ExecutionKnown` 记录当前有界的容器 phase。自然退出先写 Succeeded/Failed 和
`ExecutionCleaned=False`，保留结果及分配。后续清理先持久化取消，再 stop/remove，
按 ID、确定性名称和 inventory 确认不存在后，才能公布 `ExecutionCleaned=True` 和
allocation 的 `CleanupConfirmed=True`。暂停/删除也使用此屏障，清理不要求 Ready。
终态 Task 身份在释放分配后保留，防止重启重跑。

## 故障与恢复

Agent 独占锁定私有 journal 目录，并在进程内以支持上下文取消的方式串行化 Engine
操作。stop/remove 前 fsync 取消记录，ContainerCreate 前才写创建意图；追加式索引
检测单条记录丢失。迟到请求不能重启已取消的 Task UID，包括 Agent 重启及停止先于
创建的情况。

创建前 inspect；响应丢失时，通过确切本地创建记录及所有权 labels 恢复。镜像仓库/
创建前失败可重试。create 超时且未取得容器 ID 时，即使当前 Docker 返回不存在，
结果仍不确定，必须保留 allocation；之后确切容器出现时可清理。没有自动超时释放。
记录损坏/丢失、目录权限变化、锁替换和 Engine 身份变化均拒绝继续。必须保留整个
journal，包括已释放分配的取消记录。不要删除并重建目录以恢复旧节点；整个目录丢失
时，必须由管理员先隔离并退役确切旧节点，再注册新实例。

info/version/inventory/NVIDIA 观测失败不发送心跳，以有界退避重新读取。控制器以
45 秒为过期门槛，并定期修复状态。Engine 故障仍可先写本地取消，但不能给出清理
证明。Engine 替换需先退役旧节点，再新注册/签发凭据。不要强删不确定的 allocation，
也不要复用旧凭据。

## 有界日志与资源快照

诊断默认关闭。制包时同时设置 `enableDockerDiagnostics: true` 和
`enableExecution: true`，或在上面的执行 Agent 命令中增加
`--enable-docker-diagnostics=true`。保持 `enableLogReads: false`，该开关只用于
Raw supervisor。Docker Agent 只有显式启用后才声明 `docker-diagnostics-v1`。

为处理 Agent 轮询的同一个 API 进程配置 `--enable-task-execution=true`、
`--log-read-addr=127.0.0.1:9446` 和
`--gateway-jwt-key-file=/private/gateway-jwt.key`；控制台配置
`--log-api-origin=http://127.0.0.1:9446`。回环监听器只提供管理员 GET 路由
`/api/v1/container-runtime/logs` 与 `/api/v1/container-runtime/stats`，均严格要求
`namespace`、`allocation`、`allocationUID`、`taskUID` 四个参数。等待前和返回前
都校验浏览器的 Gateway 管理员 JWT。Agent 通过独立私有 mTLS
`/v1/docker-diagnostics` poll/complete 接口读取，不接收此 JWT。Raw 保留原有
`/v1/logs` 接口及 supervisor 语义。

每 Docker 节点最多一个在途诊断请求，每 API 进程最多 32 个 Docker 请求，等待
不超过 20 秒。API 重启会丢弃请求，需手动重试。Agent 轮询和回环读取监听器必须
指向同一个 API 实例，不支持跨实例 broker 路由。独立 Agent worker 每个
poll/read/complete 步骤最多十秒；读取响应正文不持有执行锁，卡住的读取不会阻碍
清理。

每个边界重新校验确切 allocation/Task/Job/绑定/Cluster/Node、租户及凭据。
Task generation 变化、清理确认、分配释放、能力撤销及身份替换都会拒绝读取，
包括已经进入缓冲区的结果。仅 cordon 或心跳过期不撤销保留所有权。本地读取前后
均校验 Engine ID、journal 创建记录、Task generation、容器 ID 和所有权标签。
本地取消后停止读取；与清理并发的结果被丢弃。所有权不明、TTY 容器、记录缺失
或响应格式错误返回不可用，不暴露本地错误路径。工作负载输出本身可能包含秘密，
应仅由授权管理员启用并使用此预览。

日志按 Engine 顺序合并 stdout/stderr，不添加流标签、不 follow，返回当前可用
输出的开头，解码后最多 64 KiB，并带截断标记。帧处理限制为 1 MiB 和 4096 帧。
这不是历史归档或 tail API。资源快照是一次 Linux Engine 采样，包含 `sampledAt`
及可选规范无符号 64 位十进制字符串：`cpuUsageNS`、`memoryUsageBytes`、
`memoryLimitBytes`、`pids`、`blockReadBytes`、`blockWriteBytes`。缺失计数保持
缺失，只有实际观测到零才报告零。CPU 为累计纳秒，内存和块 I/O 为字节，PID 为
数量。stats 响应正文最多 1 MiB。单次快照不提供 CPU 百分比；GPU 利用率和
host network 流量归属仍不可用。日志与快照均不写资源 Status 或指标数据库。

独立控制台在 allocation 行提供手动读取。Docker 读取要求声明诊断和执行能力，
且 allocation 尚未清理。未知计数显示不可用，输出作为转义文本显示。切换选择、
退出登录、allocation 替换/清理及能力撤销会清除显示并取消旧请求。API 重启、
daemon 不可用或已退出容器的 stats 不可用时需重新读取，不合成零值或自动重试。

## 验证与剩余范围

测试覆盖 fake Engine 的响应丢失、不可变配置、所有权、整卡 UUID、取消、进程锁、
重启、故障/恢复，以及真实 mTLS 和官方 SDK Unix socket 传输下的 Running、成功、
失败、暂停与清理。诊断测试还覆盖有界 multiplex 解码、计数缺失/零/溢出、精确 ID
SDK 读取、凭据和能力撤销、临时 broker 重启/重试，以及正文卡住时并发清理。
传输测试中的 daemon 是模拟服务，不代表真实 Docker GPU 已运行。

GPU 利用率、内核级端口预留、Docker 滚动升级和真实 GPU 主机生命周期验收仍需
单独推进。Docker Ray 整组屏障已作为预览实现；Raw Ray 编排仍不支持。
有序门槛见 [Docker 执行计划](docker-runtime-execution-plan.md)。


## Docker Ray 整组预览

先安装重新生成的 ContainerJob/ContainerAllocation CRD，再配套升级控制器、API 和
执行 Agent。沿用控制器 reservation、API execution 和 Agent execution 开关；
只观测 Agent 不声明 `docker-ray-group-v1`。控制台提交表单仍仅支持 Raw。
管理员提交暂停的原生 Job 及精确 UID 的 Docker ContainerJob 绑定，模板使用
`rlinf.io/ray` v1 workload 和 `docker.rlark.io/container` v1 carrier。
参见[执行契约](docker-runtime-execution-plan.md)与配套示例清单。

整组必须只有一个 head，合计最多 16 个副本。每个成员需要独立主机、新鲜认证观测、
匹配的预置环境、相同私网作用域以及完整授权的独占 GPU 集合，只支持 host network。
镜像须提供 Python 3、Ray **2.56.1**、POSIX shell 及训练依赖，Agent 不自动安装。
每台主机同时只运行一个 RLark 成员；同一 Cluster 中不同 Job 可使用不重叠主机并行。
本功能不为 Raw 进程 Job 增加 Ray 屏障。

控制器拥有 `ContainerJob.status.group`：不可变执行身份、精确 Task/节点/Engine/
凭据/成员计划与摘要，以及 `Allocating`、`Preparing`、`Starting`、`Stopping`、
`Succeeded`、`Failed` 阶段。取得锁前先计算完整落点。每次 allocation 创建独立原子，
竞争冲突会取消本次尝试，已暴露的分配必须完成清理；不承诺跨对象事务、公平性或抢占。
创建响应丢失时沿用原身份和主机重试。API 独占写入 allocation Status 中的精确准备
回执；Agent 发回执前先将本地容器身份持久化到私有 journal。

mTLS Task 协议新增 `prepared` 和 `authorize-start`。所有成员回执匹配后，控制器
才进入 `Starting`，每次容器启动又重新校验当前完整组。`StartPrepared` 仅启动
journal 中的精确容器 ID，已准备容器消失时绝不重建。这是协调屏障，不是原子同时
启动。容器 Running 不等于 Ray 就绪：镜像内 launcher 验证精确 GCS 地址集合后才
执行 head 训练。初始容器启动期限为十分钟，launcher 最多等待 120 秒完成 Ray
成员就绪。任何 worker 自发退出（含退出码零）都会导致整组失败。head 成功后停止
worker，必须等全组精确清理证明和 allocation 已观测删除，才报告成功。暂停、删除
或重启会撤销启动并退役整个旧集合。保留终态身份，避免 reconcile 静默重跑。

当前控制器选择 TCP **24000..24264**：九个服务端口和 256 个 worker 端口，写入
全体不可变 allocation。`rayPorts`、已解析地址、rank、GPU UUID 为内部材料，
公开容器 Task 不能设置。create/start 前通过本地 IPv4/IPv6 预检查拒绝外部监听；
检查 socket 在 Docker 启动前释放，因此不是内核级预留。管理员仍需运维保留该端口
块，并另行配置 NCCL/训练流量。

测试通过真实 mTLS 和模拟 Engine，覆盖完整/部分准备、两个 Job 竞争、worker
退出、head 完成、暂停/重启、allocation 响应丢失、凭据/generation/摘要/回执变化
及清理屏障。真实双机 Docker GPU/RLinf 验收仍待完成。金山云 Notebook 没有
Docker socket，仅能证明另行记录的 Raw 验收。
