# 给使用方（Fleet）的两样 RLark 能力（2026-11-12）

分支 `feat/fleet-service-access`，基于 `feat/optimization` 1deb4ef，和 Station 的分支互不依赖。两样都在 Gateway 里，都有单测；没有在真实的 RLark 控制面上跑过。

存储类的挂载选项不在这个分支里：存储由 liangcai 自己改（用户 11-12）。写好的一版单独放在分支 `feat/storage-mount-options`（一个提交 9ee3c60），第 3 节是它的规格，供他直接拿走或者照着自己写；他不要就删掉那个分支。

## 0. 结论

| 需求 | 做法 | 接口 |
|---|---|---|
| Fleet 的 Dashboard 要替操作员提交、停止 Job，现在只能借浏览器里的登录态 | **服务账号**：有角色和租户，一个不过期的令牌，轮换或删除即失效 | `/api/v1/service-accounts` |
| Job 要用密钥（场站 token、视频凭证），现在是手工在成员集群里建 Secret | **Job 密钥**：在 RLark 里建，按名字下发到所选集群里 Job 所在的命名空间 | `/api/v1/secrets` |

两样都不新增下发机制：密钥走存储类凭证已经在用的那条路（管理面上的 Replication 声明 → 各集群的 Delivery → 集群 agent 应用），服务账号的令牌用 Job 密钥下发。

## 1. 服务账号

- 建：`POST /api/v1/service-accounts {"name": "fleet-dashboard", "role": "user", "tenant_ids": ["default"], "deliver": {"clusters": ["rlark-ksyun-gpu"], "secret_name": "fleet-rlark-token"}}`（管理员）。返回里带令牌，只这一次。`role` 默认 `user`，`tenant_ids` 默认 `default`。
- 用：`Authorization: Bearer rlsa.<名字>.<随机串>`，和登录得到的 JWT 走同一个入口。`user` 角色能做登录用户能做的事（提交、停止、查询 Job，查节点和场站节点），不能做管理员的事。它创建的对象的创建者是 `service-account:<名字>`。
- 下发：带了 `deliver` 就同时维护一个 Job 密钥（键 `token`、`service-account`），Dashboard 的 pod 把它挂成文件来读，没有人手工拷令牌。
- 轮换：`POST /api/v1/service-accounts/<名字>/token`，旧令牌立刻失效，下发的密钥跟着换。删除：`DELETE`，令牌失效，下发的密钥一并删除。
- 存储：管理面命名空间里的 Secret `rlark-service-account-<名字>`，只存令牌的 SHA-256。每次请求都现读这个 Secret，所以轮换和删除没有延迟。
- 没有做过期时间：给程序用的凭证靠轮换和删除管，过期只会让服务在某个夜里停掉。

## 2. Job 密钥

- 建：`POST /api/v1/secrets {"name": "fleet-site-token", "clusters": [...], "data": {"site_token": "…"}}`（管理员）。各集群的 agent 在 `rlark-system`（Job 的 pod 所在的命名空间）里建出同名的 Secret，Job 按名字引用。
- 改：`PUT /api/v1/secrets/<名字>` 整体替换集群和全部值。删：`DELETE`，管理面和各集群上的都删。列：`GET`，只有名字、说明、集群、键名，没有值。
- 名字不能以 `rlark-`、`jo-`、`sc-`、`ir-`、`agent-` 开头（RLark 自己在那个命名空间里用的）。下发策略是"冲突即失败、不接管已有对象"：成员集群里已经有一个手工建的同名 Secret 时下发会失败。所以把今天手工建的 `fleet-site-token`、`fleet-video` 换成它时，先删手工建的那个，再建。
- 值存在管理面的声明里（和存储类凭证一样），单个密钥总大小上限 512 KiB。

## 3. 存储类的挂载选项（规格，不在本分支；实现在 `feat/storage-mount-options`）

现在的位置：`apps/rlark/pkg/gateway/storage_handler.go` 的 `s3MountOptions` / `smbMountOptions` 两个包级变量（1deb4ef 的第 265–266 行），在 `buildStorageClassReplicationSecret` 里原样写进 StorageClass 的 `mountOptions`。

存储类的创建和更新请求多两个可选字段（旧的存储类不带，行为不变）：

| `mount_profile` | 用途 | 选项 |
|---|---|---|
| 不写 / `default` | 原来那一套 | `vfs-cache-mode=writes`、`dir-cache-time=5m`、`vfs-write-back=1s` 等 |
| `read` | 只读的训练数据 | `vfs-cache-mode=off`、`read-only`、`dir-cache-time=1h`、`attr-timeout=1h`、`ignore-checksum`、`tpslimit=40`、`low-level-retries=20`、`vfs-read-chunk-size=32M` |
| `archive` | 归档写入：写返回时文件必须已经在对象存储上 | `vfs-cache-mode=off`、`dir-cache-time=5s`、`attr-timeout=5s`、`ignore-checksum`、`low-level-retries=20` |

- 档位只对 S3 类型有定义。`read` 和 `archive` 里的数值（`tpslimit=40`、`dir-cache-time=1h` / `5s`）是按主会话转述的要求定的起点，没有在 KS3 上量过；要别的数值就不写档位，直接给 `mount_options`（rclone 的参数名，不带 `--`，最多 32 个，只接受形如 `name` 或 `name=value` 的普通参数）。两者不能同时给。
- 同一个桶要两种挂法，就建两个存储类。
- 列表接口返回 `mountProfile` 和实际生效的 `mountOptions`。
- 改一个已有存储类的挂载选项，对已经挂载的卷不生效，要等使用它的 pod 重建。

## 4. 没做的

- 界面上没有这两样的页面，只有接口。
- 服务账号没有比角色更细的权限（比如"只能提交和停止 Job"）；现在的粒度就是 `user` / `admin` 加租户。
- Job 密钥还不能按租户隔离，创建和查看都要管理员。
- 给使用方写节点注解的前缀（`consumer.rlark.io/…`）没有做，等 Dashboard 那边说清用途。
- rclone CSI 的节点选择和每集群镜像、pod 默认 nofile、KPFS 存储类：留给 liangcai。

## 5. 动了哪些上游文件

| 文件 | 行数 | 内容 |
|---|---|---|
| `apps/rlark/pkg/gateway/router.go` | +14 | 两组路由 |
| `apps/rlark/pkg/gateway/auth_middleware.go` | +9 | 认出服务账号令牌 |
| `apps/rlark/docs/api/swagger.yaml` | +94 | 8 条新路由的说明 |

新增文件：`job_secret_handler.go`、`service_account_handler.go` 和它们的测试。
