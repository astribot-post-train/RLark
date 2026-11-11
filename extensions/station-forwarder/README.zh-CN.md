# Station SMB 转发器（试验性）

组件维护一组固定的 IPv4 TCP 转发与 SNAT 规则，只允许一个源主机访问一个目标主机的一个端口。需要 hostNetwork、`CAP_NET_ADMIN`、与宿主机实际后端兼容的 `iptables`、已开启的 IPv4 转发，以及输出接口已配置的 SNAT 地址；应用规则前会检查这些前置条件。组件插入严格匹配的 `FORWARD` 规则并追加匹配的 `POSTROUTING` SNAT 规则，不更改全局转发开关或防火墙策略；其他链或钩子仍可能拒绝流量。必须保留独立管理通道。

配置是包含 `input`、`output`、`source`、`destination`、`snat`（地址均为单个 IPv4 单播主机地址）和 TCP `port` 的 JSON 对象。组件在执行外部操作前将配置 SHA-256 指纹写入 `/var/lib/rlark-station-forwarder/config.sha256`；每个实例须使用独立持久 hostPath。重启只接受完全相同的配置。Pod 删除也不会删除防火墙规则；规则会保留至主机重启或人工移除。更改或卸载时，先经独立管理通道核对并删除宿主机上完全对应的三条规则、验证不存在后，再删除该实例的指纹状态；不得删除其他规则，也不能仅凭 comment 判定归属。部分失败可能留下部分规则，相同配置重试可继续协调。当前只检查规则是否存在，不保证其他防火墙链不会覆盖其效果。组件不认证 SMB 用户，凭证和访问授权由 NAS 执行。

镜像包含 iptables，须与宿主机后端兼容。当前部署以 `imagePullPolicy: Never` 使用本机缓存，镜像丢失后无法自动恢复。依赖该功能前必须验证正反路径、节点重启、失败恢复、存储认证及运维恢复。单元测试和隔离内核规则测试：`GOWORK=off go test ./...`、`GOWORK=off go vet ./...`、`unshare -Urn env GOWORK=off STATION_NETNS_TEST=1 go test -run TestFirewallIdempotence ./...`。

三个网络命名空间中的转发和拒绝路径测试：`unshare -Urnm sh tests/network-namespace.sh`。
