# 容器运行时管理控制台

容器运行时扩展提供独立的管理员控制台，作为原生 RLark 控制台的补充。
代码和构建入口位于 `extensions/container-runtime/`，不会向原生
Cluster、Node、Job、Task API 增加容器专属字段。

## 本次范围

本预览版面向显式接入的 Raw 容器集群，提供：

- 集群列表和详情；
- 节点的权威身份、私网地址、运行时、能力、授权 GPU UUID、心跳新鲜度、
  Ready、可调度和忙闲状态；
- 可用 Agent 版本及当前目标版本；
- 可恢复的集群创建和目标版本选择。

Allocation、Job 和 Agent 升级作为独立维度展示。控制台不会在缺少观测时
伪造利用率，也不会把过期心跳当作 Ready 或可调度。Ray/RLinf 组提交、远程
日志、terminal/exec、自动注册、Docker Engine 创建和多节点调度仍不属于本
预览版。

## 资源与所有权

扩展拥有 `containers.rlark.io/v1alpha1` 附属资源组。`ContainerCluster`
通过名称、namespace 和 UID 指向一个原生 Cluster；`ContainerNode` 指向精确
的原生 Node 和 Cluster 实例。服务端会验证这些引用、权威的
`Cluster.spec.agentNamespace` 以及独立租户，然后才返回或接受写入。

名称和标签只能用于检索，不能证明所有权。原生对象被替换后，必须由管理员
显式创建新的附属资源，服务端会将旧引用报告为冲突。

集群创建只有在使用相同规范化操作 UUID 和请求指纹时才幂等。重试可以恢复已
提交的 namespace 或附属对象，但不能接管所有权元数据不同的对象。选择目标
版本要求当前 Cluster resourceVersion、精确的 release UID、release 当前
`Available=True` 观测和相同租户。目标版本是持续的期望状态；清空或替换它
会影响后续滚动升级，不会取消已经处于 Applying 的 Agent 升级。

## 认证与部署

浏览器通过同源 Gateway 复用 RLark 的正常登录会话。扩展 API 只接受受信任
Gateway 签发的短期 RLark HS256 JWT，并强制校验 issuer、subject、管理员角色、
算法和过期时间，同时拒绝客户端伪造的身份请求头。扩展 API 不签发浏览器
token，不接受 kubeconfig，不暴露签名密钥，也不代理任意 Kubernetes 路径。API
进程使用权限受限的独立管理 ServiceAccount；Agent 使用独立的 mTLS 接口。Agent
证书不会进入浏览器。

独立 UI 和 API 从同源的 `/api/v1/container-runtime/` 提供服务。开发环境使用
TLS 或仅监听回环地址的反向代理。认证响应设置 `Cache-Control: no-store`，日志
不得记录 Authorization、kubeconfig、证书材料或任务负载。没有管理员 JWT 返回
401；已认证但不是管理员返回 403。

浏览器只接收类型化摘要，不接收 Secret、证书路径、私钥或管理 kubeconfig。
节点加入和安装包导出仍是管理员 CLI 操作；UI 只展示节点状态，不成为注册通道。

## UI 状态与失败处理

集群页面将期望状态和观测分开：

| 维度 | 示例 |
| --- | --- |
| 期望状态 | runtime、目标 Agent 版本、暂停/升级意图 |
| 身份 | 原生/附属 UID、租户、namespace、私网地址 |
| 观测 | 心跳新鲜度、Ready、可调度、忙闲、能力 |
| 容量 | CPU/内存/GPU 总量和授权 GPU UUID |
| 操作 | allocation 数量、升级阶段、最近已验证回执 |

每次刷新都可取消，并丢弃过期响应。API 出错时保留可识别的 HTTP 错误类别，
显示“数据不可用”，不能显示成零计数。过期节点仍然可见并被调度拦截，不会被
静默删除。写入使用乐观 resourceVersion；发生冲突时提示管理员刷新。

## 支持的部署顺序

1. 安装扩展 CRD 和受限 API/controller RBAC。
2. 使用 `container-runtime-admin` 创建独立原生 Cluster 和容器附属资源。
3. 注册现有 Raw 容器主机并导出受限 Agent 包。cgroup-v2 或 systemd 不可用时，
   主机使用受信任的 root 进程树后端；它不是任务隔离边界。
4. 启动扩展 controller、Agent API 和独立控制台。仅在隔离预览集群启用 Job
   reservation 与 task execution。
5. 打开控制台，确认节点心跳新鲜且 GPU 在授权白名单中，再把可用 Agent 版本
   发布为目标版本。

