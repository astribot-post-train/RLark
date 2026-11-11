# Station API 类型（试验性）

此独立 Go module 定义 `station.rlark.io/v1alpha1` API 类型、生成 CRD、controller 和 controller manager。Station API、controller、真实身份 agent 及双容器网络 DaemonSet 已在本地、机器人和两台裸金属节点运行；WireGuard 由 `extensions/station-gateway` 的网络容器实现。Agent 会随心跳从 `/proc/stat` 上报内核启动时间。公网镜像仓库、整机恢复和自动凭据轮换仍未完成。

字段归属、namespace 映射、校验、生命周期语义和待决设计见[API 契约](../../docs/design/station-api-contract.zh-CN.md)。

身份协调器拒绝已取消的获取请求，并在取消与获取竞争时释放锁。该锁仅串行化活动 manager 进程；可协作 leader-only 探针的正常交接已在隔离 kcp 验证，协调过程中的切换和分布式 fencing 尚未验证。

测试和 vet 需关闭仓库 Go workspace：

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

`TestLiveStationReconcileAndRebind` 还需将 `STATION_TEST_ISOLATED_KUBECONFIG` 指向已安装五份 CRD 的一次性 workspace。测试会创建 namespace 和资源，禁止指向生产 workspace；Cluster 注册解析和成员 Node 读取使用测试替身，并在真实 CRD status 和审计资源上测试注入审计故障后的恢复。

用仓库固定版本的生成器更新 deepcopy 代码和 CRD：

```bash
GOWORK=off ../../api/bin/controller-gen-v0.18.0 object paths=./api/v1alpha1/...
GOWORK=off ../../api/bin/controller-gen-v0.18.0 crd:crdVersions=v1 paths=./api/v1alpha1/... output:crd:artifacts:config=./config/crd/bases
```
