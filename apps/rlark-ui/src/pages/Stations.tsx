import { useEffect, useMemo, useRef, useState } from "react";
import {
  AlertTriangle,
  Check,
  CircleCheck,
  CircleHelp,
  CircleX,
  Copy as CopyIcon,
  Fingerprint,
  ListFilter,
  MonitorSmartphone,
  Network,
  ShieldCheck,
  Unplug,
  X,
} from "lucide-react";
import {
  clustersApi,
  stationsApi,
  type StationItem,
  type StationObservationItem,
  type StationRebindOperation,
} from "../backend";
import { PageToolbar, RefreshOverlay } from "../components/shared";
import { useAutoRefresh } from "../hooks";
import type { Copy } from "../i18n";
import type { ClusterSummary } from "../types";
import { copyText } from "../utils/clipboard";
import {
  clusterForStation,
  formatRelativeTime,
  shortUID,
  stationDisplayName,
  stationNetworkState,
  stationPhase,
  stationStatusClass,
  stationUptime,
  type StationNetworkState,
} from "../utils/stationDisplay";
import "./Stations.css";

function CopyValue({ value, zh }: { value?: string; zh: boolean }) {
  const [copied, setCopied] = useState(false);
  if (!value) return <strong>—</strong>;
  return (
    <span className="station-copy-value">
      <code className="inline-code" title={value}>{value}</code>
      <button
        type="button"
        className={copied ? "icon-button copy-confirmed" : "icon-button"}
        aria-label={zh ? "复制" : "Copy"}
        title={copied ? (zh ? "已复制" : "Copied") : zh ? "复制" : "Copy"}
        onClick={() => void copyText(value).then((ok) => {
          setCopied(ok);
          if (ok) window.setTimeout(() => setCopied(false), 1500);
        })}
      >
        {copied ? <Check size={13} /> : <CopyIcon size={13} />}
      </button>
    </span>
  );
}

function NetworkBadge({ state, zh }: { state: StationNetworkState; zh: boolean }) {
  const Icon = state === "verified" ? CircleCheck : state === "failed" ? CircleX : CircleHelp;
  const label = state === "verified" ? (zh ? "已验证" : "Verified") : state === "failed" ? (zh ? "探测失败" : "Probe failed") : (zh ? "未知" : "Unknown");
  return <span className={`station-network station-network-${state}`}><Icon size={14} aria-hidden="true" />{label}</span>;
}

