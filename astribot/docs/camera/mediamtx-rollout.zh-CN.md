# MediaMTX 单站灰度

发布 Compose 文件包含一个 `camera-media` profile，用于 MediaMTX。普通 `docker compose` 和 RLark 发布默认不会启动此 profile。控制台 WHEP 构建开关默认为 `0`，且 `RLARK_CAMERA_WHEP_ORIGIN` 为空时 Gateway 不开放 WHEP。这些默认值保留现有 JPEG/MJPEG 相机路径。

这是预生产模板，不能证明某台机器人、机器人网络、浏览器网络、凭据或 ICE 路由已经具备上线条件。UI 构建 allowlist 和 Gateway 服务端 `RLARK_CAMERA_WHEP_NODES` allowlist 必须同时指定试点节点。Gateway 会在请求 S1U 来源租约前拒绝其他节点。

## 网络与凭据

Gateway 和 MediaMTX 仅通过内部 `camera-media` 网络访问 WHEP 信令和 Control API。Compose 不会把 WHEP `8889` 或 Control API `9997` 发布到宿主机。RTSP/TCP `8554` 和 ICE `8189/UDP`、`8189/TCP` 绑定到可配置的宿主地址，默认均为 loopback。站点配置正确的宿主地址和防火墙规则前，机器人和浏览器都无法连接。

MediaMTX 只接受来自 `MEDIAMTX_STATION_SOURCE_CIDR` 的已认证 RTSP 发布，并且仅允许配置的试点 Agent、节点相机路径。RTSP 宿主绑定地址必须位于授权的机器人入口网络。将源 CIDR 和宿主防火墙规则限制到机器人网络。RTSP publisher、WHEP reader 和 Control API 使用不同凭据。MediaMTX 与 Gateway 凭据文件分别保存匹配的 WHEP reader 和 Control API 账号。Gateway 会拒绝 WHEP reader 和 Control API 两个角色共用用户名。MediaMTX publisher 用户名和密码必须与 S1U Addon `cameraSourceSecret` 对应 Secret 中的 `rtsp-username` 和 `rtsp-password` 相同。Gateway 源密钥须单独保管，并与该 Secret 中的 `source-key` 相同。

凭据保存在两份 root 所有、权限为 `0600` 且位于发布环境之外的 JSON 文件中。Gateway 文件位于 `${R}/identity/rlark-gateway/camera-credentials.json`，字段为 `readerUser`、`readerPassword`、`controlUser`、`controlPassword` 和 `sourceKey`。MediaMTX 输入文件默认是 `/etc/rlark/mediamtx-credentials.json`，字段为 `publishUser`、`publishPassword`、`readerUser`、`readerPassword`、`controlUser` 和 `controlPassword`。各角色用户名和密码都应不同。Gateway 身份目录已经只读挂载，Gateway 直接读取文件中的值，不将其放入进程环境。WHEP 关闭时，Gateway 不读取可选凭据文件。修改受保护的 Gateway 凭据文件后需重建 `rlark-gateway`，使其重新读取凭据。渲染脚本读取 MediaMTX 输入文件，并生成 `/etc/rlark/mediamtx.yml`，权限为 `0600`；Compose 只读挂载该文件。脚本校验来源 CIDR，并将 Agent、节点和 ICE host 写成实际值。凭据和 CIDR 不会由 Compose 插值进入模型，MediaMTX 环境中也不包含凭据或站点配置。Compose 变更检测只跟踪挂载文件路径，不会识别文件内容变更，因此每次渲染后都需重建 `mediamtx`。不要把凭据值写入发布清单、构建参数、命令、日志或报告。S1U 发布器会把 RTSP URI 传给 FFmpeg，因此具备宿主机进程检查权限的管理员可以看到对应进程参数，尽管应用日志会屏蔽该值。应保护场站宿主机的管理权限。

