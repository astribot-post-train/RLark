# 创建任务表单修复：Raw 集群支持

## 概述

修复了 RLark 前端创建任务表单在 Raw 集群（如 `ksyun-containers`）下的三个问题：
1. 集群资源选不上（节点不显示）
2. 镜像下拉框只显示历史镜像
3. 存储下拉框为空

## 实现方案

### 1. 新增文件

#### `apps/rlark-ui/src/utils/clusterRuntime.ts`
- 集群运行时检测工具
- 提供 `nodesForCluster()` 函数：根据集群类型匹配节点
  - Kubernetes 集群：`namespace === clusterId`
  - Raw 集群：`namespace === agentNamespace`
- 提供 `isRawCluster()` 函数：判断集群是否为 Raw 类型

#### `apps/rlark-ui/src/utils/rawTask.ts`
- Raw Task 生成器
- 实现 `rawTaskFromRole()` 函数，将表单配置转换为符合 `raw.rlark.io/process` 契约的 Task
- 包含完整的验证逻辑（环境名、argv、工作目录等）

#### `apps/rlark-ui/src/components/RegistryImagePicker.tsx`
- 镜像选择器组件
- 支持从私有仓库 `192.168.31.2:5443` 列出镜像
- 同时显示最近使用的镜像历史
- 使用 `imageRegistriesApi.repositories()` 和 `imageRegistriesApi.tags()` 获取镜像列表

#### `apps/rlark-ui/src/components/RawRoleFields.tsx`
- Raw 角色配置字段组件
- 替代 Kubernetes 集群的镜像和存储配置
- 包含字段：
  - 容器环境（environment）
  - 进程参数（argv）
  - 环境变量（env）
  - 工作目录（directory）
- 提供 `extractEnvironments()` 函数从节点标签提取可用环境

### 2. 修改文件

#### `apps/rlark-ui/src/components/ResourcePlacementPicker.tsx`
- 添加 `clusterNodes` 可选 prop，允许调用方传入预过滤的节点列表
- 修改 `isSchedulable()` 判断，接受 `agentType === "Raw"` 的节点

#### `apps/rlark-ui/src/utils/job.ts`
- `generateJobCRD()` 函数添加可选参数 `rawRoles`
- 在生成 tasks 时检查是否有 Raw 配置，优先生成 Raw Task
- Raw Task 使用 `raw.rlark.io/process` workload 类型

#### `apps/rlark-ui/src/pages/CreateJob.tsx`
- 添加状态：
  - `runtimeMap`: 存储集群运行时信息（Kubernetes 或 Raw）
  - `rawRoleConfigs`: 存储每个角色的 Raw 配置
- 添加 useEffect：根据节点的 `agentType` 自动构建 `runtimeMap`
- 容器运行配置区域根据集群类型条件渲染：
  - Raw 集群：显示 `RawRoleFields` 组件
  - Kubernetes 集群：显示原有的镜像和存储配置
- 镜像选择器替换为 `RegistryImagePicker` 组件
- 提交时将 `rawRoleConfigs` 转换后传递给 `generateJobCRD()`
- 移除存储类加载失败时的 mock 数据回退

#### `apps/rlark-ui/tsconfig.test.json`
- 添加新工具文件到测试编译范围：
  - `src/utils/clusterRuntime.ts`
  - `src/utils/rawTask.ts`

### 3. 测试文件

#### `apps/rlark-ui/tests/cluster-runtime.test.mjs`
- 测试 Kubernetes 和 Raw 集群的节点匹配逻辑
- 测试集群类型识别

#### `apps/rlark-ui/tests/raw-task.test.mjs`
- 测试 Raw Task 生成
- 测试各种验证场景（空环境、空 argv、非绝对路径等）

## 技术细节

### 集群识别机制

通过节点的 `spec.agentType` 字段识别集群类型：
- `agentType === "Raw"`: Raw 集群
- `agentType === "Kubernetes"` 或未设置：Kubernetes 集群

对于 Raw 集群，从节点的 `metadata.namespace` 推断 `agentNamespace`。

### Raw Task 契约

生成的 Raw Task 符合 `extensions/container-runtime/pkg/taskcontract/raw.go` 定义的契约：
- `runtime.kind`: "Raw"
- `workload.kind`: "raw.rlark.io/process"
- `placement.environment`: 环境名称
- `workload.parameters`: 包含 `argv`、`env`、`directory`

### 镜像仓库集成

使用现有的 `imageRegistriesApi`：
- `list()`: 获取已配置的镜像仓库
- `repositories()`: 列出仓库中的 repositories
- `tags()`: 列出指定 repository 的 tags

限制：
- 只支持私有仓库 `192.168.31.2:5443`
- 最多显示前 10 个 repositories，每个最多 5 个 tags

## 验证

### 单元测试
- 所有新增测试通过（8 个测试）
- 所有现有测试保持通过（268 个测试）

### TypeScript 编译
- `npx tsc -p tsconfig.app.json --noEmit` 通过
- `npm run test:compile` 通过

## 未来工作

1. **镜像仓库扩展**：如需支持多个私有仓库，可以在 `RegistryImagePicker` 组件中添加仓库选择器
2. **环境自动发现优化**：当前从节点标签提取环境列表，可以考虑从 Container Runtime API 获取更完整的环境信息
3. **Raw Task 预览**：可以添加 YAML 预览功能，显示生成的 Raw Task 结构
4. **表单持久化**：Raw 配置字段可以支持从已有 Job 克隆时的反序列化

## 注意事项

- 不改动 RLark 原生代码（backend、controller），只修改前端 UI
- 保持向后兼容：Kubernetes 集群的工作流程完全不受影响
- 错误处理：Raw Task 生成失败时，错误信息会原样展示在前端，由用户修正