当前金山云验收机使用 `regular-training` 队列，20 CPU、236 GiB、1 张 GPU、无
公网 IP；替代镜像为 `rlinf-dashboard:1.1`。该镜像只是起点，不能证明源机器
后来安装的所有依赖都存在。单进程 Raw GPU 生命周期和单节点空闲滚动升级已经
通过验收；多节点 Ray/RLinf 和 Docker Engine 生命周期需单独完成一致性测试后
才能对外宣称支持。


## 构建和启动独立控制台

控制台只接受 Bearer 认证。登录表单把类型化凭据转交配置的 Gateway，返回的
管理员 token 只留在浏览器内存中，刷新或退出后消失，不转发 Gateway Cookie。
本增量采用独立入口/会话，尚未向原生 UI 增加导航。管理员可跨租户查看平台
资源；普通用户不能访问。

从扩展目录部署 `config/rbac/container-runtime-console.yaml`。示例 ServiceAccount
位于 `rlark-system`，更换 namespace 时需同步修改三处引用。该身份可读取资源、
创建原生 Cluster 和专用 namespace、创建/patch 附属 Cluster；不能读取 Secret、
派发 Task、修改观测 Status、删除资源或释放 allocation。签名密钥不放在该
kubeconfig 中。

HS256 密钥必须是配置 Gateway 使用的原始字节（至少 32 字节），以只读文件
挂载到受信任控制面服务。HS256 是对称算法，持有密钥也能签发 token，因此
控制台与 Gateway 具有相同的 token 信任级别。运维负责分发密钥并同步轮换/
重启两个服务；控制台不会通过 Kubernetes Secret 权限获取密钥。

```bash
cd extensions/container-runtime
make build
make build-ui
bin/container-runtime-console \
  --addr=127.0.0.1:9445 \
  --kubeconfig=/private/console.kubeconfig \
  --gateway-jwt-key-file=/private/gateway-jwt.key \
  --gateway-origin=http://127.0.0.1:19000 \
  --ui-directory=ui/dist
```

打开 `http://127.0.0.1:9445/` 并用管理员登录。HTTP 只允许显式回环 IP；其他
地址必须提供 `--tls-cert` 和 `--tls-key`。远程 Gateway 也必须使用系统 CA
信任的 HTTPS，没有跳过 TLS 校验选项。节点 mTLS 仍连接独立
`container-runtime-api` 服务。UI 构建要求 Node.js 20.19+ 或 22.12+，验收使用
Node.js 24。

预览 UI 创建 Raw 集群。API/CLI 也能创建 Docker 资源记录，但不代表 Docker
Engine 注册/执行已支持。节点加入、安装包导出、产物发布、cordon/drain 仍通过
CLI 或运维流程完成。页面 Job 名来自 allocation 引用，不是任务提交或历史接口。

页面每 15 秒刷新观测。心跳超过 45 秒、未来心跳、过期 generation、凭据不匹配、
原生节点被替换、cordon/drain、旧版预留、已有锁和目标版本不一致都会阻止页面
显示可调度。快照只是提示，allocation API 在派发时仍重新检查权威状态。页面
显示登记授权与上次观测容量，不是实时利用率，也不保证 GPU 空闲。目标变化
可能按现有调度策略拦截旧节点，不保证服务可用性。

创建请求与规范操作 UUID 保存在 sessionStorage（不含凭据）。结果不确定时
只能重试完全相同请求；不能通过清除记录、换 UUID 修复部分创建，需先检查现有
原生对象/namespace/附属对象的所有权。清空与设置目标都要求相同 Cluster UID
和 resourceVersion。版本选择检查精确 UID/当前可用状态，但读后引用仍可能变化，
rollout 和 Agent 控制器在执行副作用前仍须复核。错误只返回有界 `code`/`message`
和 `X-Request-ID`，不泄露上游凭据或配置详情。

## 2026-10-31 验证记录

Go 测试、vet、扩展构建和增量 lint 通过。UI 传输/过期读取测试、TypeScript
检查、生产构建与依赖审计通过。浏览器对真实 Gateway/KCP 使用受限 console
ServiceAccount 验证了管理员登录、真实 `ksyun-containers` 资源、普通用户拒绝、
专用临时集群创建（故意丢弃首次已提交响应）、相同 UUID 重试、发布目标版本、
中英文切换和退出。浏览器没有 JavaScript 异常。临时资源通过正常生命周期清理，
现有云端 Cluster 的目标版本未改变；不代表多节点训练或生产部署已验收。
