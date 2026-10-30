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

控制器创建一个 Task 与独占整台主机的 ContainerAllocation，并写规范 Task target
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

## 验证与剩余范围

测试覆盖 fake Engine 的响应丢失、不可变配置、所有权、整卡 UUID、取消、进程锁、
重启、故障/恢复，以及真实 mTLS 和官方 SDK Unix socket 传输下的 Running、成功、
失败、暂停与清理。传输测试中的 daemon 是模拟服务，不代表真实 Docker GPU 已运行。

限定 Docker 日志/stats、利用率、端口预留、Ray/RLinf 集群屏障、Docker 滚动升级和
真实 GPU 主机生命周期验收仍需单独推进。host network 本身不构成多主机 Ray 契约。
有序门槛见 [Docker 执行计划](docker-runtime-execution-plan.md)。