function newRequestID() {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = [...bytes].map((value) => value.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export function StationsPage({ copy: c }: { copy: Copy }) {
  const zh = c.nav.stations === "场站节点";
  const [items, setItems] = useState<StationItem[]>([]);
  const [clusters, setClusters] = useState<ClusterSummary[]>([]);
  const [clusterID, setClusterID] = useState("");
  const [phase, setPhase] = useState("");
  const [search, setSearch] = useState("");
  const [selectedID, setSelectedID] = useState("");
  const [continueToken, setContinueToken] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState("");
  const [rebindOpen, setRebindOpen] = useState(false);
  const [rebindLoading, setRebindLoading] = useState(false);
  const [rebindPending, setRebindPending] = useState(false);
  const [rebindError, setRebindError] = useState("");
  const [observations, setObservations] = useState<StationObservationItem[]>([]);
  const [targetNodeName, setTargetNodeName] = useState("");
  const [rebindReason, setRebindReason] = useState("");
  const [requestID, setRequestID] = useState("");
  const [submittedRebind, setSubmittedRebind] = useState<{
    clusterID: string;
    requestID: string;
    stationName: string;
    stationUID: string;
    bindingVersion: number;
    target: StationObservationItem;
    reason: string;
  }>();
  const [trackedOperation, setTrackedOperation] = useState<{
    clusterID: string;
    operation: StationRebindOperation;
  }>();
  const [operationError, setOperationError] = useState("");
  const activeRequest = useRef<AbortController | null>(null);
  const observationRequest = useRef<AbortController | null>(null);

  useEffect(() => () => observationRequest.current?.abort(), []);

  const load = async (append = false) => {
    if (append) setLoadingMore(true);
    else setLoading(true);
    setError("");
    activeRequest.current?.abort();
    const controller = new AbortController();
    activeRequest.current = controller;
    try {
      const [response, latestClusters] = await Promise.all([stationsApi.list(
        {
          cluster_id: clusterID || undefined,
          limit: "200",
          continue: append ? continueToken || undefined : undefined,
        },
        controller.signal,
      ), clustersApi.list<ClusterSummary>()]);
      if (controller.signal.aborted) return;
      setClusters(latestClusters);
      setItems((current) => (append ? [...current, ...response.items] : response.items));
      setContinueToken(response.continue ?? "");
      if (!append && selectedID && !response.items.some((item) => `${item.metadata.namespace}/${item.metadata.name}` === selectedID)) {
        setSelectedID("");
      }
    } catch (reason) {
      if (controller.signal.aborted) return;
      setError(reason instanceof Error ? reason.message : String(reason));
      if (!append) setItems([]);
    } finally {
      if (activeRequest.current === controller) {
        activeRequest.current = null;
        setLoading(false);
        setLoadingMore(false);
        setRefreshing(false);
      }
    }
  };

  useEffect(() => {
    void load(false);
    return () => {
      activeRequest.current?.abort();
      activeRequest.current = null;
    };
    // Filter changes replace the current snapshot and reset pagination.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [clusterID]);
  useAutoRefresh(() => load(false), 30000);

  const visible = useMemo(() => {
    const query = search.trim().toLocaleLowerCase();
    return items.filter((item) => {
      const matchesPhase = !phase || item.status?.phase === phase;
      const matchesSearch = !query || [
        item.spec.displayName,
        stationDisplayName(item, clusters),
        clusterForStation(clusters, item.metadata.namespace)?.name ?? "",
        item.metadata.name,
        item.metadata.namespace,
        item.spec.binding.nodeName,
        item.spec.binding.nodeUID,
        item.spec.binding.systemUUID ?? "",
      ].some((value) => value.toLocaleLowerCase().includes(query));
      return matchesPhase && matchesSearch;
    });
  }, [items, phase, search, clusters]);
  const selected = selectedID
    ? items.find((item) => `${item.metadata.namespace}/${item.metadata.name}` === selectedID)
    : undefined;

  const refresh = async () => {
    if (refreshing) return;
    setRefreshing(true);
    await load(false);
  };

  const selectedCluster = selected
    ? clusterForStation(clusters, selected.metadata.namespace)
    : undefined;
  const rebindCandidates = selected
    ? observations.filter((item) => item.status?.nodeUID && item.status.nodeUID !== selected.spec.binding.nodeUID)
    : [];

  const openRebind = async () => {
    if (!selected || !selectedCluster || !selected.metadata.uid) return;
    setRebindOpen(true);
    setRebindError("");
    setObservations([]);
    setTargetNodeName("");
    setRebindReason("");
    setSubmittedRebind(undefined);
    setRequestID(newRequestID());
    observationRequest.current?.abort();
    const controller = new AbortController();
    observationRequest.current = controller;
    setRebindLoading(true);
    try {
      const response = await stationsApi.observations(selectedCluster.id, controller.signal);
      if (!controller.signal.aborted) {
        const candidates = response.items.filter((item) => item.status?.nodeUID !== selected.spec.binding.nodeUID);
        setObservations(candidates);
        setTargetNodeName(candidates[0]?.metadata.name ?? "");
      }
    } catch (reason) {
      if (!controller.signal.aborted) setRebindError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      if (observationRequest.current === controller) {
        observationRequest.current = null;
        setRebindLoading(false);
      }
    }
  };

  const submitRebind = async () => {
    if (!selected || !selectedCluster || !selected.metadata.uid || !targetNodeName || !rebindReason.trim() || rebindPending) return;
    const target = observations.find((item) => item.metadata.name === targetNodeName);
    if (!target?.status.nodeUID) return;
    const submitted = submittedRebind ?? {
      clusterID: selectedCluster.id,
      requestID,
      stationName: selected.metadata.name,
      stationUID: selected.metadata.uid,
      bindingVersion: selected.spec.binding.version,
      target,
      reason: rebindReason.trim(),
    };
    setSubmittedRebind(submitted);
    setRebindPending(true);
    setRebindError("");
    try {
      const response = await stationsApi.createRebind({
        cluster_id: submitted.clusterID,
        request_id: submitted.requestID,
        station_name: submitted.stationName,
        station_uid: submitted.stationUID,
        expected_binding_version: submitted.bindingVersion,
        target_node_name: submitted.target.metadata.name,
        target_node_uid: submitted.target.status.nodeUID,
        target_system_uuid: submitted.target.status.systemUUID,
        reason: submitted.reason,
      });
      setTrackedOperation({ clusterID: submitted.clusterID, operation: response.operation });
      setRebindOpen(false);
      setSubmittedRebind(undefined);
    } catch (reason) {
      setRebindError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setRebindPending(false);
    }
  };

  const closeRebind = () => {
    if (rebindPending) return;
    observationRequest.current?.abort();
    setRebindOpen(false);
    setSubmittedRebind(undefined);
    setRebindError("");
  };

  const trackedName = trackedOperation?.operation.metadata.name;
  const trackedClusterID = trackedOperation?.clusterID;
  useEffect(() => {
    if (!trackedName || !trackedClusterID) return;
    const controller = new AbortController();
    let timer = 0;
    let attempts = 0;
    const poll = async () => {
      try {
        const operation = await stationsApi.getRebind(trackedName, trackedClusterID, controller.signal);
        setTrackedOperation((current) => current?.operation.metadata.name === trackedName ? { ...current, operation } : current);
        if (operation.status?.phase === "Applied" || operation.status?.phase === "Rejected") return;
      } catch (reason) {
        if (!controller.signal.aborted) setOperationError(reason instanceof Error ? reason.message : String(reason));
      }
      attempts += 1;
      if (attempts < 40 && !controller.signal.aborted) timer = window.setTimeout(() => void poll(), 3000);
    };
    setOperationError("");
    void poll();
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [trackedName, trackedClusterID]);

  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 15000);
    return () => window.clearInterval(timer);
  }, []);
  useEffect(() => setNow(Date.now()), [items]);

  const summary = useMemo(() => ({
    total: items.length,
    bound: items.filter((item) => item.status?.phase === "Bound").length,
    offline: items.filter((item) => item.status?.phase === "Offline").length,
    conflict: items.filter((item) => item.status?.phase === "Conflict").length,
  }), [items]);

  const closeDrawer = () => {
    if (rebindPending) return;
    closeRebind();
    setSelectedID("");
  };
  const drawerOpen = !!selected;
  useEffect(() => {
    if (!drawerOpen) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") closeDrawer();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  const selectedTarget = observations.find((item) => item.metadata.name === targetNodeName);
  const formatTime = (value?: string) => value ? new Date(value).toLocaleString() : "—";
  const phaseLabel = (value: string) => {
    if (!zh) return value;
    return ({ Bound: "已绑定", Offline: "离线", Conflict: "冲突", Retired: "已退役", Unknown: "未知" } as Record<string, string>)[value] ?? value;
  };
  const phaseOptions = [
    { value: "", label: zh ? "所有状态" : "All phases" },
    ...(["Bound", "Offline", "Conflict", "Retired"] as const).map((value) => ({ value, label: phaseLabel(value) })),
  ];

  return (
    <div className="page-content resource-page stations-page">
      <div className="section-heading">
        <div>
          <span className="eyebrow"><MonitorSmartphone size={13} />{c.nav.stations}</span>
          <h2>{zh ? "场站节点" : "Site Nodes"}</h2>
          <p>{zh ? "场站节点是一台机器在场站里的长期身份和入网记录，不是 Fleet Run 里的 Station（Run 的 Station 引用这里的节点）。查看各成员集群节点的身份、绑定与健康观测。数据来自 station-agent，每 30 秒刷新。" : "A site node is one machine's long-lived identity and network membership in the site. It is not a Station of a Fleet Run, which refers to a site node. Inspect identity, Node binding and health observations across member clusters. Data comes from station-agent and refreshes every 30 seconds."}</p>
        </div>
      </div>

      <section className="storage-overview-grid stations-overview-grid" aria-label={zh ? "场站节点概况" : "Site node overview"}>
        <div className="storage-overview-card purple">
          <span><Fingerprint size={18} /></span>
          <div><small>{zh ? "场站节点总数" : "Site nodes"}</small><strong>{summary.total}</strong><em>{zh ? "已加载的身份记录" : "loaded identities"}</em></div>
        </div>
        <div className="storage-overview-card green">
          <span><ShieldCheck size={18} /></span>
          <div><small>{zh ? "已绑定" : "Bound"}</small><strong>{summary.bound}</strong><em>{zh ? "心跳正常" : "heartbeat fresh"}</em></div>
        </div>
        <div className="storage-overview-card orange">
          <span><Unplug size={18} /></span>
          <div><small>{zh ? "离线" : "Offline"}</small><strong>{summary.offline}</strong><em>{zh ? "心跳超时" : "heartbeat stale"}</em></div>
        </div>
        <div className="storage-overview-card red">
          <span><AlertTriangle size={18} /></span>
          <div><small>{zh ? "冲突" : "Conflict"}</small><strong>{summary.conflict}</strong><em>{zh ? "需要管理员确认重绑" : "needs rebind review"}</em></div>
        </div>
      </section>

      <div className="stations-toolbar">
        <PageToolbar
          placeholder={zh ? "搜索场站节点、节点名或 UID" : "Search site node, Node or UID"}
          value={search}
          onChange={setSearch}
          count={visible.length}
          copy={c}
          onRefresh={refresh}
          refreshing={refreshing}
          filterValue={phase}
          onFilterChange={setPhase}
          filterOptions={phaseOptions}
        />
        <label className="toolbar-filter stations-cluster-filter">
          <ListFilter size={16} />
          <select aria-label={zh ? "成员集群" : "Member cluster"} value={clusterID} onChange={(event) => setClusterID(event.target.value)}>
            <option value="">{zh ? "所有集群" : "All clusters"}</option>
            {clusters.map((cluster) => <option key={cluster.id} value={cluster.id}>{cluster.name} ({cluster.agentNamespace})</option>)}
          </select>
        </label>
      </div>

      {error && <div className="cert-error" role="alert" style={{ marginBottom: 12 }}>{error}</div>}

      <div className={`table-panel refreshable-region${refreshing ? " is-refreshing" : ""}`} aria-busy={refreshing} aria-label={zh ? "场站节点列表" : "Site node list"}>
        {loading && items.length === 0 ? (
          <div className="stations-state muted">{zh ? "正在加载…" : "Loading…"}</div>
        ) : visible.length === 0 ? (
          <div className="stations-state muted">{zh ? "没有匹配的场站节点" : "No matching site nodes"}</div>
        ) : (
          <table className="stations-table">
            <thead><tr>
              <th>{zh ? "场站节点" : "Site node"}</th>
              <th>{zh ? "集群" : "Cluster"}</th>
              <th>{zh ? "节点" : "Node"}</th>
              <th>{zh ? "状态" : "Phase"}</th>
              <th>{zh ? "网络" : "Network"}</th>
              <th>{zh ? "运行时间" : "Uptime"}</th>
              <th>{zh ? "最近上报" : "Last seen"}</th>
            </tr></thead>
            <tbody>
              {visible.map((item) => {
                const key = `${item.metadata.namespace}/${item.metadata.name}`;
                const itemPhase = stationPhase(item);
                return (
                  <tr key={key} className={`clickable-row${selectedID === key ? " selected" : ""}`} onClick={() => setSelectedID(key)}>
                    <td className="stations-name-cell">
                      <button type="button" className="stations-name" onClick={() => setSelectedID(key)}>
                        <strong>{stationDisplayName(item, clusters)}</strong>
                        <small className="muted">{item.metadata.name}</small>
                      </button>
                    </td>
                    <td>{clusterForStation(clusters, item.metadata.namespace)?.name ?? <span className="muted">{zh ? "集群未确定" : "Cluster unresolved"}</span>}</td>
                    <td>
                      <span className="stations-node">{item.spec.binding.nodeName}</span>
                      <code className="inline-code" title={item.spec.binding.nodeUID}>{shortUID(item.spec.binding.nodeUID)}</code>
                    </td>
                    <td><span className={`status ${stationStatusClass(itemPhase)}`}><i />{phaseLabel(itemPhase)}</span></td>
                    <td><NetworkBadge state={stationNetworkState(item)} zh={zh} /></td>
                    <td><small>{itemPhase === "Bound" ? stationUptime(item.status?.bootTime, now, zh) : "—"}</small></td>
                    <td><small title={formatTime(item.status?.lastSeenTime)}>{formatRelativeTime(item.status?.lastSeenTime, now, zh)}</small></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        <RefreshOverlay visible={refreshing} label={zh ? "刷新中" : "Refreshing"} />
      </div>
      {continueToken && <button className="secondary-button stations-more" type="button" onClick={() => void load(true)} disabled={loadingMore}>{loadingMore ? (zh ? "正在加载…" : "Loading…") : (zh ? "加载更多" : "Load more")}</button>}

      {selected && <>
        <div className="addon-drawer-overlay" onClick={closeDrawer} />
        <aside className="addon-drawer station-drawer" role="dialog" aria-modal="true" aria-labelledby="station-drawer-title">
          <div className="addon-drawer-header station-drawer-header">
            <div>
              <span className="eyebrow"><Fingerprint size={13} />{zh ? "身份详情" : "Identity details"}</span>
              <h3 id="station-drawer-title">{stationDisplayName(selected, clusters)}</h3>
              <div className="station-drawer-meta">
                <span className={`status ${stationStatusClass(stationPhase(selected))}`}><i />{phaseLabel(stationPhase(selected))}</span>
                <code className="inline-code">{selected.metadata.name}</code>
              </div>
            </div>
            <button type="button" className="icon-button" onClick={closeDrawer} disabled={rebindPending} aria-label={zh ? "关闭" : "Close"} title={zh ? "关闭" : "Close"}><X size={16} /></button>
          </div>

          <div className="addon-drawer-body station-drawer-body">
            {selected.status?.phase === "Conflict" && <section className="station-conflict-banner" role="note">
              <AlertTriangle size={16} aria-hidden="true" />
              <div>
                <strong>{zh ? "绑定冲突" : "Binding conflict"}</strong>
                <p>{zh ? `节点 ${selected.spec.binding.nodeName} 上报的身份与当前绑定不一致，Station 已暂停接受新的绑定。请核对证据后确认重绑。` : `Node ${selected.spec.binding.nodeName} reported an identity that does not match the current binding. Review the evidence before confirming a rebind.`}</p>
              </div>
            </section>}

            <section className="node-detail-section station-drawer-section">
              <span className="node-detail-label"><Fingerprint size={14} />{zh ? "身份" : "Identity"}</span>
              <div className="node-detail-grid station-detail-grid">
                <div><span className="muted">{zh ? "场站节点 ID" : "Site node ID"}</span><CopyValue value={selected.metadata.name} zh={zh} /></div>
                <div><span className="muted">{zh ? "对象 UID" : "Object UID"}</span><CopyValue value={selected.metadata.uid} zh={zh} /></div>
                <div><span className="muted">{zh ? "集群" : "Cluster"}</span><strong>{selectedCluster?.name ?? (zh ? "集群未确定" : "Cluster unresolved")}</strong></div>
                <div><span className="muted">Namespace</span><strong>{selected.metadata.namespace}</strong></div>
                <div><span className="muted">{zh ? "序号" : "Sequence"}</span><strong>{selected.spec.sequence}</strong></div>
                <div><span className="muted">{zh ? "启动时间" : "Boot time"}</span><strong>{formatTime(selected.status?.bootTime)}</strong></div>
                <div><span className="muted">{zh ? "运行时间" : "Uptime"}</span><strong>{stationUptime(selected.status?.bootTime, now, zh)}</strong></div>
                <div><span className="muted">{zh ? "最近上报" : "Last seen"}</span><strong title={formatTime(selected.status?.lastSeenTime)}>{formatRelativeTime(selected.status?.lastSeenTime, now, zh)}</strong></div>
              </div>
            </section>

            <section className="node-detail-section station-drawer-section">
              <span className="node-detail-label"><Network size={14} />{zh ? "节点绑定" : "Node binding"}</span>
              <div className="node-detail-grid station-detail-grid">
                <div><span className="muted">{zh ? "节点" : "Node"}</span><strong>{selected.spec.binding.nodeName}</strong></div>
                <div><span className="muted">{zh ? "Binding 版本" : "Binding version"}</span><strong>v{selected.spec.binding.version} · {selected.spec.binding.boundBy}</strong></div>
                <div className="station-detail-wide"><span className="muted">Node UID</span><CopyValue value={selected.spec.binding.nodeUID} zh={zh} /></div>
                <div className="station-detail-wide"><span className="muted">System UUID</span><CopyValue value={selected.spec.binding.systemUUID} zh={zh} /></div>
              </div>
            </section>

            <section className="node-detail-section station-drawer-section">
              <span className="node-detail-label"><ShieldCheck size={14} />{zh ? "健康条件" : "Conditions"}</span>
              {selected.status?.conditions?.length ? <ul className="station-conditions">
                {selected.status.conditions.map((condition) => <li key={condition.type}>
                  <span className={`station-condition-dot is-${condition.status.toLowerCase()}`} aria-hidden="true" />
                  <div>
                    <strong>{condition.type}</strong>
                    <small className="muted">{[condition.reason, condition.message].filter(Boolean).join(" · ") || "—"}</small>
                  </div>
                  <div className="station-condition-side">
                    <span>{condition.status}</span>
                    {condition.lastTransitionTime && <small className="muted" title={formatTime(condition.lastTransitionTime)}>{formatRelativeTime(condition.lastTransitionTime, now, zh)}</small>}
                  </div>
                </li>)}
              </ul> : <small className="muted">{zh ? "暂无条件上报" : "No conditions reported"}</small>}
              <small className="muted">{zh ? "网络状态取自 NetworkVerified 条件；IP 与 WireGuard 详情暂未通过 Station API 提供。" : "Network status comes from the NetworkVerified condition; IP and WireGuard details are not exposed by the Station API yet."}</small>
            </section>

            {selected.status?.phase === "Conflict" && <section className="node-detail-section station-drawer-section stations-rebind">
              <span className="node-detail-label"><AlertTriangle size={14} />{zh ? "重绑确认" : "Rebind review"}</span>
              {!rebindOpen ? <div className="station-rebind-start">
                <small className="muted">{zh ? "加载该集群当前已验证的 Node 观测，选择目标节点后提交重绑。" : "Load the cluster's current verified Node observations, then pick a target and submit a rebind."}</small>
                <button type="button" className="secondary-button" onClick={() => void openRebind()} disabled={!selectedCluster || !selected.metadata.uid}>
                  {zh ? "确认重绑…" : "Review rebind…"}
                </button>
              </div> : <div className="stations-rebind-form">
                <table className="station-evidence">
                  <thead><tr><th>{zh ? "证据" : "Evidence"}</th><th>{zh ? "原绑定" : "Current binding"}</th><th>{zh ? "目标观测" : "Target observation"}</th></tr></thead>
                  <tbody>
                    <tr><td>{zh ? "节点" : "Node"}</td><td>{selected.spec.binding.nodeName}</td><td>{selectedTarget?.metadata.name ?? "—"}</td></tr>
                    <tr><td>Node UID</td><td><code className="inline-code" title={selected.spec.binding.nodeUID}>{shortUID(selected.spec.binding.nodeUID)}</code></td><td><code className="inline-code" title={selectedTarget?.status.nodeUID}>{shortUID(selectedTarget?.status.nodeUID)}</code></td></tr>
                    <tr><td>System UUID</td><td><code className="inline-code" title={selected.spec.binding.systemUUID}>{shortUID(selected.spec.binding.systemUUID)}</code></td><td><code className="inline-code" title={selectedTarget?.status.systemUUID}>{shortUID(selectedTarget?.status.systemUUID)}</code></td></tr>
                    <tr><td>{zh ? "最近观测" : "Observed"}</td><td>—</td><td>{selectedTarget ? formatRelativeTime(selectedTarget.status.reportedAt, now, zh) : "—"}</td></tr>
                  </tbody>
                </table>
                {selectedTarget && selected.spec.binding.systemUUID && selectedTarget.status.systemUUID === selected.spec.binding.systemUUID && <small className="muted">
                  {zh ? "systemUUID 相同而 Node UID 不同，可能是同一台机器重装了系统或重新加入集群。" : "Same systemUUID with a different Node UID usually means the same machine was reinstalled or re-joined."}
                </small>}
                <label className="station-field">
                  <span>{zh ? "目标节点（已验证观测）" : "Target node (verified observation)"}</span>
                  <select value={targetNodeName} onChange={(event) => setTargetNodeName(event.target.value)} disabled={rebindLoading || rebindPending || !!submittedRebind}>
                    <option value="">{rebindLoading ? (zh ? "正在加载…" : "Loading…") : (zh ? "选择节点" : "Select a node")}</option>
                    {rebindCandidates.map((item) => <option key={`${item.metadata.name}/${item.status.nodeUID}`} value={item.metadata.name}>{item.metadata.name} · {item.status.nodeUID.slice(0, 8)}</option>)}
                  </select>
                </label>
                <label className="station-field">
                  <span>{zh ? "重绑原因" : "Reason"}</span>
                  <textarea value={submittedRebind?.reason ?? rebindReason} onChange={(event) => setRebindReason(event.target.value)} maxLength={1024} rows={3} disabled={rebindPending || !!submittedRebind} placeholder={zh ? "例如：节点重装系统后重新加入集群" : "e.g. Node was reinstalled and re-joined"} />
                </label>
                {rebindError && <div className="cert-error" role="alert">{rebindError}</div>}
                <small className="muted">{zh ? "提交后由 controller 再次核验目标 Node UID；受理不代表重绑完成。" : "The controller revalidates the target Node UID. Acceptance does not mean the rebind is complete."}</small>
              </div>}
            </section>}

            {trackedOperation && trackedOperation.operation.metadata.namespace === selected.metadata.namespace && trackedOperation.operation.spec.stationRef.name === selected.metadata.name && <section className="node-detail-section station-drawer-section stations-rebind-status" aria-live="polite">
              <span className="node-detail-label">{zh ? "重绑操作" : "Rebind operation"}</span>
              <div className="station-operation-head">
                <span className={`status ${trackedOperation.operation.status?.phase === "Applied" ? "status-online" : trackedOperation.operation.status?.phase === "Rejected" ? "status-failed" : "status-pending"}`}><i />{trackedOperation.operation.status?.phase ?? "Accepted"}</span>
                <code className="inline-code">{trackedOperation.operation.metadata.name}</code>
              </div>
              {trackedOperation.operation.status?.conditions?.map((condition) => <small className="muted" key={condition.type}>{condition.reason ?? condition.message ?? condition.type}</small>)}
              {operationError && <div className="cert-error" role="alert">{operationError}</div>}
            </section>}
          </div>

          {selected.status?.phase === "Conflict" && rebindOpen && <div className="addon-drawer-footer">
            <button type="button" className="secondary-button" onClick={closeRebind} disabled={rebindPending}>{zh ? "取消" : "Cancel"}</button>
            <button type="button" className="primary-button" onClick={() => void submitRebind()} disabled={rebindLoading || rebindPending || !targetNodeName || !rebindReason.trim()}>{rebindPending ? (zh ? "提交中…" : "Submitting…") : submittedRebind ? (zh ? "使用相同请求重试" : "Retry same request") : (zh ? "提交重绑" : "Submit rebind")}</button>
          </div>}
        </aside>
      </>}
    </div>
  );
}
