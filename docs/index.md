# 团队设计与运维文档

这里收录团队环境和扩展的设计、部署记录与操作资料，与 RLark 原生文档分开维护。文中的现场状态和验证结果均为记录时的快照，操作前应核实当前环境。

| 主题 | 文档 | 状态 |
| --- | --- | --- |
| Station | [网络设计](design/station-network-plan.zh-CN.html) | 设计与现场验证记录，部分方案待验收 |
| Station | [API 契约](design/station-api-contract.zh-CN.md) · [English](design/station-api-contract.md) | 试验性，尚未安装 |
| Station | [节点身份协议](design/station-identity-protocol.zh-CN.md) · [English](design/station-identity-protocol.md) | 试验性原型，未部署 |
| 存储与镜像 | [共享存储与镜像仓库](design/shared-storage-registry-plan.zh-CN.md) · [English](design/shared-storage-registry-plan.md) | 设计、历史测试与运维记录 |
| 存储与镜像 | [私有镜像仓库操作](operations/private-registry.zh-CN.md) · [English](operations/private-registry.md) | 试运行，2026-11-05 快照 |
| Fleet | [NAS 固定路径](design/fleet-storage-paths.zh-CN.md) · [English](design/fleet-storage-paths.md) | 路径契约；现场迁移和跨站验收单独记录 |
| Fleet | [Dashboard PostgreSQL 迁移](design/fleet-postgres-plan.zh-CN.md) | 进行中，含实验记录 |
| 部署 | [Station 网络部署快照](design/station-deploy/README.zh-CN.md) · [English](design/station-deploy/README.md) | 试验性现场记录 |
| 资产 | [金山云裸金属](design/bare-metal-assets.zh-CN.md) · [English](design/bare-metal-assets.md) | SSH 入口、节点角色及存储插件核查 |

Fleet 工程中的 [RLark 与 Fleet 集成设计](https://github.com/astribot-post-train/fleet/blob/main/docs/rlark-fleet-integration.md)和 [RLark Job 操作说明](https://github.com/astribot-post-train/fleet/blob/main/docs/operations/rlark-job.md)由 Fleet 工程维护；访问需要该仓库权限。

## 本地阅读与维护

从仓库根目录安装 `apps/rlark/docs/requirements.txt` 中的依赖后运行：

```sh
mkdocs serve -f mkdocs-team.yml
```

构建使用 `mkdocs build --strict -f mkdocs-team.yml`。HTML 原样发布并保留自己的样式和导航；站内搜索覆盖 Markdown 页面，HTML 的全文搜索需单独实现。部署时应使用受控的访问方式，文档包含环境专属信息。

维护文档时保留现有中英文对应关系，说明验证时间、适用范围和已知限制；状态未经复核时不要改成已部署或已验收。
