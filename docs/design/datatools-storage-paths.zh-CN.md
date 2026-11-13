# DataTools NAS 与云端运行约定


## 当前数据库约定（2026-11-12）

DataTools 使用金山云 RLark Job `jo-8b0e28b7d1104716`，入口仍为 <http://10.254.240.4:30800/>；PostgreSQL 独立部署在本地开发机 Docker 容器 `fleet-data-postgres`，WireGuard `10.254.240.1:25432`，库 `fleet_data`、schema `datatools`。PGDATA 是本地原生 ext4 Docker 卷，不在 NAS、KS3 或云节点。RLark、Station 和 Fleet 数据库均未修改，未来 Fleet 接入尚未实施。

源码和导出仍为下文固定NAS完整路径，输入仍为原KPFS路径；云端 `/var/lib/rlark-datatools` 保留JWT、视频文件缓存和旧SQLite回退快照。`DT_DATABASE_URL_FILE=/run/datatools-postgres/url` 强制TLS verify-full，显式PG配置失败不回退SQLite；`DT_STATE_DIR=/var/lib/rlark-datatools`。仍固定单节点单副本，不支持多活。停机切换上限15分钟，先以维护模式拒绝API写入验收，再停/改/启Job开放写入。PG收到新写入后不能直接切回旧SQLite。

NAS加密备份目录 `/mnt/post_train/RL/data-tools-data/postgres-backups`，本地密钥及云Secret独立保管；逐表摘要导入对账与NAS备份恢复演练已通过。小时/日定时备份依赖本机开机、WG及NAS可达；目标RPO一小时，整机丢失RTO未验收。详情见 DataTools 仓库 `docs/postgresql.zh-CN.md`。

## 以下为迁移前部署及NAS试验历史

以下旧Job、SQLite及“尚未适配”的描述仅反映迁移前状态，不是当前运行合同。NAS PGDATA试验失败结论继续适用；上述本地原生存储方案替代该候选。原源码、JWT和SQLite保留，回退须遵循停写门槛。

## NAS 部署约定（2026-11-12）

DataTools 通过 RLark Job `jo-1ca9f18ee83b4e9b` 在金山云运行，不走云厂商训练任务。WireGuard 入口为 <http://10.254.240.4:30800/>；容器监听 8000，RLark `exposedPorts` 映射 TCP 30800。无 GPU；固定节点 `10-0-2-196`、单副本，申请 8 CPU/16 GiB，上限 16 CPU/32 GiB。Kubernetes/Ray 的 Running 不代表应用就绪，必须检查启动日志和 HTTP。

| 用途 | 宿主机/共享盘及容器内完整路径 | 权限 |
| --- | --- | --- |
| 源码和构建后前端 | `/mnt/post_train/RL/user/liangcai/data_tools` | NAS，只读 |
| 新导出 | `/mnt/post_train/RL/data-tools-data/exports` | NAS，可写 |
| 输入数据集 | `/mnt/xc_kpfs/regular/ericzhang/datasets` | KPFS，只读 |
| 现有 SQLite、JWT、视频缓存 | `/var/lib/rlark-datatools` | 云节点 ext4，可写 |
| 发布记录 | `/mnt/post_train/RL/manifests/data_tools/<release-id>` | NAS，仅运维写入 |

直接使用完整路径，当前部署不使用 `/workspace`、`/exports`、`/state`、`/fleet` 别名。工作目录和 `PYTHONPATH` 都是源码根目录；镜像只提供依赖环境。`deploy/rlark/datatools-nas-job.json` 是脱敏的暂停模板，通过 Gateway 创建时分配实际 Job ID。禁止两个服务同时写这份状态或同时占用 30800。

发布 `datatools-20261112T055239Z` 含 192 个已校验文件。启动执行 `scripts/validate_runtime.py`，检查 NAS/KPFS/本地状态文件系统及源码摘要；现有数据库或 JWT 文件缺失时拒绝启动，避免空库。该检查只负责启动准入，不是数字签名或持续挂载监控。发布目录和清单由可信运维控制，不保证发现所有额外文件，也不提供运行中源码快照隔离：修改固定源码入口前先停 Job，完整发布源码和构建后前端、更新清单并验证，再启动；不要在运行期间覆盖源码。依赖变化还需重建环境镜像。

