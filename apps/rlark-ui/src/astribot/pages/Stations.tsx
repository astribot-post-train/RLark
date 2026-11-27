import { Fragment, useCallback, useEffect, useMemo, useRef, useState, type ComponentProps, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { AlertTriangle, Boxes, Check, ChevronDown, KeyRound, ListFilter, Server } from "lucide-react";
import { ApiError } from "../../api";
import { clustersApi, nodesApi } from "../../backend";
import { machinesApi, nodeAdmissionsApi, selfHealApi, siteNodesApi, stationsApi, type NodeOperation, type StationItem, type StationSummary } from "../api";
import { stationDeletable, stationHasNodeUIDConflict, stationsOfPhase } from "../lib/nodeRemoval";
import { PageToolbar, RefreshOverlay } from "../../components/shared";
import { useAutoRefresh } from "../../hooks";
import type { Copy } from "../../i18n";
import type { ClusterSummary, CRDNode } from "../../types";
import { RobotControls } from "../components/RobotControls";
import { NodeName } from "../components/NodeName";
import { useNodeRestart } from "../components/useNodeRestart";
import { useRobotPower } from "../components/useRobotPower";
import { outstandingRestart, readNodeRestart, restartPhaseLabel } from "../lib/nodeRestart";
import { nodeLabel } from "../lib/displayName";
import { clusterForStation, stationPhase, stationRecoveryHint } from "../lib/stationDisplay";
import { type NodeJoinTarget } from "../lib/nodeJoin";
import { assessRobotRejoin } from "../lib/robotRejoin";
import { clearRejoinObservations, isRejoinTerminal, isRobotRejoinOperation, machinesForRejoin, newRejoinRequestID, pickRejoinOperation, publishRejoinObservation, readOperationPages, readRejoinTracks, rejoinBusy, rejoinCoarseKey, rejoinObservation, robotModelForRejoin, writeRejoinTrack, type RobotRejoinCardState, type RobotRejoinTrack } from "../lib/robotRejoinTask";
import { isControlPlaneLabels } from "../../utils/nodeVisibility";
import { admissionStateLabel, isRobotNode, machineModel, robotPowerAfter, robotPowerDesired, type IssuedNodeAdmission, type NodeAdmission, type RobotPowerAction, type SiteNode } from "../lib/siteNodeDisplay";
import { cameraCells, publishedCameras, type CameraCell, type CameraFps, type CameraView } from "../lib/cameras";
import { BatchProgressPanel } from "../components/BatchProgressPanel";
import { computeFleetSummary, defaultViewModeForCount, extractStationMeta, filterAndSortFleetItems, selectFleetSummary, type FleetOccupancyFilter, type FleetPowerFilter, type FleetSortKey, type FleetStation, type FleetViewMode } from "../lib/stationFleet";
import { CameraWallView, FleetPagination } from "./CameraWallView";
import { StationsTableView } from "./StationsTableView";
import { CameraPreviewDialog } from "./CameraPreviewDialog";
import { StationCard, type StationCardVariant } from "./StationCard";
import { StationDetailDialog } from "./StationDetailDialog";
import { StationDetailPanel } from "./StationDetailPanel";
import { releasePoseSessions } from "../components/RobotControls";
import { AdmissionRecords, NodeOperations } from "./StationRecords";
import { stationFacts } from "../lib/stationDrawer";
import { shortUID, stationPhaseLabel, stationStatusClass } from "../lib/stationDisplay";
import "./Stations.css";
import "./StationsCards.css";
import "../components/controlPlaneBadge.css";

const VIEW_STORAGE_KEY = "rlark.stations.view";
type SummaryFilter = "" | "alerts" | "driver" | "camera" | "occupied" | "offline";

interface FleetGroup {
  id: string;
  name: string;
  items: FleetStation[];
  online: number;
  alerts: number;
}

function savedViewMode(): FleetViewMode | null {
  try {
    const mode = localStorage.getItem(VIEW_STORAGE_KEY);
    return mode === "table" || mode === "cards" || mode === "wall" ? mode : null;
  } catch {
    return null;
  }
}

function indexNodes(nodes: CRDNode[]) {
  return Object.fromEntries(nodes.map((node) => [`${node.metadata.namespace}/${node.metadata.name}`, node]));
}

function keyOf(item: StationItem) {
  return `${item.metadata.namespace}/${item.metadata.name}`;
}

// How often a non-terminal rejoin is read, and how many misses mean the NodeOperation never appeared.
const REJOIN_POLL_MS = 2000;
const REJOIN_MISS_LIMIT = 10;

function rejoinApiCode(reason: unknown): string {
  if (!(reason instanceof ApiError) || !reason.body || typeof reason.body !== "object" || !("code" in reason.body)) return "";
  const code = reason.body.code;
  return typeof code === "string" ? code : "";
}

// A plain grid of tiles or rows: the open detail is a dialog, so nothing is inserted between the grid children
// and no measured row grouping is needed any more.
function CardGrid({ items, className, renderCard }: {
  items: StationItem[];
  className: string;
  renderCard: (item: StationItem) => ReactNode;
}) {
  return <div className={`stations-card-grid ${className}`}>
    {items.map((item) => <Fragment key={keyOf(item)}>{renderCard(item)}</Fragment>)}
  </div>;
}

export function StationsPage({ copy: c, onJoin, onModels }: { copy: Copy; onJoin: (reissued?: IssuedNodeAdmission, sshJoin?: NodeJoinTarget) => void; onModels?: () => void }) {
  const zh = c.nav.overview === "总览";
  const [items, setItems] = useState<StationItem[]>([]);
  const [clusters, setClusters] = useState<ClusterSummary[]>([]);
  // The Nodes by "<namespace>/<name>", for the display names of the site nodes.
  const [nodesByKey, setNodesByKey] = useState<Record<string, CRDNode>>({});
  const [clusterID, setClusterID] = useState("");
  const [phase, setPhase] = useState("");
  const [search, setSearch] = useState("");
  const [serverSummary, setServerSummary] = useState<StationSummary>();
  const [summaryFilter, setSummaryFilter] = useState<SummaryFilter>("");
  const [alertOnly, setAlertOnly] = useState(false);
  const [model, setModel] = useState("");
  const [zone, setZone] = useState("");
  const [powerFilter, setPowerFilter] = useState<FleetPowerFilter>("all");
  const [occupancy, setOccupancy] = useState<FleetOccupancyFilter>("all");
  const [sort, setSort] = useState<FleetSortKey>("alert");
  const [savedView, setSavedView] = useState<FleetViewMode | null>(savedViewMode);
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const [cardPage, setCardPage] = useState(1);
  const [cardPageSize, setCardPageSize] = useState(12);
  const [cardLive, setCardLive] = useState(true);
  const [wallKeys, setWallKeys] = useState<Set<string>>(new Set());
  const [showAdmissions, setShowAdmissions] = useState(false);
  const [admissionsLoading, setAdmissionsLoading] = useState(false);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  // Per "<clusterID>/<nodeName>": the Gateway's site-node view; and the admissions of the clusters shown.
  const [siteNodes, setSiteNodes] = useState<Record<string, SiteNode>>({});
  const [admissions, setAdmissions] = useState<NodeAdmission[]>([]);
  const [admissionError, setAdmissionError] = useState("");
  const [siteDataState, setSiteDataState] = useState<"loading" | "ready" | "unavailable">("loading");
  const [showNodeOperations, setShowNodeOperations] = useState(false);
  const [deletePending, setDeletePending] = useState("");
  const [admissionPending, setAdmissionPending] = useState<{ key: string; kind: "reissue" | "revoke" }>();
  const [actionError, setActionError] = useState<{ key: string; message: string }>();
  // The station whose recovery re-check is loading; re-check runs the page's normal load instead of a second reader.
  const [recheckKey, setRecheckKey] = useState("");
  const [chosen, setChosen] = useState<Set<string>>(new Set());
  // One station is open at a time; its detail spans the row below its card. Every station starts closed.
  const [openKey, setOpenKey] = useState("");
  const [zoom, setZoom] = useState<{ item: StationItem; view: CameraView; cell?: CameraCell; trigger: HTMLElement; fps?: CameraFps }>();
  const [now, setNow] = useState(() => Date.now());
  // The dialog keeps showing the last known record for its key even when a filter would hide the row.
  const detailItem = openKey ? items.find((item) => keyOf(item) === openKey) : undefined;
  const detailsID = detailItem ? `station-details-${detailItem.metadata.namespace}-${detailItem.metadata.name}` : "";
  // The power toast portals into the top-most dialog's host so the fixed page toast is not hidden behind the
  // native top layer; with no dialog open it stays the page's own fixed corner toast.
  const [toastHost, setToastHost] = useState<HTMLElement | null>(null);
  const activeRequest = useRef<AbortController | null>(null);

  const load = async () => {
    setLoading(true);
    setError("");
    activeRequest.current?.abort();
    const controller = new AbortController();
    const { signal } = controller;
    activeRequest.current = controller;

    const loadStations = async () => {
      const result: StationItem[] = [];
      const tokens = new Set<string>();
      let next: string | undefined;
      do {
        const response = await stationsApi.list({ cluster_id: clusterID || undefined, limit: "200", continue: next }, signal);
        result.push(...response.items);
        next = response.continue || undefined;
        if (next && tokens.has(next)) throw new Error(zh ? "场站分页游标重复" : "Repeated station page cursor");
        if (next) tokens.add(next);
      } while (next);
      return result;
    };

    void stationsApi.summary(clusterID || undefined, signal)
      .then((result) => { if (!signal.aborted) setServerSummary(result); })
      .catch(() => { if (!signal.aborted) setServerSummary(undefined); });

    try {
      const [records, latestClusters, nodes] = await Promise.all([
        loadStations(), clustersApi.list<ClusterSummary>(), nodesApi.list(),
      ]);
      if (signal.aborted) return;
      setClusters(latestClusters);
      setNodesByKey(indexNodes(nodes));
      setItems(records);
      void loadSiteData(latestClusters, signal);
    } catch (reason) {
      if (!signal.aborted) setError((reason as Error).message);
    } finally {
      if (!signal.aborted) {
        setLoading(false);
        setRefreshing(false);
      }
    }
  };

  const loadSiteData = async (latestClusters: ClusterSummary[], signal: AbortSignal) => {
    setSiteDataState("loading");
    const targets = latestClusters.filter((cluster) => (!clusterID || cluster.id === clusterID) && !cluster.agentNamespaceConflict);
    const results = await Promise.allSettled(targets.map((cluster) => siteNodesApi.list(cluster.id, signal)));
    if (signal.aborted) return;
    const nextNodes: Record<string, SiteNode> = {};
    for (const result of results) {
      if (result.status === "fulfilled") {
        for (const node of result.value.items) nextNodes[`${node.cluster_id}/${node.node_name}`] = node;
      }
    }
    setSiteNodes(nextNodes);
    setSiteDataState(results.some((result) => result.status === "rejected") ? "unavailable" : "ready");
  };

  const detailClusterID = detailItem && clusterForStation(clusters, detailItem.metadata.namespace)?.id;
  const admissionsScope = showAdmissions ? "*" : detailClusterID || "";
  useEffect(() => {
    if (!admissionsScope) {
      setAdmissionsLoading(false);
      return;
    }
    const controller = new AbortController();
    const { signal } = controller;
    const targets = clusters.filter((cluster) => !cluster.agentNamespaceConflict &&
      (admissionsScope === "*" ? !clusterID || cluster.id === clusterID : cluster.id === admissionsScope));
    setAdmissionsLoading(true);
    setAdmissionError("");
    void Promise.allSettled(targets.map((cluster) => nodeAdmissionsApi.list(cluster.id, signal))).then((results) => {
      if (signal.aborted) return;
      const fresh: NodeAdmission[] = [];
      let failure = "";
      const refreshed = new Set<string>();
      results.forEach((result, index) => {
        if (result.status === "fulfilled") {
          refreshed.add(targets[index].id);
          fresh.push(...result.value.items);
        } else failure = (result.reason as Error).message;
      });
      setAdmissions((current) => [...current.filter((entry) => !refreshed.has(entry.cluster_id)), ...fresh]);
      setAdmissionError(failure);
      setAdmissionsLoading(false);
    });
    return () => controller.abort();
  }, [admissionsScope, clusterID, clusters]);

  useEffect(() => {
    setServerSummary(undefined);
    void load();
    return () => activeRequest.current?.abort();
    // Cluster changes replace the snapshot.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [clusterID]);
  useAutoRefresh(() => load(), 30000);
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 15000);
    return () => window.clearInterval(timer);
  }, []);
  useEffect(() => setNow(Date.now()), [items]);
  // While the camera zoom dialog is open it owns Escape: the detail dialog must not react to the same key.
  useEffect(() => {
    const detail = document.querySelector<HTMLDialogElement>(".station-detail-dialog");
    if (!detail) return;
    detail.closedBy = zoom ? "none" : "closerequest";
  }, [zoom, openKey]);
  useEffect(() => {
    if (!openKey && !zoom) {
      setToastHost(null);
      return;
    }
    // Two frames: the dialog mounts and calls showModal, then the top layer is live and the host is queryable.
    let raf2 = 0;
    const raf1 = window.requestAnimationFrame(() => {
      raf2 = window.requestAnimationFrame(() => {
        setToastHost(document.querySelector(".station-detail-dialog[open] > .station-toast-host, .station-camera-dialog[open] .station-toast-host"));
      });
    });
    return () => {
      window.cancelAnimationFrame(raf1);
      window.cancelAnimationFrame(raf2);
    };
  }, [openKey, zoom]);

  const nodeAt = (namespace: string, name: string) => nodesByKey[`${namespace}/${name}`] ?? { metadata: { name } };
  const nodeOf = (item: StationItem) => nodeAt(item.metadata.namespace, item.metadata.name);
  const siteNodeOf = (item: StationItem) => siteNodes[`${clusterForStation(clusters, item.metadata.namespace)?.id}/${item.metadata.name}`];
  const admissionsByKey = useMemo(() => new Map(admissions.map((entry) => [`${entry.cluster_id}/${entry.node_name}`, entry])), [admissions]);
  const admissionOf = (item: StationItem) => admissionsByKey.get(`${clusterForStation(clusters, item.metadata.namespace)?.id}/${item.metadata.name}`);
  // Prefer the management Node for live power labels. The site view supplies identity and camera metadata.
  const fleet = useMemo(() => items.map((item): FleetStation => {
    const cluster = clusterForStation(clusters, item.metadata.namespace);
    const site = siteNodes[`${cluster?.id}/${item.metadata.name}`];
    const admission = admissionsByKey.get(`${cluster?.id}/${item.metadata.name}`);
    const node = nodesByKey[keyOf(item)];
    return {
      ...item, clusterName: cluster?.name, clusterID: cluster?.id,
      labels: { ...admission?.labels, ...site?.labels, ...node?.metadata.labels },
      annotations: node?.metadata.annotations, site,
    };
  }), [items, clusters, siteNodes, admissionsByKey, nodesByKey]);
  const fleetByKey = useMemo(() => new Map(fleet.map((item) => [keyOf(item), item])), [fleet]);
  const metaByKey = useMemo(() => new Map(fleet.map((item) => [keyOf(item), extractStationMeta(item)])), [fleet]);
  const labelsOf = (item: StationItem) => fleetByKey.get(keyOf(item))?.labels;
  const isRobotItem = (item: StationItem) => isRobotNode(labelsOf(item)) && stationPhase(item) !== "Retired";
  // The restart records a Node carries: its own labels and annotations, as the page's node list has them. Keeping an
  // older request visible is what stops a second, untracked request being queued over it.
  const restartObservationOf = (item: StationItem) => {
    const node = nodesByKey[`${item.metadata.namespace}/${item.metadata.name}`];
    return readNodeRestart(node?.metadata?.labels, node?.metadata?.annotations);
  };

  const localSummary = useMemo(() => computeFleetSummary(fleet, clusters), [fleet, clusters]);
  const metrics = selectFleetSummary(serverSummary, localSummary);
  const robotCount = metrics.robots;
  const [initialView, setInitialView] = useState<FleetViewMode>();
  const view = savedView ?? initialView ?? defaultViewModeForCount(robotCount, null);
  useEffect(() => {
    // Site-node labels may identify robots that are missing from an older Gateway's enrollment summary.
    if (!loading && siteDataState !== "loading" && !initialView) {
      setInitialView(defaultViewModeForCount(robotCount, null));
    }
  }, [loading, siteDataState, initialView, robotCount]);
  const changeView = (mode: FleetViewMode) => {
    setSavedView(mode);
    try { localStorage.setItem(VIEW_STORAGE_KEY, mode); } catch { /* The current view is still available. */ }
  };
  const visible = useMemo(() => {
    // Keep the existing visible-name and system UUID search alongside the fleet filters.
    const query = search.trim().toLocaleLowerCase();
    const candidates = !query ? fleet : fleet.filter((item) => {
      const node = nodesByKey[keyOf(item)];
      const meta = metaByKey.get(keyOf(item))!;
      return [item.metadata.name, item.metadata.namespace, item.clusterName, item.spec.nodeUID,
        item.status?.observation?.systemUUID, node && nodeLabel(node), meta.model, meta.zone]
        .some((value) => value?.toLocaleLowerCase().includes(query));
    });
    return filterAndSortFleetItems(candidates, { cluster: clusterID, model, zone, power: powerFilter, occupancy, alertOnly, sort }, clusters)
      .filter((item) => {
        const itemPhase = stationPhase(item);
        if (phase && itemPhase !== phase) return false;
        const meta = metaByKey.get(keyOf(item))!;
        switch (summaryFilter) {
          case "alerts": return meta.alertSeverity > 0;
          case "driver": return meta.driverReady;
          case "camera": return meta.cameraFault;
          case "occupied": return meta.occupied;
          case "offline": return itemPhase === "Offline";
          default: return true;
        }
      });
  }, [fleet, nodesByKey, metaByKey, search, phase, clusterID, model, zone, powerFilter, occupancy, alertOnly, sort, clusters, summaryFilter]);
  const models = [...new Set([...metaByKey.values()].map((meta) => meta.model).filter(Boolean))].sort();
  const zones = [...new Set([...metaByKey.values()].map((meta) => meta.zone).filter(Boolean))].sort();
  const groupOf = (item: FleetStation) => item.clusterID ?? item.metadata.namespace;
  const groups = new Map<string, FleetGroup>();
  for (const item of visible) {
    const id = groupOf(item);
    const group = groups.get(id) ?? { id, name: item.clusterName ?? item.metadata.namespace, items: [], online: 0, alerts: 0 };
    group.items.push(item);
    if (stationPhase(item) === "Bound") group.online++;
    if (metaByKey.get(keyOf(item))!.alertSeverity) group.alerts++;
    groups.set(id, group);
  }
  const unfolded = visible.filter((item) => !collapsed.has(groupOf(item)));
  const cardPages = Math.max(1, Math.ceil(unfolded.length / cardPageSize));
  const currentCardPage = Math.min(cardPage, cardPages);
  const cardKeys = new Set(unfolded.slice((currentCardPage - 1) * cardPageSize, currentCardPage * cardPageSize).map(keyOf));
  useEffect(() => setCardPage(1), [phase, search, model, zone, powerFilter, occupancy, alertOnly, sort, clusterID, summaryFilter]);

  const selectionKey = (item: StationItem) => `${keyOf(item)}/${item.spec.nodeUID}`;
  const choose = useCallback((item: StationItem, checked: boolean) => {
    setChosen((current) => {
      const next = new Set(current);
      const key = `${keyOf(item)}/${item.spec.nodeUID}`;
      if (checked) next.add(key);
      else next.delete(key);
      return next;
    });
  }, []);
  const visibleRobots = visible.filter(isRobotItem);
  const chosenItems = visibleRobots.filter((item) => chosen.has(selectionKey(item)));
  const entries = visibleRobots.map((item) => ({ key: keyOf(item), node: item.metadata.name, namespace: item.metadata.namespace, labels: item.labels }));
  const selectedKeys = new Set(chosenItems.map(keyOf));
  const chosenEntries = entries.filter((entry) => selectedKeys.has(entry.key));
  const recordAdmissions = clusterID ? admissions.filter((entry) => entry.cluster_id === clusterID) : admissions;

  const focusExpander = (item: StationItem) => window.requestAnimationFrame(() => document.getElementById(`station-expand-${item.metadata.namespace}-${item.metadata.name}`)?.focus());
  const toggleOpen = (item: StationItem) => {
    const key = keyOf(item);
    if (openKey === key) {
      setOpenKey("");
      focusExpander(item);
    } else {
      setOpenKey(key);
    }
  };
  const closePanel = (item: StationItem) => {
    setOpenKey("");
    focusExpander(item);
  };

  const openZoom = (item: StationItem, view: CameraView, trigger: HTMLElement, fps?: CameraFps) => {
    const cells = cameraCells(publishedCameras(siteNodeOf(item)?.cameras ?? fleetByKey.get(keyOf(item))?.annotations?.["rlark.io/cameras"]), zh, siteDataState !== "unavailable");
    setZoom({ item, view, cell: cells.find((cell) => cell.view === view), trigger, fps });
  };
  const closeZoom = () => {
    const trigger = zoom?.trigger;
    setZoom(undefined);
    window.requestAnimationFrame(() => trigger?.focus());
  };

  const removeRecord = async (item: StationItem) => {
    const itemCluster = clusterForStation(clusters, item.metadata.namespace);
    const key = keyOf(item);
    const hasNode = `${item.metadata.namespace}/${item.metadata.name}` in nodesByKey;
    const uidConflict = stationHasNodeUIDConflict(item);
    if (deletePending || !itemCluster || !stationDeletable(item, hasNode) || !window.confirm(uidConflict ? (zh ? `清除 ${item.metadata.name} 的旧场站登记？同名新节点会保留。` : `Clear the old enrollment for ${item.metadata.name}? The replacement Node will be preserved.`) : zh
      ? `删除场站节点记录 ${item.metadata.name}（节点 UID ${item.spec.nodeUID.slice(0, 8)}）？删除后不能恢复；这个名字的节点如果还在上报，会按新机器重新登记。`
      : `Delete the site node record ${item.metadata.name} (Node UID ${item.spec.nodeUID.slice(0, 8)})? It cannot be brought back; a node of the name that still reports registers again as a new machine.`)) return;
    setDeletePending(key);
    setError("");
    try {
      if (uidConflict && item.metadata.uid) {
        await selfHealApi.forgetConflictingStation(item.metadata.name, itemCluster.id, item.metadata.uid);
      } else if (!uidConflict) {
        await selfHealApi.deleteStation(item.metadata.name, itemCluster.id);
      }
      await load();
    } catch (reason) {
      setError((reason as Error).message);
    } finally {
      setDeletePending("");
    }
  };

  const actAdmission = async (item: StationItem, kind: "reissue" | "revoke") => {
    const itemCluster = clusterForStation(clusters, item.metadata.namespace);
    const key = keyOf(item);
    if (!itemCluster || admissionPending || deletePending === key) return;
    if (kind === "revoke" && !window.confirm(zh ? `作废 ${item.metadata.name} 的入网码？` : `Revoke the join code of ${item.metadata.name}?`)) return;
    setAdmissionPending({ key, kind });
    setActionError(undefined);
    try {
      if (kind === "reissue") onJoin(await nodeAdmissionsApi.reissue(item.metadata.name, itemCluster.id));
      else await nodeAdmissionsApi.revoke(item.metadata.name, itemCluster.id);
      await load();
    } catch (reason) {
      setActionError({ key, message: (reason as Error).message });
    } finally {
      setAdmissionPending(undefined);
    }
  };

  // Small batches read their affected Nodes by name. Larger batches refresh the Node list once per observation round.
  // Both update the cache that supplies the tiles, detail views, and fleet counts.
  const observeRobotPower = async (targets: { key: string; node: string; namespace: string }[]) => {
    const nodes: Record<string, CRDNode> = {};
    await Promise.all(targets.map(async (target) => {
      try {
        const node = await nodesApi.get(target.node, { namespace: target.namespace });
        nodes[target.key] = node;
      } catch {
        // Unreadable now; the bounded observation tries again on its next tick.
      }
    }));
    setNodesByKey((current) => ({ ...current, ...nodes }));
    return nodes;
  };
  const refreshRobotPowerNodes = async () => {
    const nodes = indexNodes(await nodesApi.list());
    setNodesByKey(nodes);
    return nodes;
  };
  const power = useRobotPower(zh, load, { observe: observeRobotPower }, { onCoalescedRefresh: refreshRobotPowerNodes });
  const restart = useNodeRestart(zh, () => load());
  // A notice is why a click did not submit. A task records the operation name for this cluster and node.
  // Stage text is published to the card's own subscription; this state changes only when the task, the phase
  // bucket, or the error changes, so a 2s poll does not re-render the page on an unchanged read.
  const [rejoinNotice, setRejoinNotice] = useState<Record<string, string>>({});
  const [rejoinTasks, setRejoinTasks] = useState<Record<string, RobotRejoinCardState>>({});
  const rejoinTasksRef = useRef(rejoinTasks);
  rejoinTasksRef.current = rejoinTasks;
  const zhRef = useRef(zh);
  zhRef.current = zh;
  const rejoinMisses = useRef<Record<string, number>>({});
  const rejoinStop = useRef<AbortController | null>(null);
  const rejoinActiveKey = Object.entries(rejoinTasks)
    .filter(([, task]) => Boolean(task.track) && !(task.confirmed && isRejoinTerminal(task.operation?.status?.phase)))
    .map(([key, task]) => `${key}:${task.track?.operationName}`)
    .sort()
    .join("|");
  const commitRejoin = (key: string, task: RobotRejoinCardState | undefined) => {
    if (rejoinStop.current?.signal.aborted) return;
    publishRejoinObservation(key, task ? rejoinObservation(task, zhRef.current) : undefined);
    if (task && rejoinCoarseKey(rejoinTasksRef.current[key]) === rejoinCoarseKey(task)) return;
    const next = { ...rejoinTasksRef.current };
    if (task) next[key] = task;
    else delete next[key];
    rejoinTasksRef.current = next;
    setRejoinTasks(next);
  };
  useEffect(() => {
    const controller = new AbortController();
    rejoinStop.current = controller;
    return () => {
      controller.abort();
      clearRejoinObservations();
    };
  }, []);
  useEffect(() => {
    for (const [key, task] of Object.entries(rejoinTasksRef.current)) publishRejoinObservation(key, rejoinObservation(task, zh));
  }, [zh]);
  // Discover again whenever the station snapshot settles. The cleanup aborts the reads, so a Strict Mode remount
  // or a newer snapshot does not apply a stale page. A key that is already submitting or tracking a task is left alone.
  useEffect(() => {
    if (siteDataState === "loading" || items.length === 0 || clusters.length === 0) return;
    const controller = new AbortController();
    const discover = async () => {
      const stored = readRejoinTracks(window.sessionStorage);
      const clusterIDs = new Set<string>();
      const targets: { key: string; clusterID: string; nodeName: string }[] = [];
      for (const item of items) {
        if (!isRobotItem(item) || stationPhase(item) === "Bound" || stationPhase(item) === "Retired") continue;
        const cluster = clusterForStation(clusters, item.metadata.namespace);
        if (!cluster) continue;
        targets.push({ key: keyOf(item), clusterID: cluster.id, nodeName: item.metadata.name });
        clusterIDs.add(cluster.id);
      }
      for (const track of stored) clusterIDs.add(track.clusterID);
      const byCluster = new Map<string, NodeOperation[]>();
      await Promise.all([...clusterIDs].map(async (clusterID) => {
        try {
          byCluster.set(clusterID, await readOperationPages((pageToken) => stationsApi.listNodeOperations(clusterID, controller.signal, pageToken)));
        } catch {
          byCluster.set(clusterID, []);
        }
      }));
      await Promise.all(stored.map(async (track) => {
        const known = (byCluster.get(track.clusterID) ?? []).some((operation) => operation.metadata.name === track.operationName);
        if (known) return;
        try {
          const operation = await stationsApi.getNodeOperation(track.operationName, track.clusterID, controller.signal);
          if (!isRobotRejoinOperation(operation, track.clusterID, track.nodeName) || operation.metadata.name !== track.operationName) return;
          byCluster.set(track.clusterID, [...(byCluster.get(track.clusterID) ?? []), operation]);
        } catch {
          // The list is enough when the named task cannot be read.
        }
      }));
      if (controller.signal.aborted) return;
      const seenAt = Date.now();
      for (const target of targets) {
        const existing = rejoinTasksRef.current[target.key];
        if (existing?.submitting || existing?.error || existing?.track || existing?.operation) continue;
        const track = stored.find((item) => item.clusterID === target.clusterID && item.nodeName === target.nodeName);
        const operation = pickRejoinOperation(byCluster.get(target.clusterID) ?? [], target.clusterID, target.nodeName, track?.operationName, seenAt, Boolean(track));
        if (!operation) continue;
        const adopted: RobotRejoinTrack = track && track.operationName === operation.metadata.name ? track : {
          clusterID: target.clusterID,
          nodeName: target.nodeName,
          machineName: track?.machineName ?? "",
          requestID: operation.spec.requestID ?? "",
          operationName: operation.metadata.name,
        };
        writeRejoinTrack(window.sessionStorage, adopted);
        commitRejoin(target.key, { track: adopted, operation, confirmed: true });
      }
    };
    void discover();
    return () => controller.abort();
    // The latest settled snapshot is what a refresh or a return to the page restores from.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [siteDataState, items, clusters]);
  useEffect(() => {
    if (!rejoinActiveKey) return;
    const controller = new AbortController();
    let running = false;
    const tick = async () => {
      if (running || controller.signal.aborted) return;
      running = true;
      try {
        const active = Object.entries(rejoinTasksRef.current).filter(([, task]) => task.track && !(task.confirmed && isRejoinTerminal(task.operation?.status?.phase)));
        await Promise.all(active.map(async ([key, task]) => {
          const track = task.track;
          if (!track) return;
          try {
            const operation = await stationsApi.getNodeOperation(track.operationName, track.clusterID, controller.signal);
            if (controller.signal.aborted || !isRobotRejoinOperation(operation, track.clusterID, track.nodeName) || operation.metadata.name !== track.operationName) return;
            rejoinMisses.current[key] = 0;
            commitRejoin(key, { track, operation, confirmed: true });
          } catch (reason) {
            if (controller.signal.aborted || !(reason instanceof ApiError) || reason.status !== 404) return;
            const count = (rejoinMisses.current[key] ?? 0) + 1;
            rejoinMisses.current[key] = count;
            if (count !== REJOIN_MISS_LIMIT) return;
            const existing = rejoinTasksRef.current[key];
            if (!existing || existing.track?.operationName !== track.operationName || existing.confirmed) return;
            const seconds = REJOIN_MISS_LIMIT * REJOIN_POLL_MS / 1000;
            const error = zhRef.current
              ? `重接已提交，但 ${seconds} 秒内没有读到 NodeOperation ${track.operationName}。没有把它显示成进行中的阶段。可再试一次，或使用手动重新接入。`
              : `Rejoin was submitted, but NodeOperation ${track.operationName} was not readable within ${seconds}s. It is not shown as in progress. Try again, or use manual rejoin.`;
            commitRejoin(key, { ...existing, submitting: false, error });
          }
        }));
      } finally {
        running = false;
      }
    };
    const timer = window.setInterval(() => void tick(), REJOIN_POLL_MS);
    void tick();
    return () => {
      controller.abort();
      window.clearInterval(timer);
    };
    // commitRejoin reads the latest tasks and language through refs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rejoinActiveKey]);

  const count = (value: string) => stationsOfPhase(items, value).length;
  const phaseTabs = [
    { value: "", label: zh ? "全部" : "All" },
    { value: "Bound", label: zh ? "在线" : "Online" },
    { value: "Offline", label: zh ? "离线" : "Offline" },
    { value: "Conflict", label: zh ? "冲突" : "Conflicts" },
    { value: "Retired", label: zh ? "已移除" : "Retired" },
  ];

  const powerPress = (item: StationItem, action: RobotPowerAction, label: string) => {
    const entry = { key: keyOf(item), node: item.metadata.name, namespace: item.metadata.namespace, desired: robotPowerAfter(action, robotPowerDesired(labelsOf(item))), action };
    // The machine must not be moving while the browser blocks on the confirmation dialog.
    void releasePoseSessions([{ key: keyOf(item), node: entry.node, namespace: entry.namespace, labels: labelsOf(item) }]).then(() => power.set([entry], label));
  };

  // The recovery entry of a station the plane does not see as Bound: re-read the node, the site network and the
  // admissions through the page's own load. It cannot reach a machine and a site link that are both down; that needs
  // someone at the machine or an out-of-band path, which the card says in its recovery line.
  const recheck = async (item: StationItem) => {
    if (recheckKey) return;
    setRecheckKey(keyOf(item));
    setActionError(undefined);
    try {
      await load();
    } finally {
      setRecheckKey("");
    }
  };

  const rejoinAssessment = (item: StationItem) => assessRobotRejoin({
    zh,
    stationPhase: stationPhase(item),
    identityVerified: siteNodeOf(item)?.identity_verified,
    siteDataState,
    admission: admissionOf(item),
  });

  // Cases that must not submit (no record, identity conflict, a Server SSH join, a code still waiting) stay on the
  // card as text. The primary button never opens a page.
  const explainRobotRejoin = (item: StationItem, key: string) => {
    const assessment = rejoinAssessment(item);
    setRejoinNotice((current) => ({ ...current, [key]: `${stationRecoveryHint(item, now, zh)} ${assessment.message}` }));
  };

  const rejoinErrorText = (reason: unknown) => {
    if (reason instanceof ApiError && reason.message) return reason.message;
    return reason instanceof Error && reason.message ? reason.message : (zhRef.current ? "重接请求失败" : "The rejoin request failed");
  };

  // A joined robot submits one rejoin for its Machine. The POST body is only a name: the card follows the NodeOperation
  // GET returns. An active task is attached instead of starting a second one. No confirmation and no SSH password.
  const submitRobotRejoin = (item: StationItem, key: string) => {
    if (rejoinBusy(rejoinTasksRef.current[key])) return;
    const assessment = rejoinAssessment(item);
    if (assessment.kind !== "joined-offline") {
      explainRobotRejoin(item, key);
      return;
    }
    const cluster = clusterForStation(clusters, item.metadata.namespace);
    if (!cluster) {
      setRejoinNotice((existing) => ({ ...existing, [key]: `${stationRecoveryHint(item, now, zh)} ${zh ? "没有唯一的集群记录，没有提交重接。" : "There is no single cluster record, so no rejoin was submitted."}` }));
      return;
    }
    const name = item.metadata.name;
    const signal = rejoinStop.current?.signal;
    if (!signal || signal.aborted) return;
    setRejoinNotice((existing) => {
      if (!(key in existing)) return existing;
      const next = { ...existing };
      delete next[key];
      return next;
    });
    rejoinMisses.current[key] = 0;
    commitRejoin(key, { submitting: true });
    const notSubmitted = (message: string) => zhRef.current ? `重接没有提交：${message}` : `Rejoin was not submitted: ${message}`;
    const attachActive = async () => {
      const listed = await readOperationPages((pageToken) => stationsApi.listNodeOperations(cluster.id, signal, pageToken));
      const operation = pickRejoinOperation(listed, cluster.id, name, undefined, Date.now(), false);
      if (!operation || isRejoinTerminal(operation.status?.phase)) return false;
      const track: RobotRejoinTrack = { clusterID: cluster.id, nodeName: name, machineName: "", requestID: operation.spec.requestID ?? "", operationName: operation.metadata.name };
      writeRejoinTrack(window.sessionStorage, track);
      rejoinMisses.current[key] = 0;
      commitRejoin(key, { track, operation, confirmed: true });
      return true;
    };
    void (async () => {
      try {
        const listed = await machinesApi.list("", signal);
        if (signal.aborted) return;
        const matches = machinesForRejoin(listed.items, cluster.id, name);
        if (matches.length !== 1) {
          if (matches.length === 0 && await attachActive()) return;
          const message = matches.length === 0
            ? (zhRef.current ? "没有找到与此集群和节点匹配的已入网 Machine。" : "No joined Machine matches this cluster and node.")
            : (zhRef.current ? "有多台 Machine 匹配此集群和节点。" : "More than one Machine matches this cluster and node.");
          commitRejoin(key, { error: notSubmitted(message) });
          return;
        }
        const machine = matches[0];
        const requestID = newRejoinRequestID();
        const model = robotModelForRejoin(labelsOf(item)?.["rlark.io/robot-model"]);
        const accepted = await machinesApi.rejoin(machine.name, { cluster_id: cluster.id, request_id: requestID, ...(model ? { robot_model: model } : {}) }, signal);
        if (signal.aborted) return;
        if (!isRobotRejoinOperation(accepted, cluster.id, name)) {
          if (await attachActive()) return;
          commitRejoin(key, { error: notSubmitted(zhRef.current ? "重接响应与此集群或节点不符，没有采用。" : "The rejoin response does not match this cluster or node, so it was not used.") });
          return;
        }
        const track: RobotRejoinTrack = { clusterID: cluster.id, nodeName: name, machineName: machine.name, requestID, operationName: accepted.metadata.name };
        writeRejoinTrack(window.sessionStorage, track);
        rejoinMisses.current[key] = 0;
        // The 202 body is a sketch with phase Pending. The poll reads the real NodeOperation before any phase is shown.
        commitRejoin(key, { track, confirmed: false });
      } catch (reason) {
        if (signal.aborted) return;
        const code = rejoinApiCode(reason);
        if (code === "REJOIN_IN_PROGRESS" || code === "JOIN_IN_PROGRESS" || code === "MACHINE_NOT_JOINED") {
          try {
            if (await attachActive()) return;
          } catch {
            // Fall through to the error already returned by the submit.
          }
        }
        commitRejoin(key, { error: notSubmitted(rejoinErrorText(reason)) });
      }
    })();
  };

  // The explicit manual fallback: Add node's machine workspace, with the station's cluster preselected. It never
  // opens the Server-only SSH form for a robot; the machine row there carries its own problem and the NodeClass
  // credentials RLark already stores.
  const manualRobotRejoin = (item: StationItem) => {
    const cluster = clusterForStation(clusters, item.metadata.namespace);
    onJoin(undefined, { ...(cluster ? { clusterID: cluster.id } : {}), method: "machines" });
  };

  // A whole-machine restart is written as one one-shot Node label, never as a command: the robot's node tuner keeps
  // its own node-profile, occupancy and emergency-stop guards, and the console only follows the records it publishes.
  const restartPress = (item: StationItem) => {
    if (restart.pending) return;
    const name = item.metadata.name;
    // Stop any live home-pose hold first, then confirm: only a robot confirmed stopped is asked to reboot, and the
    // confirmation (which blocks the browser) never runs while the machine may still be moving.
    void releasePoseSessions([{ key: keyOf(item), node: name, namespace: item.metadata.namespace, labels: labelsOf(item) }])
      .then((release) => {
        if (release !== "released") {
          restart.reject(name, zh
            ? `回位会话没有确认停止（${release}），本次不提交整机重启；请先确认机器人已停止。`
            : `The home-pose session was not confirmed stopped (${release}); the restart is not submitted. Make sure the robot has stopped first.`);
          return;
        }
        if (!window.confirm(zh
          ? `整机重启 ${name}？\n\n这会写入一次性 Node 标签 rlark.io/restart-request：robot tuner 会停驱动、下电并重启整机一次。它的节点配置、占用（Run 或设备会话）与急停守卫仍然生效，可能排队或拒绝。\n\n请确认现场有人、机器人周围安全、急停可及；重启完成前不要下发其他动作。`
          : `Restart the whole machine ${name}?\n\nThis writes the one-shot Node label rlark.io/restart-request: the robot's tuner stops the driver, powers off and reboots the machine once, under its node-profile, occupancy (a Run or device session) and emergency-stop guards, so it may queue or refuse.\n\nMake sure someone is on site, the robot's surroundings are safe, and its emergency stop is reachable; send no other action until it finishes.`)) return;
        restart.start({ key: keyOf(item), node: name, namespace: item.metadata.namespace, nodeUID: item.spec.nodeUID });
      });
  };

  // Inventory rows: the quiet-list grammar for non-robot nodes — icon, name, a dot status word, mono facts and a
  // chevron; the open detail spans the full width below the row.
  const row = (item: StationItem) => {
    const key = keyOf(item);
    const facts = stationFacts(item, clusterForStation(clusters, item.metadata.namespace), now, zh);
    const phase = stationPhase(item);
    const admission = admissionOf(item);
    const identityOff = siteNodeOf(item)?.identity_verified === "False";
    const detailsID = `station-details-${item.metadata.namespace}-${item.metadata.name}`;
    return <div key={key} className={`station-inv${openKey === key ? " is-expanded" : ""}${phase === "Retired" ? " is-retired" : ""}`}>
      <div className="station-inv-row">
        <span className="station-inv-icon" aria-hidden="true"><Server size={15} /></span>
        <div className="station-inv-name">
          <h3><NodeName node={nodeOf(item)} /></h3>
          <small>{identityOff ? <strong className="is-alert">{zh ? "身份不匹配" : "identity mismatch"}</strong> : null}{admission ? <span title={admission.reason}>{admissionStateLabel(admission.state, zh)}</span> : <span>{zh ? "未用入网码" : "No code"}</span>} · <code className="inline-code">UID {shortUID(item.spec.nodeUID)}</code></small>
        </div>
        <span className={`station-inv-status status ${stationStatusClass(phase)}`}><i />{stationPhaseLabel(phase, zh)}</span>
        <dl className="station-inv-facts">{facts.slice(0, 3).map((fact) => <div key={fact.key}><dt>{fact.label}</dt><dd>{fact.value}</dd></div>)}</dl>
        <button id={`station-expand-${item.metadata.namespace}-${item.metadata.name}`} type="button" className="station-inv-expand" aria-expanded={openKey === key} aria-haspopup="dialog" aria-controls={detailsID} aria-label={openKey === key ? (zh ? "关闭详情" : "Close details") : (zh ? "展开详情" : "Expand details")} title={openKey === key ? (zh ? "关闭详情" : "Close details") : (zh ? "展开详情" : "Expand details")} onClick={() => toggleOpen(item)}>
          <ChevronDown size={16} className={openKey === key ? "is-open" : ""} aria-hidden="true" />
        </button>
      </div>
    </div>;
  };

  const cardHandlers = useRef({ powerPress, restartPress, recheck, toggleOpen, openZoom });
  cardHandlers.current = { powerPress, restartPress, recheck, toggleOpen, openZoom };
  const cardActions = useMemo(() => {
    type Actions = Pick<ComponentProps<typeof StationCard>, "onToggle" | "onChoose" | "onZoom"> & {
      press: ComponentProps<typeof StationCard>["power"]["press"];
      start: () => void;
      run: () => void;
    };
    return new Map(items.map((item): [string, Actions] => [keyOf(item), {
      press: (action, label) => cardHandlers.current.powerPress(item, action, label),
      start: () => cardHandlers.current.restartPress(item),
      run: () => void cardHandlers.current.recheck(item),
      onToggle: () => cardHandlers.current.toggleOpen(item),
      onChoose: (checked) => choose(item, checked),
      onZoom: (camera, trigger, fps) => cardHandlers.current.openZoom(item, camera, trigger, fps),
    }]));
  }, [items, choose]);
  const card = (item: StationItem, variant: StationCardVariant) => {
    const key = keyOf(item);
    const labels = labelsOf(item);
    const actions = cardActions.get(key)!;
    // The card shows the assessed recovery path after a click; until then only the page's own recovery line is shown.
    const rejoinMessage = rejoinNotice[key];
    const rejoinTask = rejoinTasks[key];
    const rejoinPending = rejoinBusy(rejoinTask);
    const rejoinCard = rejoinTask ? rejoinObservation(rejoinTask, zh) : undefined;
    const observation = restartObservationOf(item);
    const outstanding = outstandingRestart(observation);
    const local = restart.requests[key];
    // The restart entry is for an S1U robot (its Addon's tuner is the one that acts on rlark.io/restart-request). The
    // button is offered only while the plane sees it Bound; the records and a request already in flight stay visible
    // when a reboot takes it Offline for a while, so the card does not forget what is happening. The write re-checks
    // the management Node's identity and reachability.
    const restartAvailable = stationPhase(item) === "Bound" && !item.spec.retired && machineModel(labels).toLowerCase() === "s1u";
    const restartBlocked = !local && outstanding
      ? (zh
        ? `节点上已有未完成的重启请求 ${outstanding.request}（${restartPhaseLabel(outstanding.phase, zh)}）${outstanding.why ? `：${outstanding.why}` : ""}。等它结束后再提交新的请求。`
        : `The Node already carries an unfinished restart request ${outstanding.request} (${restartPhaseLabel(outstanding.phase, zh)})${outstanding.why ? `: ${outstanding.why}` : ""}. Wait for it to finish before asking again.`)
      : undefined;
    return <StationCard
      key={key}
      station={item}
      cluster={clusterForStation(clusters, item.metadata.namespace)}
      node={nodeOf(item)}
      siteNode={siteNodeOf(item)}
      cameraReport={fleetByKey.get(key)?.annotations?.["rlark.io/cameras"]}
      admission={admissionOf(item)}
      labels={labels}
      siteDataState={siteDataState}
      now={now}
      zh={zh}
      expanded={openKey === key}
      liveEnabled={cardLive}
      chosen={chosen.has(selectionKey(item))}
      power={{ pending: power.pending, press: actions.press, request: power.requests[key] }}
      restart={machineModel(labels).toLowerCase() === "s1u" && !item.spec.retired ? {
        available: restartAvailable,
        pending: restart.pending && (local ? local.phase === "sending" : restart.activeLabel === item.metadata.name),
        blocked: restartBlocked,
        request: local,
        outstanding: local ? undefined : outstanding,
        observation,
        start: actions.start,
      } : undefined}
      recheck={{ pending: recheckKey === key, run: actions.run }}
      rejoin={{
        liveKey: key,
        notice: rejoinMessage,
        pending: rejoinPending,
        task: rejoinCard,
        reason: zh
          ? "已入网的机器人按原 Machine 与 NodeClass 配置提交重接，不输入 SSH 口令。其他情况只说明原因并给出人工入口，不会新建记录。"
          : "A robot that already joined submits a rejoin with its Machine and NodeClass configuration, and no SSH password. Any other case only explains why and offers the manual entry. No record is created.",
        press: () => submitRobotRejoin(item, key),
        manual: () => manualRobotRejoin(item),
      }}
      variant={variant}
      onToggle={actions.onToggle}
      onChoose={actions.onChoose}
      onZoom={actions.onZoom}
    />;
  };

  const panel = (item: StationItem) => {
    const key = keyOf(item);
    return <StationDetailPanel
      key={`panel-${key}`}
      station={item}
      cluster={clusterForStation(clusters, item.metadata.namespace)}
      siteNode={siteNodeOf(item)}
      admission={admissionOf(item)}
      hasNode={`${item.metadata.namespace}/${item.metadata.name}` in nodesByKey}
      labels={labelsOf(item)}
      powerNote={nodesByKey[`${item.metadata.namespace}/${item.metadata.name}`]?.metadata.annotations?.["rlark.io/power"]}
      siteDataState={siteDataState}
      refreshError={actionError?.key === key ? actionError.message : ""}
      now={now}
      zh={zh}
      power={{ pending: power.pending, press: (action, label) => powerPress(item, action, label) }}
      deleting={deletePending === key}
      admissionPending={admissionPending?.key === key ? admissionPending.kind : ""}
      onDismiss={() => closePanel(item)}
      cameraActive={!zoom}
      onDelete={() => void removeRecord(item)}
      onReissue={() => void actAdmission(item, "reissue")}
      onRevoke={() => void actAdmission(item, "revoke")}
      onRefresh={() => { setActionError(undefined); void load(); }}
      onZoom={(view, trigger) => openZoom(item, view, trigger)}
    />;
  };

  const kpis: { id: SummaryFilter; label: string; count: number }[] = [
    { id: "", label: zh ? "总数" : "Total", count: metrics.total },
    { id: "alerts", label: zh ? "需关注异常" : "Needs attention", count: metrics.alerts },
    { id: "driver", label: zh ? "驱动就绪" : "Drivers ready", count: metrics.driverReady },
    { id: "camera", label: zh ? "相机异常" : "Camera faults", count: metrics.cameraFault },
    { id: "occupied", label: zh ? "占用中" : "Occupied", count: metrics.occupied },
    { id: "offline", label: zh ? "离线" : "Offline", count: metrics.byPhase.Offline ?? 0 },
  ];
  const modes: [FleetViewMode, string][] = [
    ["table", zh ? "列表" : "List"], ["cards", zh ? "卡片" : "Cards"], ["wall", zh ? "监控墙" : "Camera wall"],
  ];
  const groupToggle = (group: FleetGroup) => <button type="button"
    className="stations-group-toggle" key={group.id} aria-expanded={!collapsed.has(group.id)} onClick={() => {
      setCollapsed((current) => {
        const next = new Set(current);
        if (next.has(group.id)) next.delete(group.id);
        else next.add(group.id);
        return next;
      });
      setCardPage(1);
    }}>
    <ChevronDown size={14} aria-hidden="true" />
    <strong>{group.name}</strong>
    <small>{zh ? "在线" : "Online"} {group.online}/{group.items.length}</small>
    {group.alerts > 0 && <small className="stations-table-alert">{zh ? "异常" : "Alerts"} {group.alerts}</small>}
  </button>;

  return (
    <div className="page-content resource-page stations-page">
      <div className="stations-frame">
      <div className="stations-kpis" role="group" aria-label={zh ? "机群态势" : "Fleet summary"}>
        {kpis.map(({ id, label, count: value }) => <button type="button" key={id} data-metric={id} aria-pressed={summaryFilter === id}
          onClick={() => setSummaryFilter(id === summaryFilter ? "" : id)}>
          <span>{label}</span><strong>{value}</strong>
        </button>)}
      </div>
      <div className="stations-phase-tabs" role="group" aria-label={zh ? "按状态筛选" : "Filter by status"}>
        {phaseTabs.map((tab) => <button key={tab.value} type="button" aria-pressed={phase === tab.value} className={phase === tab.value ? "is-active" : ""} onClick={() => setPhase(tab.value)}>
          {tab.label}<span>{count(tab.value)}</span>
        </button>)}
      </div>

      <div className="stations-toolbar">
        <PageToolbar
          placeholder={zh ? "搜索节点名或 UID" : "Search Node or UID"}
          value={search}
          onChange={setSearch}
          count={visible.length}
          copy={c}
          onRefresh={async () => {
            if (refreshing) return;
            setRefreshing(true);
            await load();
          }}
          refreshing={refreshing}
        />
        <label className="toolbar-filter stations-cluster-filter">
          <ListFilter size={16} />
          <select aria-label={zh ? "成员集群" : "Member cluster"} value={clusterID} onChange={(event) => setClusterID(event.target.value)}>
            <option value="">{zh ? "所有集群" : "All clusters"}</option>
            {clusters.map((cluster) => <option key={cluster.id} value={cluster.id}>{cluster.name} ({cluster.agentNamespace})</option>)}
          </select>
        </label>
        <button type="button" className="secondary-button station-operation-history" onClick={() => setShowNodeOperations((value) => !value)} disabled={!clusterID} aria-expanded={showNodeOperations}>
          <ListFilter size={15} />{zh ? "节点操作" : "Node operations"}
        </button>
        <button type="button" className="secondary-button" onClick={onModels}>{zh ? "机器人型号" : "Robot models"}<Boxes size={14} /></button>
        <button type="button" className="primary-button station-join-code" onClick={() => onJoin()} disabled={!clusters.length}>
          <KeyRound size={15} />{zh ? "加入节点" : "Add node"}
        </button>
      </div>

      {showNodeOperations && clusterID && <NodeOperations key={clusterID} clusterID={clusterID} zh={zh} nodeAt={nodeAt} />}

      {error && <div className="cert-error" role="alert" style={{ marginBottom: 12 }}>{error}</div>}

      <div className="stations-fleet-filters">
        <button type="button" className="secondary-button" aria-pressed={alertOnly} onClick={() => setAlertOnly(!alertOnly)}>
          <AlertTriangle size={14} />{zh ? "仅看异常" : "Alerts only"}
        </button>
        <select aria-label={zh ? "型号" : "Model"} value={model} onChange={(event) => setModel(event.target.value)}>
          <option value="">{zh ? "所有型号" : "All models"}</option>{models.map((value) => <option key={value}>{value}</option>)}
        </select>
        <select aria-label={zh ? "区域" : "Zone"} value={zone} onChange={(event) => setZone(event.target.value)}>
          <option value="">{zh ? "所有区域" : "All zones"}</option>{zones.map((value) => <option key={value}>{value}</option>)}
        </select>
        <select aria-label={zh ? "电源与驱动" : "Power and driver"} value={powerFilter} onChange={(event) => setPowerFilter(event.target.value as FleetPowerFilter)}>
          <option value="all">{zh ? "所有电源与驱动态" : "All power and driver states"}</option>
          <option value="running">{zh ? "驱动就绪" : "Driver ready"}</option><option value="powered">{zh ? "已上电" : "Powered"}</option>
          <option value="off">{zh ? "未上电" : "Off"}</option><option value="alert">{zh ? "电源或驱动异常" : "Power or driver alert"}</option>
        </select>
        <select aria-label={zh ? "占用态" : "Occupancy"} value={occupancy} onChange={(event) => setOccupancy(event.target.value as FleetOccupancyFilter)}>
          <option value="all">{zh ? "所有占用态" : "All occupancy states"}</option>
          <option value="idle">{zh ? "空闲" : "Idle"}</option><option value="occupied">{zh ? "占用中" : "Occupied"}</option>
        </select>
        <select aria-label={zh ? "排序" : "Sort"} value={sort} onChange={(event) => setSort(event.target.value as FleetSortKey)}>
          <option value="alert">{zh ? "异常优先" : "Alerts first"}</option><option value="name">{zh ? "按名称" : "By name"}</option>
          <option value="heartbeat">{zh ? "最近上报" : "Latest report"}</option><option value="restarts">{zh ? "重启次数" : "Reboots"}</option>
        </select>
        <div className="stations-segments stations-view-switch" role="group" aria-label={zh ? "视图" : "View"}>
          {modes.map(([mode, label]) => <button key={mode} type="button" aria-pressed={view === mode} onClick={() => changeView(mode)}>{label}</button>)}
        </div>
      </div>

      {view !== "wall" && <>
        <div className="stations-selection">
          <button type="button" className="secondary-button" disabled={!entries.length || chosenEntries.length === entries.length} onClick={() => {
            setChosen((current) => new Set([...current, ...visibleRobots.map(selectionKey)]));
          }}>{zh ? `选中当前筛选全部 ${entries.length} 台` : `Select all ${entries.length} matching robots`}</button>
          {chosen.size > 0 && <button type="button" className="secondary-button" onClick={() => setChosen(new Set())}>{zh ? "清空选择" : "Clear selection"}</button>}
          {chosenEntries.length > 0 && <>
            <button type="button" className="secondary-button" onClick={() => {
              setWallKeys((current) => new Set([...current, ...chosenItems.map(selectionKey)]));
              changeView("wall");
            }}>{zh ? "加入监控墙" : "Add to camera wall"}</button>
            <RobotControls robots={entries} chosen={chosenEntries} power={power} zh={zh} showMessage={false} showPose={false} />
          </>}
        </div>
        {view === "table" && groups.size > 0 && <div className="stations-table-clusters" role="group" aria-label={zh ? "折叠集群" : "Collapse clusters"}>
          {[...groups.values()].map(groupToggle)}
        </div>}
      </>}
      {view === "cards" && <div className="stations-view-options">
        <label>{zh ? "每页" : "Per page"} <select value={cardPageSize} onChange={(event) => { setCardPageSize(Number(event.target.value)); setCardPage(1); }}>
          {[12, 24, 48].map((size) => <option key={size}>{size}</option>)}
        </select></label>
        <div className="stations-segments" role="group" aria-label={zh ? "卡片预览模式" : "Card preview mode"}>
          <button type="button" aria-pressed={cardLive} onClick={() => setCardLive(true)}>{zh ? "实时流（最多 3 路）" : "Live (up to 3)"}</button>
          <button type="button" aria-pressed={!cardLive} onClick={() => setCardLive(false)}>{zh ? "省流抽帧" : "Snapshots"}</button>
        </div>
        <FleetPagination page={currentCardPage} pageCount={cardPages} zh={zh} onPage={setCardPage} />
      </div>}

      <section className={`stations-groups refreshable-region${refreshing ? " is-refreshing" : ""}`} aria-busy={refreshing} aria-label={zh ? "场站节点" : "Site nodes"}>
        {loading && items.length === 0 ? <div className="stations-state muted">{zh ? "正在加载…" : "Loading…"}</div>
          : view === "wall" ? <CameraWallView items={wallKeys.size ? fleet.filter((item) => isRobotItem(item) && wallKeys.has(selectionKey(item))) : visibleRobots}
              metaByKey={metaByKey} zh={zh} selected={wallKeys.size > 0} onClear={() => setWallKeys(new Set())} onPreview={openZoom} />
          : visible.length === 0 ? <div className="stations-state muted">{zh ? "没有匹配的场站节点" : "No matching site nodes"}</div>
          : view === "table" ? <StationsTableView items={unfolded} metaByKey={metaByKey} nodesByKey={nodesByKey} chosen={selectedKeys}
              now={now} zh={zh} busy={power.pending} restartBusy={(item) => Boolean(outstandingRestart(restartObservationOf(item)) ||
                (restart.pending && restart.activeLabel === item.metadata.name))}
              recheckKey={recheckKey} onChoose={choose} onPower={powerPress} onDetails={toggleOpen} onRecheck={(item) => void recheck(item)}
              onRejoin={(item) => submitRobotRejoin(item, keyOf(item))} onPreview={(item, trigger) => {
                const cells = cameraCells(publishedCameras(item.site?.cameras ?? item.annotations?.["rlark.io/cameras"]), zh);
                const primary = cells.find((cell) => cell.frames && cell.present !== false) ?? cells.find((cell) => cell.present !== false) ?? cells[0];
                if (primary) openZoom(item, primary.view, trigger);
              }} />
          : [...groups.values()].map((group) => {
            const pageItems = group.items.filter((item) => cardKeys.has(keyOf(item)));
            const isCollapsed = collapsed.has(group.id);
            if (!isCollapsed && !pageItems.length) return null;
            const controls = pageItems.filter((item) => isControlPlaneLabels(labelsOf(item)));
            const robots = pageItems.filter((item) => isRobotItem(item) && !isControlPlaneLabels(labelsOf(item)));
            const others = pageItems.filter((item) => !isControlPlaneLabels(labelsOf(item)) && !isRobotItem(item));
            return <section className="stations-group stations-robot-group" key={group.id} aria-label={group.name}>
              {groupToggle(group)}
              {!isCollapsed && <>
                {controls.map((item) => card(item, "control"))}
                {robots.length > 0 && <CardGrid items={robots} className="is-robot-grid" renderCard={(item) => card(item, "robot")} />}
                {others.length > 0 && <div className="stations-inv-list">{others.map(row)}</div>}
              </>}
            </section>;
          })}
        <RefreshOverlay visible={refreshing} label={zh ? "刷新中" : "Refreshing"} />
      </section>

      <section className="stations-admission-section" aria-labelledby="stations-admissions-title">
        <header className="stations-admission-section-heading">
          <button type="button" id="stations-admissions-title" className="stations-group-toggle" aria-expanded={showAdmissions} onClick={() => setShowAdmissions(!showAdmissions)}>
            <ChevronDown size={14} aria-hidden="true" /><strong>{zh ? "入网记录" : "Admission records"}</strong>
          </button>
          {showAdmissions && <span className="stations-table-count">{recordAdmissions.length} {zh ? "条记录" : "records"}</span>}
        </header>
        {showAdmissions && (admissionsLoading ? <div className="stations-state muted">{zh ? "正在加载入网记录…" : "Loading admission records…"}</div>
          : <AdmissionRecords admissions={recordAdmissions} clusters={clusters} loadError={admissionError} zh={zh} onJoin={onJoin} onChanged={() => load()} />)}
      </section>
      </div>
      {zoom && <CameraPreviewDialog node={zoom.item.metadata.name} namespace={zoom.item.metadata.namespace} view={zoom.view} cell={zoom.cell} zh={zh} initialFps={zoom.fps} onDismiss={closeZoom} />}
      {detailItem && <StationDetailDialog labelledBy={`${detailsID}-title`} onClose={() => closePanel(detailItem)}>
        {panel(detailItem)}
      </StationDetailDialog>}
      {!detailItem && !zoom && <BatchProgressPanel batch={power.batch} cancelBatch={power.cancelBatch} retryFailed={power.retryFailed} dismissBatch={() => { power.dismissBatch(); power.clear(); }} zh={zh} />}
      {(power.pending || power.message) && (!power.batch || detailItem || zoom) && (toastHost
        ? createPortal(<div className={`stations-power-toast stations-power-toast-in-dialog${power.failed ? " is-error" : ""}`} role={power.failed ? "alert" : "status"}>
          <span className={`stations-power-toast-icon${power.pending ? " is-pending" : ""}`} aria-hidden="true">{power.pending ? <span className="stations-power-spinner" /> : power.failed ? <AlertTriangle size={16} /> : <Check size={16} />}</span>
          <div className="stations-power-toast-copy">
            <strong>{power.pending ? (zh ? `正在下发：${power.activeLabel || "命令"}` : `Sending: ${power.activeLabel || "command"}`) : power.failed ? (zh ? "操作失败" : "Action failed") : (zh ? "操作已下发" : "Action sent")}</strong>
            <p>{power.pending ? (zh ? "请稍候，命令正在发送…" : "Sending the command…") : power.message}</p>
          </div>
          {!power.pending && <button type="button" className="stations-power-toast-close" onClick={power.clear} aria-label={zh ? "关闭" : "Dismiss"}>×</button>}
        </div>, toastHost)
        : <div className={`stations-power-toast${power.failed ? " is-error" : ""}`} role={power.failed ? "alert" : "status"}>
          <span className={`stations-power-toast-icon${power.pending ? " is-pending" : ""}`} aria-hidden="true">{power.pending ? <span className="stations-power-spinner" /> : power.failed ? <AlertTriangle size={16} /> : <Check size={16} />}</span>
          <div className="stations-power-toast-copy">
            <strong>{power.pending ? (zh ? `正在下发：${power.activeLabel || "命令"}` : `Sending: ${power.activeLabel || "command"}`) : power.failed ? (zh ? "操作失败" : "Action failed") : (zh ? "操作已下发" : "Action sent")}</strong>
            <p>{power.pending ? (zh ? "请稍候，命令正在发送…" : "Sending the command…") : power.message}</p>
          </div>
          {!power.pending && <button type="button" className="stations-power-toast-close" onClick={power.clear} aria-label={zh ? "关闭" : "Dismiss"}>×</button>}
        </div>)}
      {(restart.pending || restart.message) && (toastHost
        ? createPortal(<div className={`stations-power-toast stations-power-toast-in-dialog${restart.failed ? " is-error" : ""}`} role={restart.failed ? "alert" : "status"}>
          <span className={`stations-power-toast-icon${restart.pending ? " is-pending" : ""}`} aria-hidden="true">{restart.pending ? <span className="stations-power-spinner" /> : restart.failed ? <AlertTriangle size={16} /> : <Check size={16} />}</span>
          <div className="stations-power-toast-copy">
            <strong>{restart.pending ? (zh ? `正在提交整机重启：${restart.activeLabel || "节点"}` : `Submitting the restart: ${restart.activeLabel || "Node"}`) : restart.failed ? (zh ? "整机重启未提交" : "Restart not submitted") : (zh ? "整机重启已提交" : "Restart submitted")}</strong>
            <p>{restart.pending ? (zh ? "请稍候，正在确认节点身份并写入重启请求…" : "Confirming the Node's identity and writing the restart request…") : restart.message}</p>
          </div>
          {!restart.pending && <button type="button" className="stations-power-toast-close" onClick={restart.clear} aria-label={zh ? "关闭" : "Dismiss"}>×</button>}
        </div>, toastHost)
        : <div className={`stations-power-toast${restart.failed ? " is-error" : ""}`} role={restart.failed ? "alert" : "status"}>
          <span className={`stations-power-toast-icon${restart.pending ? " is-pending" : ""}`} aria-hidden="true">{restart.pending ? <span className="stations-power-spinner" /> : restart.failed ? <AlertTriangle size={16} /> : <Check size={16} />}</span>
          <div className="stations-power-toast-copy">
            <strong>{restart.pending ? (zh ? `正在提交整机重启：${restart.activeLabel || "节点"}` : `Submitting the restart: ${restart.activeLabel || "Node"}`) : restart.failed ? (zh ? "整机重启未提交" : "Restart not submitted") : (zh ? "整机重启已提交" : "Restart submitted")}</strong>
            <p>{restart.pending ? (zh ? "请稍候，正在确认节点身份并写入重启请求…" : "Confirming the Node's identity and writing the restart request…") : restart.message}</p>
          </div>
          {!restart.pending && <button type="button" className="stations-power-toast-close" onClick={restart.clear} aria-label={zh ? "关闭" : "Dismiss"}>×</button>}
        </div>)}
    </div>
  );
}
