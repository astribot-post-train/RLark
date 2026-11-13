# DataTools NAS and cloud runtime


## Current database contract (2026-11-12)

DataTools runs as cloud RLark Job `jo-8b0e28b7d1104716` at <http://10.254.240.4:30800/>. PostgreSQL is the independent local development-host Docker service `fleet-data-postgres`, WireGuard `10.254.240.1:25432`, database `fleet_data`, schema `datatools`. PGDATA is a native local ext4 Docker volume, not NAS, KS3 or cloud-node storage. RLark, Station and Fleet databases are unchanged; future Fleet integration remains unimplemented.

Source/exports retain the full NAS paths below, and input paths remain KPFS. Cloud `/var/lib/rlark-datatools` retains JWT, video files and the old SQLite rollback snapshot. `DT_DATABASE_URL_FILE=/run/datatools-postgres/url` requires TLS verify-full and never silently falls back; `DT_STATE_DIR=/var/lib/rlark-datatools`. One fixed-node replica remains required; no active-active support. Allow at most 15 minutes cutover downtime, validate with API writes rejected in maintenance mode, then stop/update/start the Job to reopen writes. Once PG accepts writes, the old SQLite database cannot be restarted as a lossless rollback.

Encrypted NAS backups live at `/mnt/post_train/RL/data-tools-data/postgres-backups`, with key stored separately locally and in cloud Secret escrow. Per-table import hashes and restore from an encrypted NAS backup passed rehearsal. Hourly/daily scheduling depends on the local host, WG and NAS; one-hour RPO is a target, and full host-loss RTO is not validated. See the DataTools repository `docs/postgresql.md`.

## Archived pre-migration deployment and NAS experiment

Old Job IDs, SQLite and “not implemented” statements below describe the earlier deployment only. The NAS PGDATA experiment remains failed; the local native-storage contract above supersedes that candidate. Retained source/JWT/SQLite support rollback only under the documented write-stop gate.

## NAS deployment contract (2026-11-12)

The service runs on Kingsoft cloud through RLark Job `jo-1ca9f18ee83b4e9b`, not a provider training submission. Access over WireGuard: <http://10.254.240.4:30800/>. The container listens on 8000; RLark `exposedPorts` publishes TCP 30800. No GPU is requested. Keep one replica on node `10-0-2-196`: requests 8 CPU/16 GiB, limits 16 CPU/32 GiB. Kubernetes/Ray Running alone does not establish application readiness; check HTTP and startup logs.

| Purpose | Host/share and container path | Access |
| --- | --- | --- |
| Source and built frontend | `/mnt/post_train/RL/user/liangcai/data_tools` | NAS, read-only |
| New exports | `/mnt/post_train/RL/data-tools-data/exports` | NAS, writable |
| Dataset inputs | `/mnt/xc_kpfs/regular/ericzhang/datasets` | KPFS, read-only |
| Existing SQLite, JWT, video-cache | `/var/lib/rlark-datatools` | Cloud node ext4, writable |
| Release records | `/mnt/post_train/RL/manifests/data_tools/<release-id>` | NAS, operator only |

Use these full paths directly; this deployment has no `/workspace`, `/exports`, `/state` or `/fleet` aliases. Working directory and `PYTHONPATH` are the source root. The image contains dependencies only. The sanitized `deploy/rlark/datatools-nas-job.json` is a suspended template for Gateway submission; creation assigns a business Job ID. Never start a second writer against this state directory or claim port 30800 while the first Job is active.

Release `datatools-20261112T055239Z` has 192 checksum-verified files. Startup runs `scripts/validate_runtime.py`, verifies NAS/KPFS/local-state filesystem types and release hashes, and refuses a missing existing DB/JWT file. It does not create a new installation. This check is an admission check, not a cryptographic signature or continuous mount monitor. Trusted operators control the writable NAS release and manifest. It does not detect every unexpected file or provide snapshot isolation: stop the Job before changing the fixed source entry, publish a coherent checkout and rebuilt frontend, refresh the manifest, validate, then start. Do not edit a running release in place. Requirements changes also require rebuilding the runtime image.