在站点发布环境中设置非机密 Compose 变量：`RLARK_CAMERA_WHEP_ORIGIN=http://mediamtx:8889`、`RLARK_CAMERA_WHEP_CONTROL_ORIGIN=http://mediamtx:9997`、`RLARK_CAMERA_WHEP_NODES`、`MEDIAMTX_CONFIG_FILE`、`MEDIAMTX_RTSP_BIND_ADDRESS`、`MEDIAMTX_ICE_BIND_ADDRESS`、`MEDIAMTX_STATION_SOURCE_CIDR`、`MEDIAMTX_PILOT_AGENT`、`MEDIAMTX_PILOT_NODE` 和 `MEDIAMTX_ICE_ADDITIONAL_HOST`。所有这些值都必须是非机密信息。`RLARK_CAMERA_WHEP_NODES` 必须与 `VITE_STATION_WHEP_NODES` 使用同一节点。启动 profile 前先渲染配置：

   ```bash
   python3 astribot/release/control-plane/render_mediamtx_config.py \
     --environment-file /var/lib/rlark/control-plane.env \
     --credentials-file /etc/rlark/mediamtx-credentials.json \
     --output /etc/rlark/mediamtx.yml
   ```

渲染命令只报告输出路径。ICE additional host 必须能从目标浏览器网络到达。ICE 端口只绑定在该网络，并通过宿主防火墙限制来源。控制台或 Gateway 代理信令不代表它会中继 ICE 媒体。

## 单站开关

1. 通过获授权的 inventory 数据源识别一台空闲或非生产 S1U。记录准确的 Kubernetes 节点名、所属 Agent、Addon 命名空间、相机就绪状态、批准的机器人与浏览器网络。确认 Gateway 对该站点具有 admin 授权。UI allowlist 不是授权边界。
2. 配置 Gateway 和 MediaMTX 两份受保护的 JSON 凭据文件及非机密 Compose 变量。RTSP publisher、WHEP reader、Control API 和 Gateway source-key 使用不同的随机值。`RLARK_CAMERA_WHEP_NODES` 必须设置为 UI 构建使用的准确试点节点。根据已批准的网络证据配置源 CIDR、Agent、节点、ICE 地址和 RTSP 绑定地址，然后使用上述命令渲染 `/etc/rlark/mediamtx.yml`。
3. 构建只为该节点启用 WHEP 的发布。`VITE_STATION_WHEP_NODES` 接收精确匹配的逗号分隔 Kubernetes 节点名，启用开关但未提供节点列表时发布构建会拒绝继续。

   ```bash
   VITE_STATION_WHEP=1 VITE_STATION_WHEP_NODES=<node-name> \
     astribot/release/build.sh <rlark-commit> <fleet-commit>
   ```

4. 使用现有 `rlarkctl release apply` 评审和审批流程应用包含 Compose profile 与功能开关控制台的发布。通过正常发布流程设置 S1U Addon 的 `cameraRTSPPrefix`，指向 RTSP 入口和 Agent 路径并以 `$(NODE_NAME)` 结尾；`cameraSourceSecret` 使用 `rlark-system` 中 `source-key`、`rtsp-username`、`rtsp-password` 分别匹配 Gateway 和 MediaMTX 的 Secret。Gateway allowlist、UI allowlist 和 MediaMTX 路径必须使用同一节点与 Agent。
5. 完成发布前置条件和评审后，通过站点现有 Compose 环境文件仅启动可选媒体服务。

   ```bash
   docker compose -p rlark -f /var/lib/rlark/rlark.compose.yaml \
     --env-file /var/lib/rlark/control-plane.env --profile camera-media \
     up -d --no-deps --force-recreate mediamtx
   ```

6. 验证指定站点、Gateway 授权、快照回退、浏览器 ICE candidate 与媒体路由及控制面健康。WHEP HTTP 交换成功不能作为 ICE 可达的证据。

## 回滚

构建并通过发布流程应用 `VITE_STATION_WHEP=0` 的新发布，使所有站点恢复现有相机传输。通过正常发布流程移除 S1U Addon 的 `cameraRTSPPrefix` 和 `cameraSourceSecret`，然后停止可选的 `mediamtx` 服务。保持 Gateway WHEP origin 为空以禁用 WHEP；不再需要 profile 时删除生成的 MediaMTX 配置。Gateway、IAM、机器人控制、认领/释放和站点网络均不依赖 MediaMTX。回滚耗时尚未测量。

## 验证边界

Compose 校验会检查默认关闭模型以及启用 profile 的挂载和端口模型。渲染脚本会校验必填项、CIDR 语法、角色分离和受保护源文件权限，并生成实际配置 YAML。两者均不能单独验证生产入口、机器人相机状态、授权或 ICE 连通性。隔离协议 fixture 与本部署分开，其固定地址和测试凭据不得复制到本配置。
