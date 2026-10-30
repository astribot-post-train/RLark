# 固定 GPU 容器的 Raw 运行时

状态：实现中。本文是交付计划，不代表其中所有能力已经可用。

## 已确认范围

多个现有 GPU 容器作为独立 Node 加入同一逻辑 Cluster，每个容器内运行一个 Agent，使用预装环境执行进程。不需要 Docker daemon、socket、创建容器或 Docker-in-Docker。一个容器同时独占运行一个 Task，一个 Job 可以占用多个容器。首版支持同一云容器组内的 Ray/RLinf，不支持云 Raw 与机器人 Kubernetes 混合 Job。容器私网地址已确认互通，训练地址与控制面连接地址分别配置。

## API 与所有权

复用 Cluster/Node/Job/Task，Cluster.spec.agentNamespace 是成员关系的权威来源，标签不是所有权凭据。记录 Node UID、容器实例、执行和分配标识。Agent 写能力/环境/健康观测，控制器写资源预留。新增运行时无关副本字段，兼容无歧义的 Kubernetes 旧字段并拒绝冲突；Raw 副本展开为独立 Task，索引/rank 稳定，同时聚合模板与实例状态。节点级 claimant 不得接管同 namespace 的其他节点 Task；直接创建 Task 也不能绕过预留和租户验证。

## 调度与生命周期

使用 API resourceVersion CAS 实现节点独占预留。逐步取得整组资源，控制器重启后恢复，部分取得失败则回滚并退避。未全部提交禁止启动。断连、心跳过期或清理结果未知均保留预留，只能在确认清理完成后释放。手动重试先清理旧组，再创建新执行标识。

流程：全部预留 -> 全部预检 -> head 启动 -> worker 加入 -> 精确验证分配成员 -> 训练仅启动一次 -> 记录结果 -> 整组清理。准备和就绪超时可配置，错误必须阻止训练。head/worker 确认失败后整组停止；断连代表未知，不等于已经失败或停止。用户停止与失败分开；训练进程退出决定成功，不能用后台 Ray worker 的存活期代替结果。

## 执行框架

在已有 Host/Coordinator 内实现类型化 Raw 进程 Provider 和 Ray-on-Raw Adapter，使用小接口注入进程、文件、时钟、API 依赖，不另建旁路执行控制器。外部动作前持久化执行声明。独立 supervisor 跨 Agent 重启存活并保存结果，重新附着必须验证执行标识及 boot/start-time，不能仅凭 PID。隔离执行目录、进程 session、Ray 临时路径和有界日志；只清理证实属于该执行的后代，禁止全局 ray stop/pkill。所有权不明、supervisor 丢失但可能有后代或 PID 重用时阻止节点复用。容器替换不能静默接管旧执行。

## Ray 与网络

显式配置私网节点 IP，以及同一网络共享范围内不重叠的 Ray 端口段；关闭无用 dashboard/client。验证支持的 Ray 版本、精确成员，传播 readiness 错误，实际 bind 失败必须算执行失败。单容器单 Task 本身不能隔离 host network 端口。RLinf/NCCL 连通性独立验证，不承诺任意框架动态端口均可控制。

## 环境、资源与存储

只使用预装 Python/Ray/RLinf，验证绝对路径、版本和能力，不自动装依赖。资源容量来自 cgroup/affinity 而非物理宿主机总量。GPU 可见不代表已授权，必须有确认 UUID 白名单或平台绑定，范围未知拒绝调度；只支持整卡独占。分配与利用率分开，遥测可选且有界。

只使用已有挂载，每个节点验证目录权限，节点身份/状态与执行/日志目录分离。Job 删除和清理保留 checkpoint。stdout/stderr 本地轮转，经已有授权 Gateway/Server/Agent 路由提供 tail/since/follow，不允许任意路径/PID。凭证按节点限定，不能传入工作负载环境。

## 接入与界面

