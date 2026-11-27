# 迁移历史 SSH 主机和开集群记录

## 复用历史工作节点

在原主机重新发起 SSH 入网。Gateway 会核对固定的 SSH 指纹和 WireGuard 公钥。如果历史工作节点的标记没有保存集群和节点名，Gateway 会读取原安装日志目录中的节点名，并将主机保存的 API 地址和 CA 指纹与目标成员集群的原生信息核对。成员 API 必须确认该名称的原生 Node 已不存在。主机持锁重新核对证据后，才会重置工作节点安装或变更隧道归属。

如果安装仍在运行、主机是控制面、已有同名节点、API 地址或 CA 不一致，或者缺少证据，自动恢复会停止。请保留旧记录，核实归属后再重试。

## 记录历史开集群归属

清理历史开集群记录前，通过管理员接口 `POST /api/v1/cluster-provisions/{name}/migrate-ownership` 补记归属。先查安装记录、审计记录或备份，独立确认原始归属，再填写已核实资源的当前 `metadata.uid` 和 `metadata.resourceVersion`。同名、当前 DNS 或当前资源列表本身无法证明历史归属。

原管理面 Cluster 已不存在时，停止的开集群记录可以这样迁移：

```json
{
  "provision_uid": "original-provision-uid",
  "evidence": "变更单 CHG-1234",
  "site_network": {"uid": "reviewed-site-network-uid", "resource_version": "764"},
  "credentials": {
    "station-read": {"uid": "reviewed-secret-uid", "resource_version": "738"}
  }
}
```

原 Cluster 仍在时，通过 `cluster` 提供它的资源身份。接口要求核对当前 Cluster 实例，否则会拒绝迁移。凭据键支持 `station-read`、`station-operations`、`release` 和 `agent-certificate`。只提交你已核实的资源。SiteNetwork 声明已部分清理时，剩余条目与原开集群记录一致即可迁移。若要迁移历史控制面主机，还需要从原集群记录取得 `kube-system` 命名空间 UID，填入 `native_cluster_uid`。后续清理登录主机时，会核对已保存的 SSH 指纹和主机上的原生 UID，再补写清理所需的标记。

迁移写入归属元数据，并在审计日志记录管理员和证据引用。主机安装、Secret 数据以及独立的 NodeAdmission 和 NetworkEnrollment 都保留。核实后使用原清理入口。已成功创建且仍存在的 Cluster 要先走正常的集群删除流程。写入冲突时重新核实剩余资源，用最新资源版本重试。同一 UID 已写入的归属记录可继续复用。原始归属无法确认时，请保留相应历史记录。
