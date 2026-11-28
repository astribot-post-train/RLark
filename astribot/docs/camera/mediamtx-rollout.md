# MediaMTX Single-Station Rollout

The release Compose file contains a `camera-media` profile for MediaMTX. The profile is off in ordinary `docker compose` and RLark release runs. The console's WHEP build flag defaults to `0`, and Gateway leaves WHEP unavailable while `RLARK_CAMERA_WHEP_ORIGIN` is empty. These defaults preserve the existing JPEG/MJPEG camera path.

This is a pre-production template. It does not establish that a station, robot network, browser network, credentials, or ICE route is ready for use. The UI build allowlist and the Gateway's server-side `RLARK_CAMERA_WHEP_NODES` allowlist must both name the pilot node. Gateway rejects other nodes before it requests an S1U source lease.

## Network and credentials

Gateway and MediaMTX share only the internal `camera-media` network for WHEP signaling and the Control API. Compose publishes neither WHEP `8889` nor Control API `9997` on the host. RTSP/TCP `8554` and ICE `8189/UDP` and `8189/TCP` bind to configurable host addresses. Their defaults are loopback, so no station or browser can connect until the site configures the correct host addresses and firewall rules.

MediaMTX accepts authenticated RTSP publishing only from `MEDIAMTX_STATION_SOURCE_CIDR` and only for the configured pilot agent and node's camera paths. The RTSP host bind address must be on the authorized robot ingress. Limit the source CIDR and host firewall rule to the robot network. RTSP publisher, WHEP reader, and Control API accounts use separate credentials. The MediaMTX and Gateway credential files hold matching values for their respective WHEP reader and Control API accounts. Gateway rejects a shared username for those two roles. The RTSP publisher username and password in MediaMTX must match `rtsp-username` and `rtsp-password` in the S1U Addon's `cameraSourceSecret`. Keep the Gateway source key separate and equal to that Secret's `source-key`.

Keep credentials in two root-owned JSON files with mode `0600`, outside the release environment. The Gateway file is `${R}/identity/rlark-gateway/camera-credentials.json` and uses the keys `readerUser`, `readerPassword`, `controlUser`, `controlPassword`, and `sourceKey`. The MediaMTX source file defaults to `/etc/rlark/mediamtx-credentials.json` and uses `publishUser`, `publishPassword`, `readerUser`, `readerPassword`, `controlUser`, and `controlPassword`. Keep role usernames and passwords distinct. The Gateway identity directory is already mounted read-only, and Gateway reads those values directly from the file instead of its environment. When WHEP is disabled, Gateway does not read the optional credential file. Recreate `rlark-gateway` after changing its protected credential file so it reloads the new values. The render script reads the MediaMTX source file and writes actual YAML to `/etc/rlark/mediamtx.yml` with mode `0600`; Compose mounts that file read-only. It validates the source CIDR and writes concrete agent, node, and ICE host values. Compose does not interpolate credentials or CIDRs into its model, and MediaMTX receives neither credentials nor site settings through its environment. Compose change detection tracks the mounted file path, not its content, so recreate `mediamtx` after each render. Never place credential values in a release manifest, build argument, command, log, or report. The S1U publisher passes its RTSP URI to FFmpeg, so privileged host process inspection can see that process argument even though application logs suppress it. Protect station host administration accordingly.

