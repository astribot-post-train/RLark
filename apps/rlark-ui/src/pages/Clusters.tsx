import {
  Fragment,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
} from "react";
import {
  Activity,
  Check,
  ChevronRight,
  CloudCog,
  Copy as CopyIcon,
  KeyRound,
  Network,
  RefreshCw,
  Server,
  Settings,
  TerminalSquare,
} from "lucide-react";
import type { Phase } from "../data";
import { NodeOverview } from "../components/NodeOverview";
import type { Copy } from "../i18n";
import type { CRDNode } from "../types";
import type { CRDTask } from "../crdModels";
import { useAutoRefresh } from "../hooks";
import { copyText } from "../utils/clipboard";
import {
  categoryLabels,
  getNodeCategory,
  getNodeLocation,
  getNodeResourceSummary,
  isBusinessWorkerNode,
  nodeStatusReasonLabel,
  parseResourceQuantity,
} from "../utils/nodes";
import {
  compareSortValues,
  MetricCard,
  RefreshOverlay,
  SortButton,
  StatusBadge,
  type SortDirection,
} from "../components/shared";
import { NodeResourceBrowser } from "../components/NodeResourceBrowser";
import { formatChinaDateTime } from "../utils/time";

type NodeWorker = {
  id: string;
  crName: string;
  name: string;
  job: string;
  role: string;
  node: string;
  cluster: string;
  domain: string;
  ip: string;
  phase: string;
  requests: Record<string, string | number>;
};

function addRequestedResource(
  totals: Record<string, number>,
  key: string,
  raw?: string | number,
) {
  const value = parseResourceQuantity(key, String(raw ?? ""));
  if (value !== null) totals[key] = (totals[key] ?? 0) + value;
}