rlarkadm 生成配置和前台启动包，可接已有 supervisor，不假设容器内有 systemd。展示 Raw 能力、环境、健康、占用；Job 选择节点、环境、入口而非镜像。展示预留、预检、Ray、训练、清理条件，每副本日志以及手动停止/重试。首版不含通用终端/SSH 注入、TensorBoard 代理、动态 PVC 或云平台调度适配器。

## 交付清单

- [x] 从确认的父分支创建功能分支。
- [x] 中英文设计与交付计划。
- [ ] 类型 API、schema/deepcopy 生成、兼容及所有权测试。
- [ ] 持久化独占预留、回滚和恢复。
- [ ] 通过 Host/Coordinator 完成单节点 supervisor 执行。
- [ ] 多节点 Ray 屏障及整组生命周期。
- [ ] 节点注册、凭证、容量和环境校验。
- [ ] rlarkadm 启动包、授权日志、Gateway 和 UI。
- [ ] 中英文示例、能力矩阵、部署和运维文档。
- [ ] 定向及仓库 Go/web/doc 检查，附证据的草稿 PR。

## 验收与故障测试

覆盖并发抢占、部分回滚、控制器重启、外部动作后崩溃、Agent 重新附着、PID 重用、supervisor 丢失、孤儿后代、端口占用、GPU/环境未知、跨节点/租户认领、head/worker 失败、断网、停止与失败区分、checkpoint 保留、Kubernetes 兼容。CPU 假实现和进程集成测试本地运行；真实双容器 Ray/GPU 与最小 RLinf 验证需要准备好环境并确认 GPU 授权，不自动安装或启动云训练。未验证行为明确列出，端到端验收前保持 preview。

## 当前实现证据与剩余接线

本分支目前提供内部基础组件，尚非可运行的 Raw Agent。`agent --agent-type=Raw` 尚未接入这些组件。不能仅因 schema 增加字段就注册生产 GPU 节点或提交训练。生成的 `status.raw`、`status.reservation` 和 `spec.placement.binding` 是实验契约；向节点身份开放前，必须完成凭证和服务端状态字段写权限约束。

已实现的边界：

- `rawallocation.Manager` 使用实时读取和乐观锁状态补丁，实现整节点组预留、逐步提交、相同执行恢复和未提交预留回滚。环境标识固定在预留中。组校验检查当前成员关系、节点/容器身份、心跳、禁止调度/排空、租户、私网范围、不同训练 IP 和组内 GPU UUID 不重叠。尚未证明不同组或平台分配之间的 GPU 独占性。
- `AssignTask` 将已提交预留幂等绑定到唯一 Task UID。仅提交预留不能授权执行；节点准入要求组内每个节点均已分配 Task UID，还实时验证每个同组 Task 存在、未暂停、绑定完全一致，并由同一个权威 Job 实例持有。
- `raw.NodeAdmission` 重新读取 Task、所属 Job 和整组预留，拒绝直接创建的 Task、旧 generation、暂停、容器替换、跨租户或不完整组。调用方必须使用不经缓存的管理客户端；生产凭证/RBAC 接线待完成。
- `raw.Provider` 实现已有 Runtime Provider 和 inventory 契约，启动和清理委派给独立 supervisor；未确认清理持续返回处理中。Raw Host claimant 必须精确包含 Node UID，禁止接管旧声明。
- `raw.NewCoordinator` 和 `raw.NewHost` 通过现有框架组装显式工作负载 adapter、版本化 process carrier 解码、节点级声明、执行和状态。进程 manifest 带类型化 API 元数据，namespace/name/UID 必须与 Task 一致；验证和 lowering 不产生进程副作用。已测试 Host 重建、重复协调、同级节点隔离和暂停清理。尚未注册内置 Ray adapter、watch/定期重同步循环或生产入口。Unknown 观测映射为 `Pending`，并设置 `ExecutionKnown=False` 及已观测 Task generation；不能据此认定进程已停止。
- Linux supervisor 在启动前持久化意图，显式支持 cgroup 和可信 root 进程树两种后端，记录结果，通过私有 Unix socket 仅接受 Task 身份。已测试 Agent 客户端重新连接。supervisor 自身重启后将未确认执行标为 Unknown 并阻止复用，不接管 PID。

