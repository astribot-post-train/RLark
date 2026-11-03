import {
  Activity,
  AlertCircle,
  Cpu,
  HardDrive,
  MemoryStick,
  Network,
  Package,
  Server,
  Download,
} from "lucide-react";
import type { Phase } from "../data";
import type { Copy } from "../i18n";
import type { CRDNode } from "../types";
import {
  categoryLabels,
  formatResourceQuantity,
  getGPUResourceKey,
  getNodeDeviceModel,
  getNodeDiskUsage,
  getNodeCategories,
  getNodeGPUModel,
  getNodeLocation,
  getResourceUsagePercent,
  nodeStatusReasonLabel,
  parseResourceQuantity,
} from "../utils/nodes";
import { rawResourceState } from "../utils/rawNodeResources";
import { StatusBadge } from "./shared";
import { formatChinaDateTime } from "../utils/time";

// formatBytes converts a byte count into a human-readable string (B/KB/MB/GB).
function formatBytes(bytes: number): string {
  if (!bytes || bytes < 0) return "0B";
  const GB = 1024 * 1024 * 1024;
  const MB = 1024 * 1024;
  const KB = 1024;
  if (bytes >= GB) return (bytes / GB).toFixed(1) + "GB";
  if (bytes >= MB) return (bytes / MB).toFixed(1) + "MB";
  if (bytes >= KB) return (bytes / KB).toFixed(1) + "KB";
  return bytes + "B";
}

