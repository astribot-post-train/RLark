# Docker 主机 Agent 观测（预览）

本次在独立 `extensions/container-runtime` 模块中增加显式、只读的 Docker
主机注册与心跳观测，不下发 Docker Job。Raw 执行及其现有安装包仍为默认模式。
金山云测试 Notebook 没有 Docker socket，目前只能验收 Raw，不能用于验收
此 Docker Engine 路径。

## 实施计划与验收边界

1. 通过管理员 CLI 创建独立 Docker Cluster 并注册绑定确切原生/伴生 Node 的主机。
   `instanceID` 使用主机 `docker info --format '{{.ID}}'` 的 Engine ID。
   显式声明租户、私网作用域、环境与授权 GPU UUID 清单。GPU 可见不代表使用授权。
2. 导出私有的只读 Agent 包，包含 mTLS 证书、Agent、校验启动脚本及 root systemd
   单元，不包含 Raw supervisor、工作负载路径、签发私钥或管理 kubeconfig。
3. 以 `--runtime=docker` 连接主机本地 Unix socket，读取 Engine 信息/版本、托管
   容器占用与本地 NVIDIA UUID。每次观测最多十秒，不启动 daemon、不创建容器、
   不把 socket 挂载进工作负载。
4. 服务端绑定观测的运行时与注册 Engine ID。对象替换、凭证不匹配、信息不完整、
   清单查询失败都不能产生新的 Ready 心跳；继续兼容旧 Raw 心跳。
5. 测试 daemon 故障及恢复、上下文取消、残留容器、Engine 替换、错误运行时、
   凭证撤销及安装包导出，并通过本地 Unix socket 测试官方 Docker SDK。
   提交前构建模块与中英文文档。

`Ready=True` 表示收到当前有效的认证观测；本次所有 Docker 观测均保持
`Schedulable=False`，不公布执行或升级能力。容器创建/启动/停止/删除、限定作用域
的日志/统计、Docker Job 调度、GPU 设备分配、Ray 就绪/端口屏障及 Docker Agent
升级需要后续实现与独立主机验收。现有 provider 单元测试不等于云端 Docker 验收。

## 运维流程

执行 `make -C extensions/container-runtime build`。使用现有
`container-runtime-admin cluster-create`、`node-register`、`node-package`，
通过 `--kubeconfig` 和严格 JSON `--config` 提交。以扩展目录的
`config/examples/provision-cluster.json` 与 `provision-node.json` 为起点，
将集群 `runtime` 改为 `Docker`，使用新的操作 UUID 和返回的确切 Cluster UID。
主机须为独立 Linux Docker 主机，使用 root，具备正常 NVIDIA runtime 与已确认
的 GPU 授权。不使用 Docker-in-Docker，也不改造云 Notebook 来伪装主机。

Docker 安装包使用 `extensions/container-runtime/config/examples/provision-docker-package.json`
模板。显式设置 `runtime` 为 `Docker`（省略时默认为 `Raw`），保留通用证书、
产物和身份字段，
`dockerHost` 使用 `unix:///var/run/docker.sock`（默认值），省略
`supervisorBinary`、`stateDirectory`、`executables`、`workDirectories` 和
`logBytes`。不启用执行、日志或升级；不支持的选项在创建输出目录前拒绝。
使用全新私有输出/安装目录，在管理员机器导出后安全复制整个目录到主机的确切
安装路径，禁止工作负载写入。签发私钥与 kubeconfig 不离开管理员机器。
自动注册与续期尚未实现。

生成的 `start-agent` 支持前台运行。若主机有 systemd，把
`rlark-container-agent.service` 安装到 `/etc/systemd/system/`，执行
`systemctl daemon-reload` 和 `systemctl enable --now rlark-container-agent.service`。
单元排在 `docker.service` 之后，不负责安装或启动 Docker。自定义 socket 只支持
规范的绝对 `unix:///...` 路径；远程 TCP/TLS daemon 不是此 Agent 的支持契约。

使用导出凭证时，手动等价命令为：

```bash
/opt/rlark-docker/container-runtime-agent \
  --runtime=docker \
  --docker-host=unix:///var/run/docker.sock \
  --endpoint=https://container-api.example.internal:9444 \
  --tls-cert=/opt/rlark-docker/agent.crt \
  --tls-key=/opt/rlark-docker/agent.key \
  --server-ca=/opt/rlark-docker/server-ca.crt \
  --version=preview-v1 \
  --private-address=10.0.3.85
```

Docker 模式不需要 supervisor socket。Raw 为 `--runtime=raw`（默认）且要求
`--supervisor-socket`。混用运行时专属参数或启用未支持的 Docker 操作均验证失败。

## 观测与恢复语义

容量为 daemon 报告的 CPU/内存总量及管理员授权 GPU 清单，不代表空闲资源或任务
利用率。要求 Docker Engine API 主版本为 1、版本至少为 1.42。NVIDIA runtime
存在只证明配置，不证明 GPU 容器启动已通过验收。所有授权 UUID 必须可见，且存在配置好的 NVIDIA runtime。运行时或版本
信息缺失/未知时观测失败。旧 Raw 心跳可以省略新增字段；Docker 心跳必须带
`runtime: Docker` 与确切 `instanceID`。额外可见 GPU 不扩大授权。

daemon 中只要存在托管容器（包括已退出/dead 容器），就保持 `Busy=True`，直到
由可证明的所有者删除。这是保守的本地观测，不是分配锁，也不授予接管/删除权限。
不修改无标签的外部容器；主机需专用，因为本次观测不为外部容器预留端口或 GPU。

Docker info/version/list 或 NVIDIA 查询失败时不发送心跳，按有界指数退避重新
读取状态，不重放旧成功观测。控制器以 45 秒为过期门槛，每 30 秒修复 Ready 状态。
同一 daemon 恢复后继续观测；Engine ID 改变时服务端拒绝。应 cordon 并按正常清理
流程退役确切旧 Node，再注册新实例、签发新凭证；不编辑旧绑定、不复用旧凭证、
不强删清理状态不确定的 allocation。
