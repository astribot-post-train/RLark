# Astribot Gateway 目录结构

共享 Gateway 负责注册路由，并提供鉴权、响应格式和客户端。Astribot 开集群及 SSH 入网流程位于 `apps/rlark/astribot/provisioning/workflow`，场站观测及机器人控制位于 `stations`，场站网络及入网契约位于 `stationidentity`。存储声明、校验及渲染位于 `storageconfig`，镜像元数据位于 `registry`。

本次提取沿用已有 API 路由，无需迁移数据。Gateway 和 workflow 回归测试覆盖原有入口与响应契约。