export function NodeOverview({
  node,
  copy: c,
  requestedFallback = {},
}: {
  node: CRDNode;
  copy: Copy;
  requestedFallback?: Record<string, string>;
}) {
  const zh = c.nav.overview === "总览";
  const raw = node.spec.agentType === "Raw";
  const resourceState = rawResourceState(node);
  const blocked = raw
    ? !resourceState.schedulable
    : Boolean(node.spec.unschedulable);
  const phase = (node.status?.phase ?? "Offline") as Phase;
  const labels = node.metadata.labels ?? {};
  const addresses = node.status?.addresses ?? [];
  const internalAddress =
    addresses.find((address) => address.type === "InternalIP")?.address ??
    addresses[0]?.address ??
    "—";
  const categories = getNodeCategories(node);
  const capacity = node.status?.capacity ?? {};
  const allocatable = node.status?.allocatable ?? {};
  const reportedUsed = node.status?.used ?? {};
  const used =
    raw || Object.keys(reportedUsed).length > 0
      ? reportedUsed
      : requestedFallback;
  const pullProgress = node.status?.pullProgress ?? [];
  const getPercent = (key: string) => {
    const rawUsed = used[key];
    if (raw && used[key] === undefined) return null;
    if (!rawUsed && (allocatable[key] ?? capacity[key])) return 0;
    if (rawUsed?.endsWith("%"))
      return Math.min(100, Math.max(0, Number.parseFloat(rawUsed)));
    const usedNumber = parseResourceQuantity(key, rawUsed);
    const capacityNumber = parseResourceQuantity(
      key,
      allocatable[key] ?? capacity[key],
    );
    return usedNumber !== null && capacityNumber !== null && capacityNumber > 0
      ? Math.min(
          100,
          Math.max(0, Math.round((usedNumber / capacityNumber) * 100)),
        )
      : null;
  };
  const formatUsedResource = (key: string) => {
    if (node.spec.agentType === "Raw" && used[key] === undefined) return "—";
    const raw = used[key];
    if (!raw && (allocatable[key] ?? capacity[key])) {
      return formatResourceQuantity(key, "0", zh);
    }
    if (raw?.endsWith("%")) {
      const capacityValue = parseResourceQuantity(
        key,
        allocatable[key] ?? capacity[key],
      );
      const percent = Number.parseFloat(raw);
      if (capacityValue !== null && Number.isFinite(percent)) {
        return formatResourceQuantity(
          key,
          String((capacityValue * percent) / 100),
          zh,
        );
      }
    }
    return formatResourceQuantity(key, raw, zh);
  };
  const formatAvailableResource = (key: string) => {
    if (raw && (!resourceState.current || used[key] === undefined)) return "—";
    if (raw && !resourceState.schedulable)
      return formatResourceQuantity(key, "0", zh);
    const total = parseResourceQuantity(key, allocatable[key] ?? capacity[key]);
    const requested = used[key]?.endsWith("%")
      ? total !== null
        ? (total * Number.parseFloat(used[key])) / 100
        : null
      : parseResourceQuantity(key, used[key]);
    if (total === null) return "—";
    return formatResourceQuantity(
      key,
      String(Math.max(0, total - (requested ?? 0))),
      zh,
    );
  };
  const gpuResourceKey = getGPUResourceKey(node);
  const resourceKeys = Array.from(
    new Set([
      ...Object.keys(capacity),
      ...Object.keys(allocatable),
      ...Object.keys(used),
    ]),
  );
  const hasGPUResource = resourceKeys.includes(gpuResourceKey);
  const deviceResourceKeys = resourceKeys.filter(
    (key) => key === "rlinf.io/device" || key.startsWith("rlinf.io/device-"),
  );
  const deviceLabel = (key: string) => {
    const model = key.replace(/^rlinf\.io\/device-?/, "");
    if (!model) return zh ? "端侧设备" : "Edge device";
    const reportedModel = labels["rlark.io/model"];
    if (deviceResourceKeys.length === 1 && reportedModel) return reportedModel;
    return model
      .split("-")
      .filter(Boolean)
      .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
      .join(" ");
  };
  const diskKey = "ephemeral-storage";
  const diskUsage = getNodeDiskUsage(node);
  const diskPercent =
    diskUsage?.percent ??
    getResourceUsagePercent(
      diskKey,
      used[diskKey],
      allocatable[diskKey] ?? capacity[diskKey],
    );
  const diskWarning =
    node.status?.diskPressure === true ||
    (diskPercent !== null && diskPercent >= 90);
  const resourceItems = [
    { key: "cpu", label: "CPU", icon: Cpu, available: true },
    {
      key: "memory",
      label: zh ? "内存" : "Memory",
      icon: MemoryStick,
      available: true,
    },
    {
      key: "ephemeral-storage",
      label: zh ? "磁盘" : "Storage",
      icon: HardDrive,
      available: Boolean(
        diskUsage ??
        allocatable["ephemeral-storage"] ??
        capacity["ephemeral-storage"],
      ),
    },
    {
      key: gpuResourceKey,
      label: "GPU",
      icon: Activity,
      available: hasGPUResource,
    },
    ...deviceResourceKeys.map((key) => ({
      key,
      label: deviceLabel(key),
      icon: Package,
      available: true,
    })),
    ...(deviceResourceKeys.length === 0
      ? [
          {
            key: "rlinf.io/device",
            label: zh ? "端侧设备" : "Edge device",
            icon: Package,
            available: false,
          },
        ]
      : []),
  ];
  const created = formatChinaDateTime(node.metadata.creationTimestamp);
  return (
    <div className="node-resource-detail node-insight-detail">
      <header className="node-insight-hero">
        <div className="node-insight-identity">
          <span className={`node-insight-icon ${phase.toLowerCase()}`}>
            <Server size={22} />
          </span>
          <div>
            <span className="eyebrow">
              {zh ? "节点运行概况" : "Node overview"}
            </span>
            <h3>{node.metadata.name}</h3>
            <p>
              <Network size={13} /> {node.metadata.namespace ?? "default"}
              <span />
              {internalAddress}
            </p>
          </div>
        </div>
        <div className="node-insight-state">
          <StatusBadge phase={phase} copy={c} />
          <span className={blocked ? "schedule-chip blocked" : "schedule-chip"}>
            {blocked
              ? zh
                ? "已停止调度"
                : "Unschedulable"
              : zh
                ? "可调度"
                : "Schedulable"}
          </span>
        </div>
      </header>

      {(node.status?.reason || raw) && (
        <div className="node-health-message">
          <Activity size={15} />
          <span>
            {raw
              ? `${resourceState.reason} · ${zh ? "采集时间" : "Observed"}: ${formatChinaDateTime(resourceState.observedAt)}`
              : nodeStatusReasonLabel(node.status?.reason, zh)}
          </span>
        </div>
      )}

      <div className="node-insight-facts">
        <div>
          <small>{zh ? "节点类型" : "Node type"}</small>
          <strong>
            {categories
              .map((value) =>
                zh ? categoryLabels[value].zh : categoryLabels[value].en,
              )
              .join(" / ")}
          </strong>
        </div>
        <div>
          <small>{zh ? "接入形态" : "Agent type"}</small>
          <strong>{node.spec.agentType ?? "—"}</strong>
        </div>
        <div>
          <small>{zh ? "系统 / 架构" : "System / Arch"}</small>
          <strong>
            {node.status?.nodeInfo?.operatingSystem ?? "—"} ·{" "}
            {node.status?.nodeInfo?.architecture ?? "—"}
          </strong>
        </div>
        <div>
          <small>{zh ? "Agent 版本" : "Agent version"}</small>
          <strong>{node.status?.nodeInfo?.agentVersion ?? "—"}</strong>
        </div>
      </div>

      <div className="node-insight-layout">
        <div className="node-insight-main">
          <section className="node-insight-section">
            <div className="node-insight-section-head">
              <div>
                <span>{zh ? "健康与容量" : "Health & capacity"}</span>
                <small>
                  {raw
                    ? zh
                      ? "CPU/内存为容器上限，GPU 为授权集合；已预留量为 RLark 整个 Notebook 独占预留，并非实时利用率。维护和外部占用单独阻止调度。"
                      : "CPU/memory use container bounds; GPUs use the authorized set. Reserved means exclusive RLark Notebook reservations, not live utilization. Maintenance and external occupancy separately block scheduling."
                    : zh
                      ? "资源占用按运行 Worker 申请统计，压力来自节点状态"
                      : "Usage uses Worker requests; pressure uses node status"}
                </small>
              </div>
            </div>
            <div className="node-capacity-grid">
              {resourceItems.map(({ key, label, icon: Icon, available }) => {
                const percent =
                  key === "ephemeral-storage"
                    ? diskPercent
                    : available
                      ? getPercent(key)
                      : null;
                const usedLabel =
                  key === "ephemeral-storage" && diskUsage
                    ? zh
                      ? "已使用"
                      : "Used"
                    : raw
                      ? zh
                        ? "已预留"
                        : "Reserved"
                      : zh
                        ? "已请求"
                        : "Requested";
                const usedValue =
                  key === "ephemeral-storage" && diskUsage
                    ? formatResourceQuantity(
                        key,
                        String(diskUsage.usedBytes),
                        zh,
                      )
                    : formatUsedResource(key);
                const totalValue =
                  key === "ephemeral-storage" && diskUsage
                    ? formatResourceQuantity(
                        key,
                        String(diskUsage.capacityBytes),
                        zh,
                      )
                    : formatResourceQuantity(
                        key,
                        allocatable[key] ?? capacity[key],
                        zh,
                      );
                const availableValue =
                  key === "ephemeral-storage" && diskUsage
                    ? formatResourceQuantity(
                        key,
                        String(diskUsage.availableBytes),
                        zh,
                      )
                    : formatAvailableResource(key);
                return (
                  <div
                    className={`node-capacity-card${
                      key === "ephemeral-storage" && diskWarning
                        ? " is-warning"
                        : ""
                    }`}
                    key={key}
                  >
                    <div className="node-capacity-title">
                      <span>
                        <Icon size={16} />
                      </span>
                      <strong>{label}</strong>
                      {key === "ephemeral-storage" && diskWarning && (
                        <span
                          className="node-capacity-alert"
                          tabIndex={0}
                          aria-label={
                            zh ? "查看磁盘异常详情" : "View disk alert details"
                          }
                        >
                          <AlertCircle size={16} />
                          <span
                            className="node-capacity-alert-tooltip"
                            role="status"
                          >
                            <strong>
                              {zh
                                ? "健康与容量告警"
                                : "Health & capacity alert"}
                            </strong>
                            <small>
                              {node.status?.diskPressure
                                ? zh
                                  ? "节点存在磁盘压力，请及时清理空间"
                                  : "Node disk pressure detected; free up space"
                                : zh
                                  ? `磁盘使用率已达到 ${diskPercent ?? 90}%，请及时清理空间`
                                  : `Disk usage reached ${diskPercent ?? 90}%; free up space`}
                            </small>
                          </span>
                        </span>
                      )}
                    </div>
                    <div className="node-capacity-progress">
                      <div className="node-capacity-track">
                        <i style={{ width: `${percent ?? 0}%` }} />
                      </div>
                      <b>
                        {available
                          ? percent === null
                            ? "—"
                            : `${percent}%`
                          : raw
                            ? "—"
                            : zh
                              ? "无"
                              : "None"}
                      </b>
                    </div>
                    <div className="node-capacity-amounts">
                      <span>
                        <em>{usedLabel}</em>
                        <strong>
                          {available
                            ? usedValue
                            : raw
                              ? "—"
                              : zh
                                ? "无"
                                : "None"}
                        </strong>
                      </span>
                      <span>
                        <em>{zh ? "总量" : "Total"}</em>
                        <strong>
                          {available
                            ? totalValue
                            : raw
                              ? "—"
                              : zh
                                ? "无"
                                : "None"}
                        </strong>
                      </span>
                      <span>
                        <em>{zh ? "剩余量" : "Available"}</em>
                        <strong>
                          {available
                            ? availableValue
                            : raw
                              ? "—"
                              : zh
                                ? "无"
                                : "None"}
                        </strong>
                      </span>
                    </div>
                  </div>
                );
              })}
            </div>
          </section>

          {pullProgress.length > 0 && (
            <section className="node-insight-section">
              <div className="node-insight-section-head">
                <div>
                  <span>
                    <Download size={13} style={{ verticalAlign: -2 }} />{" "}
                    {zh ? "镜像拉取进度" : "Image Pull Progress"}
                  </span>
                  <small>
                    {zh
                      ? "节点正在拉取的镜像及其进度"
                      : "Images currently being pulled on this node"}
                  </small>
                </div>
              </div>
              <div className="node-pull-progress-list">
                {pullProgress.map((p, i) => {
                  const pct =
                    p.total > 0
                      ? Math.min(
                          100,
                          Math.round((p.downloaded / p.total) * 100),
                        )
                      : 0;
                  const isPulling = p.status === "pulling";
                  return (
                    <div className="node-pull-progress-entry" key={i}>
                      <div className="node-pull-progress-header">
                        <code className="node-pull-image" title={p.image}>
                          {p.image}
                        </code>
                        <span className={`node-pull-status chip-${p.status}`}>
                          {p.message || p.status}
                        </span>
                      </div>
                      {isPulling && p.total > 0 && (
                        <div className="node-pull-progress-bar">
                          <i style={{ width: `${pct}%` }} />
                          <b>{pct}%</b>
                        </div>
                      )}
                      {(p.total > 0 || p.speed > 0) && (
                        <div className="node-pull-progress-meta">
                          {p.total > 0 && (
                            <span>
                              {formatBytes(p.downloaded)} /{" "}
                              {formatBytes(p.total)}
                            </span>
                          )}
                          {p.speed > 0 && <span>{formatBytes(p.speed)}/s</span>}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            </section>
          )}
        </div>

        <aside className="node-insight-side">
          <section className="node-insight-section">
            <div className="node-insight-section-head">
              <div>
                <span>{zh ? "基础信息" : "Details"}</span>
              </div>
            </div>
            <dl className="node-info-list">
              <div>
                <dt>{zh ? "所属集群" : "Cluster"}</dt>
                <dd>{node.metadata.namespace ?? "default"}</dd>
              </div>
              <div>
                <dt>{zh ? "内部地址" : "Internal IP"}</dt>
                <dd>
                  <code>{internalAddress}</code>
                </dd>
              </div>
              <div>
                <dt>{zh ? "物理位置" : "Location"}</dt>
                <dd>
                  {getNodeLocation(node) || (zh ? "未标注" : "Unlabeled")}
                </dd>
              </div>
              <div>
                <dt>{zh ? "节点分类" : "Category"}</dt>
                <dd>
                  {categories
                    .map((value) =>
                      zh ? categoryLabels[value].zh : categoryLabels[value].en,
                    )
                    .join(" / ")}
                </dd>
              </div>
              <div>
                <dt>{zh ? "GPU 型号" : "GPU model"}</dt>
                <dd>
                  {hasGPUResource
                    ? getNodeGPUModel(node) || (zh ? "未标注" : "Unlabeled")
                    : zh
                      ? "无 GPU"
                      : "No GPU"}
                </dd>
              </div>
              <div>
                <dt>{zh ? "端设备型号" : "Device model"}</dt>
                <dd>
                  {deviceResourceKeys.length > 0
                    ? getNodeDeviceModel(node) ||
                      deviceResourceKeys.map(deviceLabel).join(zh ? "、" : ", ")
                    : zh
                      ? "无端设备"
                      : "No device"}
                </dd>
              </div>
              <div>
                <dt>{zh ? "内核版本" : "Kernel"}</dt>
                <dd>{node.status?.nodeInfo?.kernelVersion ?? "—"}</dd>
              </div>
              <div>
                <dt>{zh ? "创建时间" : "Created"}</dt>
                <dd>{created}</dd>
              </div>
            </dl>
          </section>
        </aside>
      </div>
    </div>
  );
}