### 实验 supervisor 前置条件与构建

执行 `make -C apps/rlark raw-supervisor` 构建。该目标按需启用，不表示 Raw Agent 已可用。默认 `cgroup` 后端要求 Linux cgroup v2、`cgroup.kill`，以及内核支持原子 `CLONE_INTO_CGROUP` 放置。运维需提供可写的委派子树和独立非 root 工作负载 UID/GID。工作负载不得迁移到其他 cgroup、写控制文件、读取节点凭证或写 supervisor 状态/socket 路径。普通云容器可能没有所需权限，需先验证委派；不能因此接入 Docker socket，也不应默认要求 privileged 模式。

通过 `bin/raw-supervisor --config /private/supervisor.json` 传入严格 JSON，字段如下：

```json
{
  "backend": "cgroup",
  "stateDirectory": "/var/lib/rlark-supervisor",
  "executables": ["/usr/bin/python3.12"],
  "workDirectories": ["/workspace/training"],
  "logBytes": 10485760,
  "cgroupDirectory": "/sys/fs/cgroup/rlark-workloads",
  "workloadUID": 10001,
  "workloadGID": 10001
}
```

路径必须存在且为规范绝对路径，配置可执行文件前先解析符号链接。状态目录私有（0700），位于工作负载目录之外，跨 Agent 重启保留。授权可执行文件及其父目录必须防止工作负载替换；路径白名单本身不保证文件完整性。训练不继承 Agent/supervisor 环境，仅接收显式环境。cgroup 后端按配置 UID/GID 运行；进程树后端以 root 运行可信任务，私有文件权限不是对这些任务的安全边界。Unix socket 权限 0600，只供可信本地 Agent 身份使用；该 socket 不提供远程日志鉴权。

当前日志只保留每次执行最初 `logBytes` 字节，持续读取并丢弃超额输出；尚未实现轮转、tail、since、follow 或 Gateway 路由。记录和每次执行的 cgroup 目录尚无自动回收。不得删除活动或未知记录来绕过所有权检查。活动期间停止/重启 supervisor 可能留下未知后代；重新创建节点实例前，需取得容器/平台生命周期证据并经过可审计恢复流程。目前故意不提供强制释放/恢复端点。supervisor 不删除私有状态目录之外的 checkpoint。

### 按依赖顺序完成接线

1. 完成生产所有权约束：限定 bootstrap/注册、唯一 Node/实例身份持久化、精确状态写权限、Lease、cgroup/affinity 容量，以及跨所有组的权威 GPU 绑定。证明两个 Agent 不能冒充同一 Node。
2. 增加运行时无关副本校验及旧字段冲突处理，将 Raw 模板展开为稳定副本 Task。先创建暂停/未绑定 Task，再分配 UID、持久化全部绑定，最后允许分发；Job 控制器重启从当前状态重建此流程。
3. 通过 Host/Coordinator 组装 Raw decoder、Ray adapter、provider、生命周期和状态映射；拒绝不支持的 Kubernetes environment、carrier 和 Pod 模板功能，并对已有 Kubernetes 路径做回归验证。
4. 实现类型化 Ray 配置、预检/就绪屏障、有界超时、按网络范围预留端口、精确成员验证和训练结果。全部分配成员验证前禁止训练；区分 Unknown 与失败，确认组失败后停止全部成员。
5. 完成清理/重试所有权：Job 删除/重启或部分分发时，所有 supervisor 确认清理前保留预留。为 supervisor 丢失提供可审计运维恢复；心跳消失不释放容量。
6. 完成前台注册/启动包、授权日志路由及有界保留、资源观测、UI 能力开关与副本进度。链路可用后才添加可执行中英文部署/训练示例。
7. 在两个准备好的容器验证显式选择的监督后端、授权 GPU UUID 和支持的 Ray/RLinf 版本；覆盖端口占用、后代清理、Agent 重启、supervisor 丢失、断网、head/worker 失败和 checkpoint 保留。本地尚未执行这些平台测试。