export function ClustersPage({
  copy: c,
  initialView,
  selectedNodeName,
  onNavigate,
  onTaskNavigate,
}: {
  copy: Copy;
  initialView?: "clusters" | "nodes";
  selectedNodeName?: string;
  onNavigate?: (name?: string) => void;
  onTaskNavigate?: (name: string) => void;
}) {
  const zh = c.nav.overview === "总览";
  const searchParams = new URLSearchParams(window.location.search);
  const requestedCategory = searchParams.get("category");
  const initialCategory =
    requestedCategory === "cloud" ||
    requestedCategory === "edge" ||
    requestedCategory === "robot" ||
    requestedCategory === "unknown"
      ? requestedCategory
      : "all";
  const initialQuery = searchParams.get("city") ?? searchParams.get("q") ?? "";
  const [realNodes, setRealNodes] = useState<CRDNode[]>([]);
  const [nodeWorkloads, setNodeWorkloads] = useState<
    Record<string, { jobs: string[]; workers: number }>
  >({});
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const resourceView = initialView ?? "clusters";
  const [selectedClusterNs, setSelectedClusterNs] = useState<string | null>(
    null,
  );

  const fetchNodes = async (isInitial = true) => {
    if (isInitial) setLoading(true);
    setError("");
    try {
      const [nodes, tasks, pods] = await Promise.all([
        nodesApi.list(),
        tasksApi.list(),
        podsApi.list(),
      ]);
      setRealNodes(nodes);
      const taskJobs = new Map<string, string>(
        tasks.map(
          (task: {
            metadata?: { name?: string; labels?: Record<string, string> };
          }) => [
            task.metadata?.name ?? "",
            task.metadata?.labels?.["rlinf.io/job"] ?? "",
          ],
        ),
      );
      const workloadMap = new Map<
        string,
        { jobs: Set<string>; workers: number }
      >();
      for (const pod of pods as Array<{
        spec?: { taskName?: string };
        status?: { phase?: string; node?: string };
      }>) {
        if (pod.status?.phase !== "Running" || !pod.status?.node) continue;
        const nodeName = pod.status.node as string;
        const current = workloadMap.get(nodeName) ?? {
          jobs: new Set<string>(),
          workers: 0,
        };
        const jobName = taskJobs.get(pod.spec?.taskName ?? "");
        if (jobName) current.jobs.add(jobName);
        current.workers += 1;
        workloadMap.set(nodeName, current);
      }
      setNodeWorkloads(
        Object.fromEntries(
          [...workloadMap].map(([name, workload]) => [
            name,
            { jobs: [...workload.jobs].sort(), workers: workload.workers },
          ]),
        ),
      );
    } catch (e) {
      setRealNodes([]);
      setNodeWorkloads({});
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  };

  useAutoRefresh(fetchNodes, 10000);

  const handleRefresh = async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      await fetchNodes(false);
    } finally {
      setRefreshing(false);
    }
  };

  const workerNodes = useMemo(
    () => realNodes.filter(isBusinessWorkerNode),
    [realNodes],
  );

  const clustersList = useMemo(() => {
    const map = new Map<string, CRDNode[]>();
    for (const n of workerNodes) {
      const ns = n.metadata.namespace ?? "default";
      if (!map.has(ns)) map.set(ns, []);
      map.get(ns)!.push(n);
    }
    return Array.from(map.entries()).sort((a, b) => a[0].localeCompare(b[0]));
  }, [workerNodes]);

  const selectedCluster =
    clustersList.find(([ns]) => ns === selectedClusterNs) ?? clustersList[0];
  const selectedClusterNodes = selectedCluster?.[1] ?? [];

  const onlineClusters = clustersList.filter(([, nsNodes]) =>
    nsNodes.some((n) => n.status?.phase === "Online"),
  ).length;
  const totalNodes = workerNodes.length;

  if (loading) {
    return (
      <div className="page-content resource-page cluster-page">
        <div className="section-heading">
          <div>
            <span className="eyebrow">{c.clusters.eyebrow}</span>
            <h2>{c.clusters.title}</h2>
            <p>{c.clusters.desc}</p>
          </div>
        </div>
        <p className="muted">{zh ? "加载中..." : "Loading..."}</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className="page-content resource-page cluster-page">
        <div className="section-heading">
          <div>
            <span className="eyebrow">{c.clusters.eyebrow}</span>
            <h2>{c.clusters.title}</h2>
            <p>{c.clusters.desc}</p>
          </div>
          <button
            className="secondary-button"
            onClick={handleRefresh}
            disabled={refreshing}
            aria-busy={refreshing}
          >
            <RefreshCw
              size={16}
              className={refreshing ? "job-action-loading" : ""}
            />
            {refreshing
              ? zh
                ? "刷新中..."
                : "Refreshing..."
              : c.common.refresh}
          </button>
        </div>
        <div className="cert-error" style={{ marginBottom: 12 }}>
          {error}
        </div>
      </div>
    );
  }

  const selectedNodeObj =
    realNodes.find((n) => n.metadata.name === selectedNodeName) ?? null;

  if (selectedNodeName && selectedNodeObj) {
    return (
      <div className="page-content resource-page node-detail-page">
        <div className="section-heading">
          <div>
            <button
              className="plain-button back-button"
              onClick={() => onNavigate?.()}
            >
              ← {zh ? "返回节点列表" : "Back"}
            </button>
            <span className="eyebrow">
              <Settings size={13} />
              {zh ? "节点详情" : "Node Detail"}
            </span>
          </div>
        </div>
        <NodeDetailReal
          node={selectedNodeObj}
          copy={c}
          onTaskNavigate={onTaskNavigate}
        />
      </div>
    );
  }

  return (
    <div
      className={`page-content resource-page cluster-page${
        resourceView === "clusters"
          ? " cluster-overview-page refreshable-region page-refresh-region"
          : ""
      }${resourceView === "clusters" && refreshing ? " is-refreshing" : ""}`}
      aria-busy={resourceView === "clusters" ? refreshing : undefined}
    >
      <div className="section-heading">
        <div>
          <span className="eyebrow">{c.clusters.eyebrow}</span>
          <h2>
            {resourceView === "nodes"
              ? zh
                ? "节点管理"
                : "Node Management"
              : c.clusters.title}
          </h2>
          <p>
            {resourceView === "nodes"
              ? zh
                ? "统一查看和管理各集群中的算力节点与具身设备。"
                : "View and manage compute nodes and embodied devices across clusters."
              : c.clusters.desc}
          </p>
        </div>
        {resourceView === "clusters" && (
          <button
            className="secondary-button"
            onClick={handleRefresh}
            disabled={refreshing}
            aria-busy={refreshing}
          >
            <RefreshCw
              size={16}
              className={refreshing ? "job-action-loading" : ""}
            />
            {refreshing
              ? zh
                ? "刷新中..."
                : "Refreshing..."
              : c.common.refresh}
          </button>
        )}
      </div>
      {resourceView === "clusters" && (
        <section className="cluster-overview-grid">
          <MetricCard
            icon={Network}
            tone="blue"
            label={zh ? "集群总数" : "Clusters"}
            value={`${clustersList.length}`}
            note={`${onlineClusters} ${c.status.Online}`}
          />
          <MetricCard
            icon={Server}
            tone="mint"
            label={zh ? "节点总数" : "Nodes"}
            value={`${totalNodes}`}
            note={`${workerNodes.filter((n) => n.status?.phase === "Online").length} ${c.status.Online}`}
          />
          <MetricCard
            icon={Activity}
            tone="orange"
            label={zh ? "在线率" : "Online Rate"}
            value={`${totalNodes > 0 ? Math.round((workerNodes.filter((n) => n.status?.phase === "Online").length / totalNodes) * 100) : 0}%`}
            note={`${workerNodes.filter((n) => n.status?.phase === "Online").length}/${totalNodes} ${zh ? "在线" : "online"}`}
          />
        </section>
      )}
      {resourceView === "clusters" && (
        <>
          <section className="cluster-topology-grid">
            <div className="panel cluster-map-card">
              <div className="panel-title">
                <div>
                  <span>{zh ? "分布" : "Distribution"}</span>
                  <h3>{zh ? "集群分布" : "Cluster Distribution"}</h3>
                </div>
              </div>
              <div className="cluster-map">
                <div className="map-grid" />
                {clustersList.map(([ns, nsNodes], index) => (
                  <button
                    key={ns}
                    className={`map-pin pin-${index} ${selectedCluster?.[0] === ns ? "active" : ""}`}
                    onClick={() => setSelectedClusterNs(ns)}
                  >
                    <span>
                      <CloudCog size={15} />
                    </span>
                    <strong>{ns}</strong>
                    <small>
                      {nsNodes.length} {zh ? "节点" : "nodes"}
                    </small>
                  </button>
                ))}
              </div>
            </div>
            <ClusterDetailReal
              namespace={selectedCluster?.[0] ?? ""}
              nodes={selectedClusterNodes}
              copy={c}
            />
          </section>
        </>
      )}
      {resourceView === "nodes" && (
        <section className="nodes-resource-section">
          <NodeResourceBrowser
            nodes={workerNodes}
            nodeWorkloads={nodeWorkloads}
            copy={c}
            initialCategory={initialCategory}
            initialQuery={initialQuery}
            onRefresh={handleRefresh}
            refreshing={refreshing}
            onSelectNode={(name) => onNavigate?.(name)}
          />
        </section>
      )}
      <RefreshOverlay
        visible={resourceView === "clusters" && refreshing}
        label={zh ? "正在刷新集群概览" : "Refreshing cluster overview"}
      />
    </div>
  );
}

