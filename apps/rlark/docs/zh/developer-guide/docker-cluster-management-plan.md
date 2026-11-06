# Docker 集群管理交付计划

本文跟踪将固定的私有 Docker GPU 主机作为独立 RLark 集群接入的运维路径。
实现代码继续放在 `extensions/container-runtime`，复用原生 Cluster、Node、
Job、Task 身份，但通过配套资源的 `runtime` 判别值保持 Kubernetes 集群和
Docker 集群隔离。

## 当前契约

- Docker 集群使用 `runtime: Docker` 和独立 Agent namespace 创建。
- 每台登记主机对应一个配套 ContainerNode 和一个
  `AgentTypeDocker` 原生 Node。
- Agent 以 root 在主机上运行，直接访问主机 Docker Unix socket；不支持
  Docker-in-Docker，也不会把 socket 挂进工作负载容器。
- 调度观测包含私网地址、网络 scope、授权整卡 GPU、Engine 身份、心跳、
  capability、忙闲状态和 allocation。只有身份、所有权和观测均当前有效时
  节点才可调度。
- Agent 版本发布是 Cluster 的持久目标状态。控制器每次只选择一台新鲜且空闲
  的节点，创建精确 upgrade allocation，并等待 Agent journal 和清理证明后再
  选择下一台。

## 交付增量

1. **创建正确的集群。** 控制台创建表单选择 Raw 或 Docker，并将选择保留在
   可重试的 operation 请求中；API 已校验两个值，UI 不能再静默创建 Raw。
2. **接入 Docker 主机。** Docker 集群详情页开放现有管理员节点登记流程。服务端
   从已绑定集群推导 runtime，创建匹配的 Docker 原生 Node/ContainerNode；不接受
   客户端自报 runtime 或 GPU 授权。
3. **下发 Agent 更新。** Docker 安装包可以选择复用 Raw 使用的 root updater。
   升级准入必须同时核对 Docker 原生 Agent 类型、runtime、Engine 身份和整机空闲
   allocation。Docker 节点只通过精确安装路径和持久 updater journal 升级。
4. **让页面可用。** 详情页展示 runtime、节点清单、Engine/capability 观测、GPU
   数量、占用、allocation、目标版本和升级阶段。Docker 集群隐藏不支持的工作负载
   操作；Docker Ray Job 提交仍是单独的预览门槛。

## 安全与恢复要求

- 从当前对象收敛；重复或丢响应的请求只有在 operation UUID、对象 UID、请求指纹
  和 owner reference 全部匹配时才恢复。
- 过期心跳、实例替换、credential 变化、版本变化或 held allocation 会阻止调度和升级。
  Cordon 只阻止新的工作负载启动，不会暂停独立的空闲 Agent 滚动升级。
- 安装包创建或 upgrade claim 后丢响应时保留 operation 和精确锁；不能创建第二
  个 Agent，也不能释放状态不确定的 allocation。
- 默认串行、只选择空闲节点升级。它不会停止运行中的 Docker Job、撤销已经授权的
  start，也不提供内核级端口预留。
- UI 过滤只是展示辅助；Gateway/API 和直接管理路径仍执行租户、所有权、runtime
  和授权检查。

## 验收门槛

- 单元及故障恢复测试覆盖 Docker 集群创建、Docker 节点登记、安装包校验、升级
  准入和控制器重启。
- 中英文文档、CRD 输出和扩展 UI 构建通过。
- 真实 Docker Engine 主机必须完成 Agent 观测、单容器生命周期、精确清理和一次
  空闲 Agent 更新，才能称为 Docker 云端升级验收通过。当前金山云开发机没有
  Docker socket，只能用于 Raw/Agent 验证。

本计划保持增量实现，不向原生 RLark API 增加 Docker 专属字段，也不宣称已经达到
生产级 Kubernetes 能力等价。