### 本地验证证据

内部组件通过定向竞态测试和仓库 `make test-go` 测试，supervisor 二进制构建成功，API/client/UI 生成模型已重新生成，中英文文档通过严格 MkDocs 构建。尚未完成真实 GPU、委派 cgroup 或双容器 Ray/RLinf 验收。仓库 Go lint 仍报告网络包中三个已有的未检查 channel 接收；新增 Raw 包未报告 lint 问题。这些证据仅验证实验基础组件，不代表上述交付清单完成。

### 金山云容器预检（2026-10-30）

通过已有 SSH 连接检查了此前确认的开发容器。仅运行短时 CPU 进程及能力探针，没有 GPU 计算、安装依赖、修改平台任务、写父级 cgroup 或重新挂载文件系统。

| 检查项 | 实测结果 | 影响 |
| --- | --- | --- |
| 内核与 cgroup | Linux 5.15；cgroup v2 为只读挂载 | 创建子 cgroup 返回 `EROFS`，cgroup 后端无法在此容器运行；仅存在 `cgroup.kill` 不足以满足要求。 |
| CPU 与内存边界 | CPU affinity 为 192；`cpu.max` 为 `12000000 100000`；`memory.max` 为 `1523639648256` | 仅按 affinity 会高估 CPU 容量，实际 quota 相当于 120 个 CPU。 |
| GPU 可见性 | `nvidia-smi` 列出六张 H100 80GB | 可以枚举设备，但不代表已获分配授权；没有执行 GPU 任务。 |
| 默认 Python | 所检查解释器中没有 `ray`、`torch`、`rlinf` 发行包；PATH 无 `ray` | 此解释器不是已准备好的训练环境；后续已找到预装环境，见下文。 |
| CPU 子进程 | 短时 Python 子进程正常退出 | 普通进程执行可用。 |
| user/PID namespace | `unshare --user --map-root-user --pid --fork` 成功 | 存在替代隔离边界的可能性，但当前 supervisor 未实现。 |
| namespace 后代清理 | namespace PID 1 退出，其子进程创建新 session，父进程按 PID/start time 验证无存活的非僵尸进程 | 内核清理了本次 `setsid` CPU 后代；仅为可行性探针，不代表 Ray/GPU 验收通过，也不是生产所有权/恢复实现。 |
| 私有 proc 挂载 | user/mount/PID namespace 结合 `--mount-proc` 在更改挂载传播属性时返回 `EPERM` | 不能假定允许挂载私有 `/proc`；namespace 实现需要单独设计和验证。 |

当前可查询的开发任务列表没有匹配该 SSH 端点，平台 GPU 分配授权仍未确认，不能用六个可见 UUID 推导授权白名单。当前 cgroup supervisor 在该目标上存在阻塞；需先实现并验证兼容真实容器的执行边界，再继续生产接入。不得静默降级为仅按进程组清理，也不得改成 privileged 模式或重新挂载宿主机 cgroup。

### 可信 root 进程树后端

已确认的云容器模式让 Agent、服务、专用执行 worker 和训练全部使用 root，保留容器原有 PID namespace 和 `/proc`。必须显式选择，cgroup 预检失败不会自动切换。要求 Linux subreaper、pidfd、匹配的 `/proc` 和可信训练代码。不创建额外容器或 mount namespace，不提供 UID 隔离或任务级 CPU/内存硬限制，资源边界仍是外层云容器。一个容器同时独占一个 Task，直到证实清理完成。root 训练可以修改服务文件或凭证，因此这不是沙箱，也不能作为互不信任租户之间的执行安全边界。

构建 `raw-supervisor`，解析安装路径及 Python 符号链接，准备工作目录，传入以下配置（路径需替换为实际规范路径）：

```json
{
  "backend": "process-tree",
  "workerExecutable": "/usr/local/bin/raw-supervisor",
  "stateDirectory": "/var/lib/rlark-supervisor",
  "executables": ["/usr/local/bin/python3.11"],
  "workDirectories": ["/workspace/training"],
  "logBytes": 10485760
}
```