The cloud uses static Retain PVs/PVCs `datatools-nas-source` (ROX) and `datatools-nas-exports` (RWX), driver `rclone.csi.veloxpack.io`; observed filesystem is `fuse.rclone`. Secret references are in kube-system; credentials must never enter the Job/example. The dedicated CSI mounts use 5-second directory cache and 1-second attribute cache, source VFS cache off, export VFS writes with zero writeback delay. These are deployment-specific settings, not a change to RLark's generated storage defaults. Prefix scoping is not a NAS account ACL boundary. Reported 10 GiB PV capacity is not a NAS hard quota. Read-only source prevents application writes, but other NAS clients can still change it. Cold Python imports and per-file release verification can take minutes; declare ready only after Uvicorn startup and HTTP success. NAS disconnect/reconnect recovery and sustained large-export throughput still need a controlled test; do not infer them from a small read/write probe.

Phase one retains SQLite WAL and flock on local cloud ext4, with the existing JWT secret, cache identity and paused cache intent. PostgreSQL/shared Fleet metadata are a later migration and are not implemented here. `DT_DB_PATH` and `DT_JWT_SECRET_FILE` use the full state path; `DT_STARTUP_SCAN_MODE=off` avoids a broad startup scan. Root configuration remains the existing imported KPFS dataset root. This change does not reimport, delete or rewrite dataset paths. Old KPFS source and historical exports remain available. New exports default to `DT_EXPORT_DIR`, with `<task_name>/<config.name>` (or `<config.name>` without a task); numeric export IDs are not directory names. Choose distinct export names; existing nonempty outputs are rejected. Explicit user output paths remain supported and are not automatically confined to this default.

### Release, validation and recovery

1. Build frontend, run backend tests and publish a checksum manifest without credentials, local environments or databases. Retain release ID, git status and source diff. Validate source as read-only and state as read-only in a temporary RLark Job with no exposed port. Verify exports by writing a unique probe and reading its hash from a separate NAS client; remove only that probe.
2. Confirm no import/export/delete or active cache conversion is running. Preserve cache intent. Use SQLite online backup, check `PRAGMA integrity_check`, and record dataset/episode/user counts. The current local backup is `/var/lib/rlark-datatools/backups/datatools-20261112T055239Z/data_tools.db`; it is not disaster recovery against node loss. Arrange and test an independent protected backup before claiming that guarantee.
3. Create the replacement suspended through RLark Gateway. Suspend the old Job and wait until its actual Pod and exposed Service are gone, then start the new Job. Preserve single-writer node placement; no direct Deployment mutation is needed.
4. Check startup admission, backend import path, WG HTTP 200, anonymous admin API 401, authenticated dataset/episode and cache endpoints, byte and suffix ranges (206), default export path, UI cache progress and unchanged baseline counts (329 datasets, 36,410 episodes, one user). Repeat after a full Job stop/start. Backend automated tests also cover imports/exports and failure admission.
5. If admission or application checks fail, suspend the replacement and wait for its Pod to disappear before starting old Job `jo-c13f4fc097ac40f8` against the same state. Do not overwrite a live DB with a snapshot. Restore a database only with all writers stopped and an explicitly assessed recovery point. Existing backup lacks JWT/cache; retain those separately for disaster recovery. Old code must remain schema-compatible before rolling back.

WG reachability and application login remain required; the NodePort does not establish authentication. Progressive transcoding playback remains disabled: incompatible uncached media waits for full conversion; completed shared cached files serve immediately.


**Validated export limitation:** the real cloud rclone/SMB mount passed Parquet write/read and single-file link fallback to copy, but directory symlink creation failed with EIO. Image-directory exports using `_safe_link_dir` are therefore unsupported on this destination. Do not advertise all export modes as working or silently copy entire image trees; a separate explicit copy-mode change and capacity/throughput validation are needed. Current imported online samples are v2.0; v3 has automated fixtures but no live v3 sample was available in this database.


## PostgreSQL NAS admission — 2026-11-12

The isolated RLark probe `jo-08a06c1e39e94e8d` tested rclone 1.71.2 with writes cache and zero writeback delay. After successful fsync, an independent SMB client could not find the still-open file at 5, 10 and 15 seconds; after close, the complete payload was readable and its SHA-256 matched. The exact rclone implementation syncs the local VFS cache, not the remote open file. This fails the proposed NAS-only PGDATA durability gate. It does not establish PostgreSQL transaction loss, NAS power-loss behavior or suitability of other NAS protocols.