云端使用静态 Retain PV/PVC `datatools-nas-source`（ROX）、`datatools-nas-exports`（RWX），驱动 `rclone.csi.veloxpack.io`，实测文件系统 `fuse.rclone`。Secret 引用位于 kube-system，禁止把凭证写进 Job 或示例。专用挂载的目录缓存 5 秒、属性缓存 1 秒，源码 VFS 缓存关闭，导出采用 writes 且 writeback 延时为零；这是本次专用部署参数，不改变 RLark 自动生成存储的默认参数。目录前缀不等于 NAS 账号 ACL 隔离；PV 标称 10 GiB 不等于硬配额。源码只读仅限制容器，其他 NAS 客户端仍可改动。冷启动 Python 导入及逐文件校验可能耗时数分钟，应以 Uvicorn 启动完成和 HTTP 成功为准。NAS 断链重连及大规模持续导出吞吐仍需受控测试，小文件探针不能代替验收。

首期保留云端本地 ext4 上的 SQLite WAL/flock、JWT 和缓存及其暂停意图；PostgreSQL 和 Fleet 共享元数据迁移留待下一阶段，本次没有实施。`DT_DB_PATH`、`DT_JWT_SECRET_FILE` 使用完整状态路径，`DT_STARTUP_SCAN_MODE=off` 避免全盘启动扫描。沿用已导入的 KPFS 数据集根目录，不重新导入、删除或改写数据路径。旧 KPFS 源码和历史导出保留。新导出默认 `DT_EXPORT_DIR`，其下使用 `<task_name>/<config.name>`（无 task 时为 `<config.name>`），不是数字 export ID。请使用不同导出名称，非空目标会拒绝；用户显式指定输出路径仍受支持，并未被限制到该默认目录。

### 发布、验证与恢复

1. 构建前端、运行后端测试，发布带摘要清单的源码，排除凭证、虚拟环境和数据库；保留发布 ID、git 状态和代码差异。临时 RLark Job 不暴露端口，源码和状态只读；写入唯一导出探针，从另一个 NAS 客户端回读并核对摘要，随后只删除自己的探针。
2. 确认无导入、导出、删除或缓存转码正在执行，保留缓存意图。使用 SQLite 在线备份并检查 `PRAGMA integrity_check`，记录数据集/episode/用户数。本次本地备份为 `/var/lib/rlark-datatools/backups/datatools-20261112T055239Z/data_tools.db`；它不能抵御节点丢失，独立受保护备份及恢复测试完成前不能宣称具备灾备能力。
3. 通过 RLark Gateway 创建暂停的新 Job；暂停旧 Job 并等待实际 Pod 和端口 Service 消失，再启动新 Job。固定节点，始终只有一个数据库写服务，无需直接修改 Deployment。
4. 验收启动准入、后端源码路径、WG HTTP 200、匿名管理 API 401、认证后的数据集/episode/缓存接口、字节及后缀 Range 206、默认导出路径、UI 缓存进度，以及基线 329 个数据集、36,410 个 episode、1 个用户未丢失。完整停止/启动 Job 后重复检查。后端自动测试另覆盖导入导出及准入失败路径。
5. 准入或应用失败时，暂停新 Job，等其 Pod 消失后再启动旧 Job `jo-c13f4fc097ac40f8`，继续使用同一状态。不要拿快照覆盖运行中的数据库；恢复必须先停止全部写入并评估恢复点。数据库备份不含 JWT/缓存，灾备需另行保留；旧代码回退前仍需确认 schema 兼容。

WG 可达和应用登录仍是访问前提，NodePort 本身不提供身份认证。渐进转码播放仍未启用：不兼容且未缓存的视频需要完整转码，已完成的共享缓存可直接读取。


**已验证的导出限制：** 云端 rclone/SMB 挂载已通过 Parquet 写入回读及单文件链接失败后复制，但目录软链接返回 EIO。因此调用 `_safe_link_dir` 的图片目录导出模式在该目标上不可用。不能宣称所有导出模式已通过，也不能隐式复制整棵图片目录；需要另行明确复制模式并验证容量与吞吐。当前线上样本是 v2.0；v3 有自动测试样例，本库暂无可用线上 v3 样本。


## PostgreSQL NAS 准入试验——2026-11-12

隔离 RLark Job `jo-08a06c1e39e94e8d` 验证了 rclone 1.71.2 的 writes 缓存及零 writeback 延时。fsync 成功后，独立 SMB 客户端在第 5、10、15 秒均找不到仍打开的文件；关闭文件后才读到完整数据且 SHA-256 一致。该版本实现只同步本地 VFS 缓存，不同步远端仍打开的文件。因此未通过“NAS 为唯一权威 PGDATA”的持久性准入；不等于已实测 PostgreSQL 丢事务、NAS 断电行为，亦不泛指其他 NAS 协议。