`workloadUID`、`workloadGID` 必须省略或为零，`cgroupDirectory` 必须省略。未知后端、混合凭证/cgroup 配置均拒绝。worker 使用已安装的同一个 `raw-supervisor` 二进制；`--process-tree-worker` 是需要继承协议描述符的内部模式，不是用户训练命令。Agent 重启后连接原服务 socket，不要随 Agent 一起重启服务。

每次执行由服务先持久化意图，再启动唯一专用 worker。worker 在启动训练前启用 subreaper，接收显式环境并保留原 `/proc`。仅内核证明的子进程关系可以授权发送信号。清理时打开 pidfd，通过 `waitid(P_PIDFD)` 再次确认子进程归属，然后反复终止和回收被收养的后代；只有 `ECHILD` 才证明清理完成，进程名/PID 列表为空不够。禁止按 root UID、命令名、端口或进程组批量清理，禁止全局 `ray stop`/`pkill`。主进程正常退出后仍清理后代。停止先向主进程发送 SIGTERM，等待 30 秒后使用 SIGKILL；主进程强制退出确认和后代清理各有 30 秒上限，任一无法确认都阻止复用。

服务必须同时收到 worker 的清理成功结果并观测其正常退出，才持久化 `cleaned=true`。结果缺失/非法、worker 丢失、意图传输中断或持久化不确定均保留占用。服务自身重启后不接管旧 PID，即使 worker 可能仍在运行。未知执行通过已验证的平台容器替换恢复，不提供强制释放端点。宿主机 boot ID 或 Agent 重启都不能单独证明容器已替换。状态目录外的 checkpoint 保留。Agent 失联不会指示 worker 停止训练，但服务自身丢失暂不支持透明恢复。

后续云探针找到 Python 3.11.16 环境：`/root/fleet/venv/bin/python` 包含 Ray 2.59.0、PyTorch 2.10.0、RLinf 0.4.0；`/opt/rl_dashboard_bundle/env/bin/python` 包含 Ray 2.56.1、PyTorch 2.7.0。user/PID namespace 中 CPU Ray 任务虽能完成，但继承的 `/proc` 会引发 psutil PID 错误，`include_dashboard=False` 也会启动部分后台服务。在临时挂载点挂载私有 proc 和 tmpfs 同样失败。选用进程树后端避免该 PID 不匹配问题。已验证 GPU 可见，GPU 授权和真实 GPU/多容器训练尚未验证。授权环境前必须解析 Python 可执行文件符号链接。

root 进程测试覆盖脱离会话后代清理、无关进程保护、重复启动/停止、完成结果恢复和结果缺失后的阻塞恢复。这些测试不代表生产 Agent 注册、限定授权、Job 副本分发、Ray 屏障、授权日志或 UI 已接通；完成这些接线和端到端验收前，交付清单仍未完成。

实际云容器验收：已构建的新 supervisor 以 root 运行进程树后端，通过私有 socket 启动 Ray 2.59.0 CPU 任务（1 CPU、0 GPU），返回 `{value:45, uid:0}`，最终 `Succeeded` 且 `cleaned=true`。重复 Ensure 复用原执行，每次 Observe 使用新连接；确认清理后 Stop 返回删除标记。未安装依赖、未运行 GPU 计算。该测试没有覆盖完整 RLark Job/Agent 接入。

当前只允许规范可执行路径。Python venv 的符号链接解析后可能失去 venv 的 `sys.prefix` 和包搜索路径；本次探针显式设置已准备环境的 `PYTHONPATH` 和 `PATH`，不能据此宣称直接传 venv 链接即可运行。正式环境适配还需验证 Python 前缀和依赖解析。

新增故障测试覆盖运行中 socket 重连、忽略 SIGTERM 后等待 30 秒再强制停止，以及结果持久化失败。结果落盘失败返回 Unknown、`cleaned=false`，恢复磁盘后重复 Stop 也不释放节点；重启只读取原持久化意图并继续阻塞。
