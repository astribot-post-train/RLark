# S1U 机器人节点（Addon `s1u-robot`）

每台 S1U 机器人 x86 的机型配置，由内嵌目录里的 Addon `s1u-robot`（`apps/rlark/pkg/addons/catalog/s1u-robot`）以一个
DaemonSet 下发到带 `rlark.io/robot-model=s1u` 标签的节点。新机器入网后自动套上；没有人登录机器人或它的 Orin。

| 容器 | 源码 | 保持什么 |
|---|---|---|
| `tuner` | `tuner/node_tuner.py`、`node_profile.sh`、`node_profile.env`、`node_spec.py`、`boot_report.py` | 宿主机符合节点规格：内核隔离核、systemd 默认亲和、CPU 份额、kubelet 的 CPU 管理、内存预留和镜像回收、调频策略、调度器参数（每次开机由 `rlark-s1u-profile.service` 重新套上）、经 Orin 的默认路由。规格需要时重启整机或 kubelet，只在机器人空闲且轮到它那一批时做。发布对 S1U 规格的符合情况。 |
| `orin` | `orin/orin.py`、`orin/s1u-orin-uplink.service` | 经机器人内网保持 Orin：x86 上网的单元、相机的 USB 规则和按请求的 USB 复位、站点的 adb 密钥、声明的载荷（`orinPayloads`）。Orin 的 ssh 主机密钥首次使用时信任。 |
| `clock` | `clock/clock.py` | 微调厂商时间链源头那块网卡时钟的速率，让 x86 和 Orin 跟上站点时间源（`timeSource`）。 |

## 和其他组件的接口

- **在用标记**：宿主机上 `/run/rlark/in-use/<组件名>`，每个组件一个文件，内容是原因，只在这个组件真忙时留着。目录里有任何文件
  （包括自己的）机器人就算在用：所有会打扰机器人的动作只看这一个信号，不分是谁问。本 Addon 在厂商驱动运行或电机带电时写
  `s1u-robot`（设备层接着 Station 时写 `fleet-station`）；有任何文件时不重启、不切换载荷。RLark 自己在宿主机上的程序只在目录为空时替换，
  节点 agent 把它报到节点注解 `rlark.io/in-use` 上，供集群里做判断的一方读。
- **节点规格文件**：宿主机上 `/etc/rlark/s1u/node_profile.env`（`S1U_*` 变量），设备层只读它，得到自己的 CPU 和机器人地址。
- **相机复位**：设备层在可以的时候（它知道有没有 Station 接着）写 `/run/rlark/s1u/camera-reset-request`（`{"id", "cameras"}`）；
  每个 id 做一次，结果写到 `/run/rlark/s1u/camera-reset-result`，并在节点上记一条 Event。
- **Orin 主机密钥**：状态在节点标签 `s1u.rlark.io/orin-state` 和注解 `s1u.rlark.io/orin`（JSON）。Orin 的密钥变了之后，由人把
  节点注解 `s1u.rlark.io/orin-host-key-confirm` 设成它现在出示的指纹来确认。
- **Orin 载荷**：`orinPayloads: 名字=镜像,...`。每个镜像里有 `/payload`（以及 `/bin/sh` 和 `cp`）；其中可执行的 `install` 由
  root 在 Orin 上执行：新版本解包后执行 `install`，切换到它时执行 `install restart`，不再声明时执行 `install remove`。同目录的
  `rlark.env` 给出机器人的地址和账户。RLark 只依赖镜像引用。
- **节点标签和注解**：通用的放在 `rlark.io/` 下（`node-profile-state`、`node-conformance`、`rollout-wave`、`rollout-state`、
  `restart-request`、`clock-state`、`boot-report` 等），只有 S1U 才有的放在 `s1u.rlark.io/` 下（`orin-state`、`orin-host-key`、
  `body-parameters` 等）。

## 构建和测试

```bash
docker build -f extensions/s1u/Dockerfile -t <仓库>/rlark/s1u-robot:<标签> .
python3 -m pytest extensions/s1u/tests
go test ./apps/rlark/pkg/addons -run S1U
```

目录里的 `bootstrap/orin-uplink.sh` 是生成的：`python3 extensions/s1u/orin/orin.py --uplink-setup`，有测试保证两者一致。新机器
第一次入网时由 Station API 交给它的 Orin（站点的 `leafUplink.setup`）。

## 撤除

把 `profile` 设为 `removed`，等 `rlark.io/node-profile-state`、`s1u.rlark.io/orin-state`、`rlark.io/clock-state` 都变成
`removed` 后再卸载 Addon。内核命令行在下次开机时回到装规格之前的那份；不会自己重启。Orin 上的上网单元保留：那是机器人到站点的路。