Formal DataTools remains on SQLite; no production PostgreSQL data or application adapter was introduced. The remaining migration stages are conditional on a storage design that passes admission. Test evidence is retained under `/mnt/post_train/RL/data-tools-data/postgres-tests/pg-nas-20261112T062553Z`. See the DataTools repository `docs/postgresql-nas-validation.md` and its Chinese counterpart for the full experiment.

PostgreSQL 15.19 checksum-enabled initdb also failed while creating template0, reporting a missing `base/1/2605.63aec2fb.partial` file; its cleanup subsequently hit the 180-second timeout. SQL/load/recovery stages were not run. The probe is stopped and dedicated Kubernetes storage resources are cleaned up; nonsecret evidence remains on NAS.


## Connection reuse and latency

`DT_DATABASE_POOLING_ENABLED=true` (default) enables application-lifecycle-owned pools. Async requests use only their owning event loop's pool (4 retained connections, 4 overflow); background synchronous work uses a separate thread-safe pool (2 retained, 2 overflow). Each operation checks out its own connection; transactions are not shared. Pools grow lazily and wait at most 2 seconds, then return generic HTTP 503 on exhaustion. Checkout health checks discard dead idle connections; uncommitted transactions roll back on return. Failed writes are never automatically replayed. PostgreSQL temporary inventory tables use ON COMMIT DROP. Closing the application stops background users before disposing pools. The app role limit remains 32; the pools allow at most 12 connections plus dedicated lock sessions and nonpooled tools/scans. This is a single-worker deployment budget, not a multi-worker sizing promise.

Explicit lifecycle binding prevents hidden global engines: requests and inherited background tasks use the runtime; temporary scan event loops, migrations, external tools and leader/scanner session locks retain NullPool. Advisory lock sessions must never reconnect silently or return to a general pool. Runtime loss still fences background work by exiting. Disable the switch and stop/start the RLark Job to revert connection reuse, while retaining PostgreSQL and all current data. Credentials/TLS changes also require a Job restart to replace existing pooled sessions.

Cache progress reads reuse the authenticated request connection and one combined status/count query; filesystem accounting runs in a worker thread. Statistics total queries are combined without caching results. Progress polling starts its next request two seconds after the previous one settles and stops on panel close. No authorization or business-result cache is introduced. PostgreSQL date filtering rejects malformed dates with equivalent semantics to SQLite's digit-only test; HTTP field shapes remain unchanged.

Performance acceptance uses the same WG URL, three warm-ups and 30 samples per endpoint: target medians <=700ms for datasets/statistics, <=800ms for cache status, and >=40% reduction from baseline. Ten simultaneous clients must not leak connections or produce cross-loop errors. NAS source/asset cold reads and first-video transcoding remain separate latency sources. No schema migration, database move or cache relocation is required.


## Cache dialog: close versus background

The administrator cache dialog has two distinct exit actions. **Close** submits `pause` to the existing cache control API and hides only after the server confirms `desired=paused`. The current scan or video finishes before further processing stops; completed files and inventory remain available for Resume. Already-paused dialogs close without another command. Escape and clicking the backdrop have the same pause-and-close behavior. While a command is pending, repeated exits and other controls are disabled; a failed or unconfirmed pause keeps the dialog open with a retryable error. A failed initial status fetch does not silently dismiss the dialog: Close still requires a pause acknowledgement.

**Run in background** hides without sending a control request, and is enabled only when the latest loaded state says running and no command is pending. A notification confirms background processing; reopening fetches fresh server status. Closing a browser tab, refreshing, navigation away or losing connectivity does not reliably deliver pause. Use the dialog's Close button to confirm pause. Controls affect the shared server task for all colleagues, not a per-browser task. No database schema, cache location or backend scheduling changes are introduced.

Browser regression tests: build with `npm run build --prefix frontend`, serve `frontend/dist` on localhost, then run `DT_UI_TEST_URL=http://127.0.0.1:<port> DT_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node --test frontend/tests/video-cache-panel.test.mjs`. The opt-in tests mock API responses in the browser and cover scan/transcode close, errors/retry, duplicate exits, background continuation, fresh status, keyboard focus and narrow layout; they do not mutate production cache state.

Dataset robot model is read from existing PostgreSQL `info_json.robot_type` (trimmed nonempty string; otherwise unknown). Directory device tokens and suffixes retain legacy meanings. The UI exposes robot model filtering/statistics and full directory-suffix viewing/copying. No schema migration, source directory rename, KPFS scan or reimport is required; see DataTools `docs/robot-metadata.md`.
