# 核心能力

RLark 为异构具身智能基础设施提供统一控制面。

- **多集群资源纳管**：统一接入多种运行时，集中查看可用算力节点和具身设备。
    - **Kubernetes Runtime — Preview**：已实现并可用于评估，但尚不承诺生产稳定性。
    - **Docker Runtime — Preview**：通过宿主机 Docker socket 执行 Ray/RLinf v1 容器 Task，支持整卡 GPU 预留和 host network。
    - **Raw Runtime — Experimental**：预置 Node 可通过限定 Agent/supervisor 路径运行一个可信进程 Task；Ray/RLinf 组、副本、日志和接入安装包尚不支持。
- **任务编排**：用 Task 描述跨可用算力资源协同运行的分布式任务角色。
- **跨集群网络**：通过 TUN、gVisor netstack 与 SSH 隧道连接工作负载，不要求数据面具备公网入站能力。
- **交互式开发**：从控制台查看 Worker、复制 SSH 命令并打开 WebTerminal。
- **Embodied Runtime**：通过设备插件和运行时控制器将机器人、相机等设备转化为可调度资源。
- **平台治理**：在管理平台统一处理调度、资源标注、权限、存储与运行状态。

建议先阅读[核心概念](../concepts.md)，再按照[快速开始](../quickstart.md)完成一次端到端部署。