Set the non-secret Compose variables in the site's release environment: `RLARK_CAMERA_WHEP_ORIGIN=http://mediamtx:8889`, `RLARK_CAMERA_WHEP_CONTROL_ORIGIN=http://mediamtx:9997`, `RLARK_CAMERA_WHEP_NODES`, `MEDIAMTX_CONFIG_FILE`, `MEDIAMTX_RTSP_BIND_ADDRESS`, `MEDIAMTX_ICE_BIND_ADDRESS`, `MEDIAMTX_STATION_SOURCE_CIDR`, `MEDIAMTX_PILOT_AGENT`, `MEDIAMTX_PILOT_NODE`, and `MEDIAMTX_ICE_ADDITIONAL_HOST`. Keep these values non-secret. Set `RLARK_CAMERA_WHEP_NODES` to the same node as `VITE_STATION_WHEP_NODES`. Render the config before starting the profile:

   ```bash
   python3 astribot/release/control-plane/render_mediamtx_config.py \
     --environment-file /var/lib/rlark/control-plane.env \
     --credentials-file /etc/rlark/mediamtx-credentials.json \
     --output /etc/rlark/mediamtx.yml
   ```

The render command reports only the output path. The ICE additional host must be reachable from the intended browser network. Bind the ICE ports only on that network and restrict them with the host firewall. Signaling through the console or Gateway does not relay ICE media.

## Single-station switch

S1U Addon v1.7.5 adds the optional RTSP publisher configuration. Upgrading the Addon without those values leaves the publisher disabled.

1. Identify an idle or non-production S1U by an authorized inventory source. Record its exact Kubernetes node name, owning agent, Addon namespace, camera readiness, and approved station and browser networks. Confirm Gateway admin authorization for that station. The UI allowlist is not an authorization boundary.
2. Provision the protected Gateway and MediaMTX JSON credential files and the non-secret Compose variables. Generate distinct random values for RTSP publisher, WHEP reader, Control API, and Gateway source-key roles. Set `RLARK_CAMERA_WHEP_NODES` to the exact pilot node used by the UI build. Configure the source CIDR, agent, node, ICE address, and RTSP bind address from approved network evidence, then render `/etc/rlark/mediamtx.yml` with the command above.
3. Build a release with WHEP enabled only for that node. `VITE_STATION_WHEP_NODES` accepts exact comma-separated Kubernetes node names, and the release build rejects an enabled flag without a node list.

   ```bash
   VITE_STATION_WHEP=1 VITE_STATION_WHEP_NODES=<node-name> \
     astribot/release/build.sh <rlark-commit> <fleet-commit>
   ```

4. Use the existing `rlarkctl release apply` review and approval process for the release that adds the Compose profile and feature-enabled console. Set the S1U Addon's `cameraRTSPPrefix` to the configured RTSP ingress and agent path, ending in `$(NODE_NAME)`, and set `cameraSourceSecret` to an `rlark-system` Secret with matching `source-key`, `rtsp-username`, and `rtsp-password` values. The Gateway allowlist, UI allowlist, and MediaMTX paths must name the same node and agent.
5. After the release prerequisites and review are complete, start only the optional media service with the site's existing Compose environment file.

   ```bash
   docker compose -p rlark -f /var/lib/rlark/rlark.compose.yaml \
     --env-file /var/lib/rlark/control-plane.env --profile camera-media \
     up -d --no-deps --force-recreate mediamtx
   ```

6. Validate the selected station, Gateway authorization, snapshot fallback, browser ICE candidate and media route, and control-plane health. Do not infer ICE reachability from a successful WHEP HTTP exchange.

## Rollback

Build and apply a release with `VITE_STATION_WHEP=0` to return every station to the existing camera transport. Remove the S1U Addon's `cameraRTSPPrefix` and `cameraSourceSecret` through the normal release process, then stop the optional `mediamtx` service. Leave the Gateway's WHEP origin empty so WHEP remains disabled, then remove the generated MediaMTX config when the profile is no longer needed. No Gateway, IAM, robot control, claim/release, or station networking path depends on MediaMTX. The rollback duration has not been measured.

## Validation boundary

Compose validation checks the default-off model and the enabled profile's mount and port model. The render script validates required settings, CIDR syntax, role separation, protected source-file permissions, and produces concrete YAML. Neither check alone validates production ingress, robot camera readiness, authorization, or ICE connectivity. The isolated protocol fixture remains separate and its fixed addresses and test credentials must not be copied into this deployment.
