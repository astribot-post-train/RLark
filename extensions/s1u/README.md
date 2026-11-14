# S1U robot node (Addon `s1u-robot`)

The model configuration of every S1U robot x86, delivered by the catalog Addon `s1u-robot`
(`apps/rlark/pkg/addons/catalog/s1u-robot`) as one DaemonSet on nodes labelled `rlark.io/robot-model=s1u`. A new
robot gets it when it joins; nobody logs in to the robot or its Orin.

| Container | Source | What it keeps |
|---|---|---|
| `tuner` | `tuner/node_tuner.py`, `node_profile.sh`, `node_profile.env`, `node_spec.py`, `boot_report.py` | The host follows the node profile: kernel CPU isolation, systemd's default affinity, CPU shares, kubelet's CPU manager, reserved memory and image collection, the governor, the scheduler's tunables (re-applied at every boot by `rlark-s1u-profile.service`), the default route through the Orin. Restarts the robot or kubelet when the profile needs it, only while the robot is idle and its rollout wave allows. Publishes conformance to the S1U specification. |
| `orin` | `orin/orin.py`, `orin/s1u-orin-uplink.service` | The Orin over the robot's internal network: the x86's uplink unit, the cameras' USB rule and USB resets on request, the site's adb key, and the declared payloads (`orinPayloads`). Trusts the Orin's ssh host key on first use. |
| `power` | `device/power.py` | The vendor control centre's power / driver / restart buttons, through the vendor's ROS master (rospy), with the vendor page's rules; power off and restart wait while the robot is in use unless forced with a reason. Listens on the Unix socket `/run/rlark/services/s1u-power.sock` only (`GET /power`, `POST /power {action, force, reason}`); RLark reaches it on the node and records who pressed. `autoDriver` / `autoPower` parameters. |
| `video` | `device/video_server.py`, `video.py`, `camera.py`, `preview.py` | The body's cameras (`bodies.json`) from the vendor's compressed ROS topics to MediaMTX (WebRTC on :18889; a viewing session's token, checked on a loopback port, is the only access), and the camera wall's strip from the device layer's thumbnails (`/run/rlark/s1u/preview.bin`). Control on the Unix socket `/run/rlark/services/s1u-video.sock` only (`GET /status`, `POST /viewers`, `PUT`/`DELETE /viewers/<id>`, `GET /preview[/<camera>]`). |
| `clock` | `clock/clock.py` | Trims the rate of the NIC clock at the root of the vendor's time chain so the x86 and the Orin follow the site's time source (`timeSource`). |

## Interfaces with other components

- **In use** — `/run/rlark/in-use/<component>` on the host, one file per component, holding the reason, kept only
  while that component is busy. The robot is in use while the directory holds any file, its own included: one
  signal for every disruption, whoever asks. This Addon writes `s1u-robot` while the vendor driver runs or the motors
  are powered (a Station attached to the device layer writes `fleet-station`); it restarts nothing and switches no
  payload while any file is there. RLark's own host programs are replaced only while the directory is empty, and the
  node agent reports it on the Node (annotation `rlark.io/in-use`) for what decides in the cluster.
- **Node profile** — `/etc/rlark/s1u/node_profile.env` on the host (`S1U_*` variables): the device layer reads its
  CPUs and the robot's addresses from it, read-only.
- **Bodies** — `/etc/rlark/s1u/bodies.json` on the host: each body's cameras (`{"s1u": ["head", "wrist_left",
  "wrist_right"], "s1u_observer": ["head"]}`), the one list the device layer and the video read.
- **Camera reset** — the device layer writes `/run/rlark/s1u/camera-reset-request` (`{"id", "cameras"}`) when it may
  (it knows whether a Station is attached); each id is done once, the result in `/run/rlark/s1u/camera-reset-result` (`{"id", "ok", "detail", "finished_at"}`)
  and a Node Event.
- **Orin host key** — state in the Node's label `s1u.rlark.io/orin-state` and annotation `s1u.rlark.io/orin` (JSON).
  After the Orin's key changed, a person confirms the new one by setting the Node's annotation
  `s1u.rlark.io/orin-host-key-confirm` to the presented fingerprint.
- **Orin payloads** — `orinPayloads: NAME=IMAGE,...`. Each image holds `/payload` (and `/bin/sh` and `cp`); its
  executable `install` is run as root on the Orin: `install` after a new version is unpacked, `install restart` when
  it is switched to, `install remove` when it is no longer declared. `rlark.env` beside it gives the robot's
  addresses and account. RLark depends on the image reference only.
- **Node labels and annotations** — generic ones under `rlark.io/` (`node-profile-state`, `node-conformance`,
  `rollout-wave`, `rollout-state`, `restart-request`, `clock-state`, `boot-report`, …), S1U-only ones under
  `s1u.rlark.io/` (`orin-state`, `orin-host-key`, `body-parameters`, …).

## Ports

What s1u-robot listens on in the host's network of a robot x86 (the containers are on the host's network). Apart from
MediaMTX's defaults (8889, 8189, 8554, 9997) and 8890, which other software on a robot may hold.

| Port | Bound to | Who | For |
|---|---|---|---|
| 18889/tcp | all addresses | video (MediaMTX) | WebRTC (WHEP) for the browser; the address comes with a viewing session |
| 18189/udp, 18189/tcp | all addresses | video (MediaMTX) | WebRTC ICE |
| 18890/tcp | 127.0.0.1 | video (video_server.py) | MediaMTX's authorization callback (a viewing session's token) |
| 18554/tcp | 127.0.0.1 | video (MediaMTX) | RTSP, the camera publishers' |
| 19997/tcp | 127.0.0.1 | video (MediaMTX) | MediaMTX's API |

Power and video's control are Unix sockets only (`/run/rlark/services/s1u-{power,video}.sock`), no TCP port.

## Build and test

```bash
docker build -f extensions/s1u/Dockerfile -t <registry>/rlark/s1u-robot:<tag> .
python3 -m pytest extensions/s1u/tests
go test ./apps/rlark/pkg/addons -run S1U
```

`bootstrap/orin-uplink.sh` in the catalog is generated: `python3 extensions/s1u/orin/orin.py --uplink-setup`; a test
keeps the two equal. Station API hands it to a new robot's Orin when the robot first joins (the site's
`leafUplink.setup`).

## Removal

Set `profile=removed` and wait until `rlark.io/node-profile-state`, `s1u.rlark.io/orin-state` and
`rlark.io/clock-state` all read `removed`, then uninstall the Addon. The kernel command line returns to its copy from
before the profile at the next boot; nothing restarts by itself. The Orin's uplink unit stays: it is the robot's way to
the site.