export function ClusterDetailReal({
  namespace,
  nodes: clusterNodes,
  copy: c,
}: {
  namespace: string;
  nodes: CRDNode[];
  copy: Copy;
}) {
  const zh = c.nav.overview === "总览";
  const onlineCount = clusterNodes.filter(
    (n) => n.status?.phase === "Online",
  ).length;
  const phase: Phase = onlineCount > 0 ? "Online" : "Offline";
  const sortedNodes = [...clusterNodes].sort((a, b) =>
    a.metadata.name.localeCompare(b.metadata.name, zh ? "zh-CN" : "en", {
      numeric: true,
    }),
  );
  return (
    <div className="panel selected-cluster-panel">
      <div className="cluster-detail-header">
        <div className="cluster-detail-title">
          <CloudCog size={18} />
          <strong>{namespace}</strong>
          <StatusBadge phase={phase} copy={c} />
        </div>
        <div className="cluster-detail-meta">
          <span>
            {clusterNodes.length} {zh ? "节点" : "nodes"}
          </span>
          <i className="dot" />
          <span>
            {onlineCount} {zh ? "在线" : "online"}
          </span>
          <i className="dot" />
          <span>
            {Array.from(
              new Set(clusterNodes.map((n) => n.spec.agentType ?? "—")),
            ).join(", ") || "—"}
          </span>
        </div>
      </div>
      <div className="cluster-node-table-wrap">
        <table className="cluster-node-table">
          <thead>
            <tr>
              <th>{zh ? "节点名称" : "Name"}</th>
              <th>{zh ? "类型" : "Type"}</th>
              <th>{zh ? "接入状态" : "Connection"}</th>
              <th>{zh ? "上次心跳" : "Last heartbeat"}</th>
              <th>{zh ? "状态变化时间" : "Status changed"}</th>
              <th>{zh ? "物理位置" : "Location"}</th>
              <th>{zh ? "节点 IP" : "Node IP"}</th>
              <th>{zh ? "资源与空闲" : "Resources"}</th>
              <th>{zh ? "型号" : "Model"}</th>
            </tr>
          </thead>
          <tbody>
            {sortedNodes.map((node) => {
              const category = getNodeCategory(node);
              const categoryInfo = categoryLabels[category];
              const resource = getNodeResourceSummary(node, zh);
              const address =
                node.status?.addresses?.find(
                  (item) => item.type === "InternalIP",
                )?.address ??
                node.status?.addresses?.[0]?.address ??
                "—";
              return (
                <tr key={node.metadata.name}>
                  <td>
                    <strong>{node.metadata.name}</strong>
                  </td>
                  <td>
                    <small>{zh ? categoryInfo.zh : categoryInfo.en}</small>
                  </td>
                  <td>
                    <StatusBadge
                      phase={(node.status?.phase ?? "Offline") as Phase}
                      copy={c}
                    />
                  </td>
                  <td>
                    <small>
                      {node.status?.lastHeartbeatTime
                        ? formatChinaDateTime(node.status.lastHeartbeatTime)
                        : "—"}
                    </small>
                  </td>
                  <td>
                    <small>
                      {node.status?.lastStatusChangeTime
                        ? formatChinaDateTime(node.status.lastStatusChangeTime)
                        : "—"}
                    </small>
                  </td>
                  <td>
                    <small>{getNodeLocation(node) || "—"}</small>
                  </td>
                  <td>
                    <code>{address}</code>
                  </td>
                  <td>
                    {resource.lines.length ? (
                      resource.lines.map((line) => (
                        <small key={line.key} className="cluster-resource-line">
                          {line.primary}
                        </small>
                      ))
                    ) : (
                      <small>{resource.primary}</small>
                    )}
                  </td>
                  <td>
                    {resource.lines.length ? (
                      resource.lines.map((line) => (
                        <small key={line.key} className="cluster-resource-line">
                          {line.secondary}
                        </small>
                      ))
                    ) : (
                      <small>—</small>
                    )}
                  </td>
                </tr>
              );
            })}
            {clusterNodes.length === 0 && (
              <tr>
                <td
                  colSpan={7}
                  style={{ textAlign: "center", padding: "16px" }}
                >
                  <small className="muted">
                    {zh ? "暂无节点" : "No nodes"}
                  </small>
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export function NodeDetailReal({
  node,
  copy: c,
  onTaskNavigate,
}: {
  node: CRDNode;
  copy: Copy;
  onTaskNavigate?: (name: string) => void;
}) {
  const zh = c.nav.overview === "总览";
  const [nodeWorkers, setNodeWorkers] = useState<NodeWorker[]>([]);
  useAutoRefresh(
    async () => {
      if (node.spec.agentType === "Raw") return;
      const [taskItems, podItems] = await Promise.all([
        tasksApi.list(),
        podsApi.list(),
      ]);
      const tasks = new Map<string, CRDTask>(
        taskItems.map((task) => [task.metadata?.name ?? "", task]),
      );
      const workers: NodeWorker[] = (podItems as Record<string, unknown>[])
        .filter(
          (pod: { status?: { node?: string } }) =>
            pod.status?.node === node.metadata.name,
        )
        .map(
          (pod: {
            metadata?: { name?: string; namespace?: string };
            spec?: { taskName?: string; podName?: string; domain?: string };
            status?: { phase?: string; ip?: string };
          }) => {
            const task = tasks.get(pod.spec?.taskName ?? "");
            const job = task?.metadata?.labels?.["rlinf.io/job"];
            if (!job) return null;
            return {
              id: `${pod.metadata?.namespace ?? ""}/${pod.metadata?.name ?? ""}`,
              crName: pod.metadata?.name ?? "",
              name: pod.spec?.podName || pod.metadata?.name || "—",
              job,
              role:
                task?.metadata?.annotations?.["rlark.io/ray-role"] ??
                task?.spec?.role ??
                "—",
              node: node.metadata.name,
              cluster: pod.metadata?.namespace ?? "default",
              domain: pod.spec?.domain ?? "",
              ip: pod.status?.ip ?? "—",
              phase: pod.status?.phase ?? "Pending",
              requests:
                task?.spec?.workload.template?.spec?.containers?.find(
                  (container) => container.name === "main",
                )?.resources?.requests ?? {},
            };
          },
        )
        .filter(
          (worker: NodeWorker | null): worker is NodeWorker => worker !== null,
        );
      setNodeWorkers(workers.sort((a, b) => a.name.localeCompare(b.name)));
    },
    10000,
    [node.metadata.name],
  );
  const requestedFallback = useMemo(() => {
    const totals: Record<string, number> = {};
    nodeWorkers.forEach((worker) =>
      Object.entries(worker.requests).forEach(([key, value]) =>
        addRequestedResource(totals, key, value),
      ),
    );
    return Object.fromEntries(
      Object.entries(totals).map(([key, value]) => [key, String(value)]),
    );
  }, [nodeWorkers]);
  return (
    <>
      <NodeOverview
        node={node}
        copy={c}
        requestedFallback={requestedFallback}
      />
      {node.spec.agentType !== "Raw" && (
        <section className="node-worker-panel">
          <div className="worker-panel-head">
            <div>
              <span className="eyebrow">
                {zh ? "运行实例" : "Runtime instances"}
              </span>
              <h3>{zh ? "Worker 列表" : "Worker list"}</h3>
            </div>
            <span className="worker-total-count">
              {nodeWorkers.length} {zh ? "个 Worker" : "workers"}
            </span>
          </div>
          <NodeWorkerTable
            workers={nodeWorkers}
            copy={c}
            onTaskNavigate={onTaskNavigate}
          />
        </section>
      )}
    </>
  );
}

function NodeWorkerTable({
  workers,
  copy: c,
  onTaskNavigate,
}: {
  workers: NodeWorker[];
  copy: Copy;
  onTaskNavigate?: (name: string) => void;
}) {
  const zh = c.nav.overview === "总览";
  const [expanded, setExpanded] = useState<string | null>(null);
  const [copied, setCopied] = useState<string | null>(null);
  const [sort, setSort] = useState<{
    key: "name" | "job" | "role" | "ip" | "requests" | "phase";
    direction: SortDirection;
  }>({ key: "name", direction: "asc" });
  const tableRef = useRef<HTMLDivElement>(null);
  const dragState = useRef({ active: false, x: 0, scrollLeft: 0 });
  const [dragging, setDragging] = useState(false);
  const [sshConfig, setSSHConfig] = useState<{
    sshJumpHost?: string;
    sshJumpPort?: string;
  }>({});
  useEffect(() => {
    systemConfigApi
      .get()
      .then((config) =>
        setSSHConfig({
          sshJumpHost: config.ssh?.jumpHost || config.sshJumpHost,
          sshJumpPort: config.ssh?.jumpPort || config.sshJumpPort,
        }),
      )
      .catch(() => setSSHConfig({}));
  }, []);
  const requestTextFor = useCallback(
    (worker: NodeWorker) =>
      Object.entries(worker.requests)
        .filter(
          ([key, value]) =>
            Number.parseFloat(String(value)) > 0 &&
            (/(^|[./-])gpu($|[./-])/i.test(key) ||
              key === "rlinf.io/device" ||
              key.startsWith("rlinf.io/device-")),
        )
        .map(([key, value]) => {
          if (/(^|[./-])gpu($|[./-])/i.test(key)) return `GPU ${value}`;
          const model = key.replace(/^rlinf\.io\/device-?/, "");
          return `${model || (zh ? "设备" : "Device")} ${value}`;
        })
        .filter(Boolean)
        .join(" · "),
    [zh],
  );
  const sortedWorkers = useMemo(
    () =>
      [...workers].sort((a, b) => {
        const value = (worker: NodeWorker) =>
          sort.key === "requests" ? requestTextFor(worker) : worker[sort.key];
        return compareSortValues(value(a), value(b), sort.direction);
      }),
    [workers, sort, requestTextFor],
  );
  const toggleSort = (key: typeof sort.key) =>
    setSort((current) => ({
      key,
      direction:
        current.key === key && current.direction === "asc" ? "desc" : "asc",
    }));
  const handlePointerDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0 || !tableRef.current) return;
    const target = event.target as HTMLElement;
    if (target.closest("button, a, [role='button']")) return;
    dragState.current = {
      active: true,
      x: event.clientX,
      scrollLeft: tableRef.current.scrollLeft,
    };
    event.currentTarget.setPointerCapture(event.pointerId);
    setDragging(true);
  };
  const handlePointerMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (!dragState.current.active || !tableRef.current) return;
    tableRef.current.scrollLeft =
      dragState.current.scrollLeft - (event.clientX - dragState.current.x);
  };
  const stopDragging = () => {
    dragState.current.active = false;
    setDragging(false);
  };
  if (workers.length === 0) {
    return (
      <div className="node-worker-empty">
        {zh ? "当前没有运行中的 Worker" : "No running workers"}
      </div>
    );
  }
  return (
    <div
      ref={tableRef}
      className={`node-worker-table-wrap worker-table worker-console-table worker-table-scroll${dragging ? " dragging" : ""}`}
      onPointerDown={handlePointerDown}
      onPointerMove={handlePointerMove}
      onPointerUp={stopDragging}
      onPointerCancel={stopDragging}
    >
      <table className="node-worker-table">
        <thead>
          <tr>
            {(
              [
                ["name", zh ? "Worker 名称" : "Worker"],
                ["job", "Job"],
                ["role", zh ? "角色" : "Role"],
                ["ip", "IP"],
                ["requests", zh ? "GPU / 端设备" : "GPU / edge devices"],
                ["phase", zh ? "状态" : "Status"],
              ] as const
            ).map(([key, label]) => (
              <th key={key}>
                <SortButton
                  label={label}
                  active={sort.key === key}
                  direction={sort.direction}
                  onClick={() => toggleSort(key)}
                />
              </th>
            ))}
            <th>{zh ? "操作" : "Actions"}</th>
          </tr>
        </thead>
        <tbody>
          {sortedWorkers.map((worker) => {
            const requestText = requestTextFor(worker);
            const sshJump = sshConfig.sshJumpHost
              ? `${sshConfig.sshJumpHost}${sshConfig.sshJumpPort ? `:${sshConfig.sshJumpPort}` : ""}`
              : "";
            const sshCommand = sshJump
              ? `ssh -J ${sshJump} root@${worker.name} -p 2222`
              : "";
            const isExpanded = expanded === worker.id;
            return (
              <Fragment key={worker.id}>
                <tr>
                  <td>
                    <strong className="node-worker-name" title={worker.name}>
                      {worker.name}
                    </strong>
                  </td>
                  <td>
                    <button
                      className="plain-button node-worker-job-link"
                      disabled={worker.job === "—" || !onTaskNavigate}
                      onClick={() => onTaskNavigate?.(worker.job)}
                    >
                      {worker.job}
                    </button>
                  </td>
                  <td>{worker.role}</td>
                  <td>
                    <code>{worker.ip}</code>
                  </td>
                  <td>{requestText || (zh ? "未使用" : "Not used")}</td>
                  <td>
                    <StatusBadge phase={worker.phase as Phase} copy={c} />
                  </td>
                  <td>
                    <div className="worker-table-actions">
                      <button
                        className="icon-button worker-copy-ssh-icon"
                        disabled={!sshCommand}
                        title={zh ? "复制 SSH 命令" : "Copy SSH command"}
                        onClick={async () => {
                          if (!sshCommand) return;
                          if (!(await copyText(sshCommand))) return;
                          setCopied(worker.id);
                          window.setTimeout(() => setCopied(null), 1500);
                        }}
                      >
                        {copied === worker.id ? (
                          <Check size={15} />
                        ) : (
                          <KeyRound size={15} />
                        )}
                      </button>
                      <button
                        className="icon-button worker-terminal-icon"
                        disabled={!worker.crName}
                        title={zh ? "打开 WebTerminal" : "Open WebTerminal"}
                        onClick={() => {
                          if (!worker.crName) return;
                          const params = new URLSearchParams({
                            job: worker.job,
                            worker: worker.crName,
                            status: worker.phase,
                          });
                          const terminalWindow = window.open(
                            `/terminal?${params.toString()}`,
                            "_blank",
                          );
                          if (terminalWindow) terminalWindow.opener = null;
                        }}
                      >
                        <TerminalSquare size={15} />
                      </button>
                      <button
                        className="icon-button"
                        title={zh ? "查看详情" : "View details"}
                        onClick={() =>
                          setExpanded(isExpanded ? null : worker.id)
                        }
                      >
                        <ChevronRight
                          size={15}
                          style={{
                            transform: isExpanded ? "rotate(90deg)" : "none",
                          }}
                        />
                      </button>
                    </div>
                  </td>
                </tr>
                {isExpanded && (
                  <tr className="worker-expanded-row node-worker-expanded">
                    <td colSpan={7}>
                      <div className="worker-detail-drawer">
                        <div className="worker-detail-head">
                          <div>
                            <span className="eyebrow">
                              {zh ? "Worker 详情" : "Worker details"}
                            </span>
                            <strong>{worker.name}</strong>
                          </div>
                          {sshCommand && (
                            <div className="worker-ssh-inline">
                              <KeyRound size={15} />
                              <code title={sshCommand}>{sshCommand}</code>
                              <button
                                className="icon-button"
                                onClick={async () => {
                                  if (!(await copyText(sshCommand))) return;
                                  setCopied(worker.id);
                                  window.setTimeout(
                                    () => setCopied(null),
                                    1500,
                                  );
                                }}
                                aria-label={
                                  zh ? "复制 SSH 地址" : "Copy SSH address"
                                }
                              >
                                {copied === worker.id ? (
                                  <Check size={15} />
                                ) : (
                                  <CopyIcon size={15} />
                                )}
                              </button>
                            </div>
                          )}
                        </div>
                        <div className="worker-detail-grid">
                          <div>
                            <span>{zh ? "集群" : "Cluster"}</span>
                            <strong>{worker.cluster}</strong>
                          </div>
                          <div>
                            <span>{zh ? "角色" : "Role"}</span>
                            <strong>{worker.role}</strong>
                          </div>
                          <div>
                            <span>{zh ? "节点" : "Node"}</span>
                            <strong>{worker.node}</strong>
                          </div>
                          <div>
                            <span>{zh ? "申请 GPU" : "GPU request"}</span>
                            <strong>
                              {requestText || (zh ? "未使用" : "Not used")}
                            </strong>
                          </div>
                        </div>
                        <div className="pod-subtable">
                          <table>
                            <thead>
                              <tr>
                                <th>{zh ? "实例名称" : "Worker name"}</th>
                                <th>Job</th>
                                <th>{zh ? "实例 IP" : "Worker IP"}</th>
                                <th>{zh ? "网络域" : "Domain"}</th>
                                <th>{zh ? "状态" : "Status"}</th>
                              </tr>
                            </thead>
                            <tbody>
                              <tr>
                                <td>
                                  <code className="inline-code">
                                    {worker.name}
                                  </code>
                                </td>
                                <td>{worker.job}</td>
                                <td>{worker.ip}</td>
                                <td>
                                  {worker.domain ? (
                                    <span
                                      className="pod-subtable-truncated"
                                      title={worker.domain}
                                    >
                                      <Network size={13} />
                                      <span>{worker.domain}</span>
                                    </span>
                                  ) : (
                                    "—"
                                  )}
                                </td>
                                <td>
                                  <StatusBadge
                                    phase={worker.phase as Phase}
                                    copy={c}
                                  />
                                </td>
                              </tr>
                            </tbody>
                          </table>
                        </div>
                      </div>
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
import { nodesApi, podsApi, systemConfigApi, tasksApi } from "../backend";