正式 DataTools 保留 SQLite，没有引入正式 PostgreSQL 数据或应用适配器。后续迁移以存储方案通过准入为前提。试验记录保留于 `/mnt/post_train/RL/data-tools-data/postgres-tests/pg-nas-20261112T062553Z`。完整结果见 DataTools 工程 `docs/postgresql-nas-validation.zh-CN.md` 及英文对应页。

PostgreSQL 15.19 启用校验和的 initdb 在创建 template0 时另报 `base/1/2605.63aec2fb.partial` 文件缺失，随后清理触发 180 秒超时。未执行后续 SQL、压测及恢复阶段。临时验证任务停止并清理专用 Kubernetes 存储资源，非敏感证据保留 NAS。


## 连接复用与延迟

`DT_DATABASE_POOLING_ENABLED=true`（默认）启用应用生命周期持有的连接池。异步请求只使用所属事件循环的池（保留4个、额外4个）；同步后台使用独立线程安全池（保留2个、额外2个）。每个操作独立借还连接，不共享事务。池按需增长，借用等待最多2秒，耗尽返回通用503；借出健康检查淘汰失效连接，归还回滚未提交事务。失败写入不会自动重放。PG临时inventory表使用ON COMMIT DROP。应用先停止后台使用者再释放池。运行账号上限仍32：池合计最多12个，加独立锁会话及非池化工具/扫描；这是单worker预算，不是多worker容量承诺。

通过显式生命周期绑定管理依赖，不使用隐藏全局engine。请求及其继承的后台任务使用runtime；临时扫描事件循环、迁移、独立工具和leader/scanner会话锁保留NullPool。锁会话不得静默重连或返回通用池，失去所有权仍退出进程。关闭开关并停启RLark Job可回退连接行为，继续使用同一PG及现有数据；更换凭证/TLS也须重启Job更新已有池连接。

缓存进度复用认证请求连接，以单次合并查询读取控制状态和分类计数，磁盘统计在线程执行。统计总量查询合并但不缓存结果。进度面板在上次请求结束2秒后再发下次请求，关闭面板后停止；不缓存授权或业务查询结果。PG日期筛选与SQLite数字校验语义一致，排除错误日期；HTTP字段形状不变。

性能验收使用相同WG入口，预热3次后每接口采样30次：列表/统计中位数目标不超过700ms，缓存进度不超过800ms，且较基线下降至少40%。10并发不得泄漏连接或出现跨循环错误。NAS源码/静态文件冷读取和首次视频转码仍是独立延迟来源；不迁库、不改schema、不移动缓存。


## 缓存窗口：关闭与后台运行

管理员缓存窗口的两种退出行为不同。**关闭**通过现有缓存API提交 `pause`，仅在服务器确认 `desired=paused` 后收起；当前扫描或视频完成后停止后续处理，已完成缓存与清单保留，之后可继续。已暂停状态直接收起，不重复发命令。Esc及点击遮罩也执行暂停后关闭。命令进行中禁用重复退出和其他操作；暂停失败或未确认时保留窗口及可重试错误。初次状态读取失败也不能静默关闭，仍须取得暂停确认。

**后台运行**只收起，不发送控制请求；仅最新读取状态为运行中且没有提交命令时可用。消息提示后台继续，重开窗口重新获取服务器状态。关闭浏览器、刷新、跳转页面或断网不能可靠发送暂停，请使用窗口内“关闭”确认。控制针对所有同事共享的服务器任务，不是每个浏览器独立任务。本次不改数据库schema、缓存位置或后台调度。

浏览器回归：`npm run build --prefix frontend` 后在本机提供 `frontend/dist`，执行 `DT_UI_TEST_URL=http://127.0.0.1:<port> DT_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node --test frontend/tests/video-cache-panel.test.mjs`。按需启用的测试在浏览器拦截并模拟API，覆盖扫描/转码关闭、失败重试、重复退出、后台继续、重新读状态、键盘焦点及窄屏，不修改正式缓存。

数据集机器人型号读取现有 PostgreSQL `info_json.robot_type`（非空字符串去除首尾空白，否则未记录）。旧目录设备字段、后缀保持原含义。界面提供型号筛选统计及完整目录后缀查看复制；不涉及结构迁移、目录重命名、KPFS 扫描或重新导入。详见 DataTools `docs/robot-metadata.zh-CN.md`。
