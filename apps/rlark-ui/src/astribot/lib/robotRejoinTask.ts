import type { NodeOperation } from "../api.js";
import { machineJoinSteps } from "./machineJoin.js";
import type { Machine } from "./siteNodeDisplay.js";

// A robot rejoin is one NodeOperation, isolated by the cluster, the node and the task id the Gateway returns.
// The card never invents a phase: it renders this record, and a refresh reads the same record back.

const STORAGE_KEY = "rlark.robot-rejoin.v1";
const TERMINAL = new Set(["Succeeded", "Failed", "Blocked"]);
const SHOWN_FOR_MS = 24 * 60 * 60 * 1000;

export interface RobotRejoinTrack {
  clusterID: string;
  nodeName: string;
  machineName: string;
  requestID: string;
  operationName: string;
}

export function rejoinTrackKey(clusterID: string, nodeName: string): string {
  return `${clusterID}\n${nodeName}`;
}

export function isRejoinTerminal(phase: string | undefined): boolean {
  return TERMINAL.has(phase ?? "");
}

// Installer stages of a robot Join (station-controller). An unknown stage is shown as the API wrote it.
export function rejoinStageLabel(
  stage: string | undefined,
  zh: boolean,
): string {
  if (!stage) return "";
  const known = machineJoinSteps.find((step) => step.name === stage);
  return known ? (zh ? known.zh : known.en) : stage;
}

export function rejoinPhase(
  operation: NodeOperation | undefined,
): "Pending" | "Running" | "Succeeded" | "Failed" | "Blocked" {
  const phase = operation?.status?.phase;
  if (
    phase === "Running" ||
    phase === "Succeeded" ||
    phase === "Failed" ||
    phase === "Blocked"
  )
    return phase;
  return "Pending";
}

// The failure the operator can act on: the latest condition that says why, bounded, with no extra text added.
export function rejoinConditionReason(
  operation: NodeOperation | undefined,
): string {
  const conditions = operation?.status?.conditions ?? [];
  for (let index = conditions.length - 1; index >= 0; index -= 1) {
    const condition = conditions[index];
    const failed =
      condition.status === "False" ||
      condition.type === "Failed" ||
      condition.type === "Blocked";
    const text = [condition.reason, condition.message]
      .map((part) => part?.trim() ?? "")
      .filter(Boolean)
      .join(": ");
    if (failed && text) return text.slice(0, 400);
  }
  const last = conditions[conditions.length - 1];
  return [last?.reason, last?.message]
    .map((part) => part?.trim() ?? "")
    .filter(Boolean)
    .join(": ")
    .slice(0, 400);
}

export function describeRejoin(
  phase: string,
  stage: string | undefined,
  reason: string,
  zh: boolean,
): { text: string; title: string; alert: boolean } {
  const step = rejoinStageLabel(stage, zh);
  const stageBit = step ? ` · ${step}` : "";
  if (phase === "Succeeded") {
    const text = zh ? "重接完成" : "Rejoin finished";
    return { alert: false, title: text, text };
  }
  if (phase === "Failed" || phase === "Blocked") {
    const label =
      phase === "Failed"
        ? zh
          ? "重接失败"
          : "Rejoin failed"
        : zh
          ? "重接被阻断"
          : "Rejoin blocked";
    const text = reason
      ? `${label}${zh ? "：" : ": "}${reason}`
      : `${label}${stageBit}`;
    return { alert: true, title: text, text };
  }
  if (phase === "Running") {
    const text = zh ? `重接进行中${stageBit}` : `Rejoin in progress${stageBit}`;
    return { alert: false, title: text, text };
  }
  const text = zh ? `重接已提交${stageBit}` : `Rejoin submitted${stageBit}`;
  return { alert: false, title: text, text };
}

// The Gateway names a rejoin Join "join-rejoin-…" and prefixes its request id with "rejoin-".
// clusterID is required: an operation that does not name this cluster is another task.
export function isRobotRejoinOperation(
  operation: NodeOperation,
  clusterID: string,
  nodeName: string,
): boolean {
  const requestID = operation.spec.requestID ?? "";
  return (
    operation.spec.action === "Join" &&
    operation.spec.nodeName === nodeName &&
    operation.spec.clusterID === clusterID &&
    requestID.startsWith("rejoin-") &&
    operation.metadata.name.startsWith("join-rejoin-")
  );
}

// Phase, stage and condition text. A poll that repeats this key has nothing new to draw.
export function rejoinStatusKey(operation: NodeOperation | undefined): string {
  const status = operation?.status;
  const conditions = (status?.conditions ?? [])
    .map((condition) =>
      [
        condition.type,
        condition.status,
        condition.reason ?? "",
        condition.message ?? "",
      ].join("\u001f"),
    )
    .join("\u001e");
  return [status?.phase ?? "", status?.stage ?? "", conditions].join("\u001f");
}

export interface RobotRejoinCardState {
  submitting?: boolean;
  error?: string;
  track?: RobotRejoinTrack;
  operation?: NodeOperation;
  // True only after GET /node-operations/{name} returned this object. The rejoin POST body is a sketch, always Pending.
  confirmed?: boolean;
}

// True while a click is in flight, or a task is accepted and not yet terminal. An error or a finished phase can be clicked again.
export function rejoinBusy(task: RobotRejoinCardState | undefined): boolean {
  if (!task) return false;
  if (task.submitting) return true;
  if (
    task.error &&
    !(
      task.confirmed &&
      task.operation &&
      !isRejoinTerminal(task.operation.status?.phase)
    )
  )
    return false;
  if (!task.track) return false;
  if (task.confirmed && isRejoinTerminal(task.operation?.status?.phase))
    return false;
  return true;
}

// What the page state has to change for: a new task, a new phase bucket, or an error. A stage change inside Running is not one of these.
export function rejoinCoarseKey(
  task: RobotRejoinCardState | undefined,
): string {
  if (!task) return "";
  const phase =
    task.confirmed && task.operation ? rejoinPhase(task.operation) : "";
  return [
    task.submitting ? "1" : "0",
    task.error ?? "",
    task.track?.operationName ?? "",
    task.confirmed ? "1" : "0",
    phase,
  ].join("\u001f");
}

export interface RobotRejoinObservation {
  phase: string;
  text: string;
  title: string;
  alert: boolean;
}

// The line the card draws. The POST sketch is not a phase: until GET confirms the object, the card says it is reading that name.
export function rejoinObservation(
  task: RobotRejoinCardState,
  zh: boolean,
): RobotRejoinObservation {
  if (task.submitting && !task.track) {
    const text = zh ? "正在提交重接…" : "Submitting rejoin…";
    return { phase: "submitting", text, title: text, alert: false };
  }
  if (task.error && !(task.confirmed && task.operation)) {
    return {
      phase: "Failed",
      text: task.error,
      title: task.error,
      alert: true,
    };
  }
  if (task.confirmed && task.operation) {
    const phase = rejoinPhase(task.operation);
    return {
      phase,
      ...describeRejoin(
        phase,
        task.operation.status?.stage,
        rejoinConditionReason(task.operation),
        zh,
      ),
    };
  }
  if (task.track) {
    const text = zh
      ? `重接已接受，正在读取 ${task.track.operationName}`
      : `Rejoin accepted, reading ${task.track.operationName}`;
    return {
      phase: "Pending",
      text,
      title: task.track.operationName,
      alert: false,
    };
  }
  const text = zh ? "正在准备重接…" : "Preparing rejoin…";
  return { phase: "Pending", text, title: text, alert: false };
}

// Follow a Station list's continue token. The same token twice, or a blank one, stops the walk.
export async function readOperationPages(
  readPage: (
    pageToken: string | undefined,
  ) => Promise<{ items: NodeOperation[]; continue?: string }>,
  maxPages = 20,
): Promise<NodeOperation[]> {
  const items: NodeOperation[] = [];
  const seen = new Set<string>();
  let pageToken: string | undefined;
  for (let page = 0; page < maxPages; page += 1) {
    const response = await readPage(pageToken);
    items.push(...response.items);
    const next = response.continue;
    if (!next || seen.has(next)) return items;
    seen.add(next);
    pageToken = next;
  }
  return items;
}

// Stage text is published here, not through the page's state. One card subscribes to its own key.
const rejoinObservations = new Map<string, RobotRejoinObservation>();
const rejoinListeners = new Map<string, Set<() => void>>();

export function readRejoinObservation(
  key: string,
): RobotRejoinObservation | undefined {
  return rejoinObservations.get(key);
}

export function subscribeRejoinObservation(
  key: string,
  listener: () => void,
): () => void {
  let targets = rejoinListeners.get(key);
  if (!targets) {
    targets = new Set();
    rejoinListeners.set(key, targets);
  }
  targets.add(listener);
  const bucket = targets;
  return () => {
    bucket.delete(listener);
    if (bucket.size === 0) rejoinListeners.delete(key);
  };
}

export function publishRejoinObservation(
  key: string,
  observation: RobotRejoinObservation | undefined,
): void {
  const current = rejoinObservations.get(key);
  if (observation === undefined) {
    if (current === undefined) return;
    rejoinObservations.delete(key);
  } else if (
    current &&
    current.phase === observation.phase &&
    current.text === observation.text &&
    current.title === observation.title &&
    current.alert === observation.alert
  ) {
    return;
  } else {
    rejoinObservations.set(key, observation);
  }
  const targets = rejoinListeners.get(key);
  if (!targets) return;
  for (const listener of [...targets]) listener();
}

export function clearRejoinObservations(): void {
  const keys = [...rejoinObservations.keys()];
  rejoinObservations.clear();
  for (const key of keys) {
    const targets = rejoinListeners.get(key);
    if (!targets) continue;
    for (const listener of [...targets]) listener();
  }
}

function createdAt(operation: NodeOperation): number {
  const parsed = Date.parse(operation.metadata.creationTimestamp ?? "");
  return Number.isFinite(parsed) ? parsed : 0;
}

// The task for this cluster and node only. A stored id wins; otherwise the newest non-terminal rejoin, then the
// newest terminal one that is still worth showing. Another node's operation is never selected.
export function pickRejoinOperation(
  operations: NodeOperation[],
  clusterID: string,
  nodeName: string,
  operationName: string | undefined,
  now: number,
  stored: boolean,
): NodeOperation | undefined {
  const mine = operations.filter((operation) =>
    isRobotRejoinOperation(operation, clusterID, nodeName),
  );
  const named = operationName
    ? mine.find((operation) => operation.metadata.name === operationName)
    : undefined;
  if (named) return named;
  const visible = mine.filter(
    (operation) =>
      !isRejoinTerminal(operation.status?.phase) ||
      stored ||
      now - createdAt(operation) < SHOWN_FOR_MS,
  );
  const active = visible.filter(
    (operation) => !isRejoinTerminal(operation.status?.phase),
  );
  const pool = active.length ? active : visible;
  return pool
    .slice()
    .sort((a, b) => createdAt(a) - createdAt(b))
    .at(-1);
}

// One joined robot Machine for this cluster and node. Zero or several means the card must not guess.
export function machinesForRejoin(
  machines: Machine[],
  clusterID: string,
  nodeName: string,
): Machine[] {
  return machines.filter(
    (machine) =>
      (machine.phase === "Joined" ||
        (machine.phase === "Admitted" &&
          ["Failed", "Blocked"].includes(machine.join?.phase ?? ""))) &&
      machine.node_name === nodeName &&
      machine.machine_type !== "Server" &&
      (machine.cluster_id === clusterID ||
        machine.admit_cluster_id === clusterID),
  );
}

const DNS_LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

export function robotModelForRejoin(
  model: string | undefined,
): string | undefined {
  const value = (model ?? "").trim().toLowerCase();
  return DNS_LABEL.test(value) ? value : undefined;
}

// A DNS label the rejoin API accepts as request_id. The clock and the random nibble keep two clicks apart.
export function newRejoinRequestID(
  now = Date.now(),
  rand = Math.random(),
): string {
  const nibble = Math.floor(
    Math.max(0, Math.min(0.999999, rand)) * 36,
  ).toString(36);
  return `card${now.toString(36)}${nibble}`;
}

export function readRejoinTracks(
  storage: Pick<Storage, "getItem"> | undefined,
): RobotRejoinTrack[] {
  if (!storage) return [];
  try {
    const parsed = JSON.parse(storage.getItem(STORAGE_KEY) ?? "[]") as unknown;
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(isTrack);
  } catch {
    return [];
  }
}

export function writeRejoinTrack(
  storage: Pick<Storage, "getItem" | "setItem"> | undefined,
  track: RobotRejoinTrack,
): void {
  if (!storage || !isTrack(track)) return;
  const tracks = readRejoinTracks(storage).filter(
    (item) =>
      rejoinTrackKey(item.clusterID, item.nodeName) !==
      rejoinTrackKey(track.clusterID, track.nodeName),
  );
  tracks.push(track);
  storage.setItem(STORAGE_KEY, JSON.stringify(tracks));
}

function isTrack(value: unknown): value is RobotRejoinTrack {
  if (!value || typeof value !== "object") return false;
  const track = value as Partial<RobotRejoinTrack>;
  return (
    typeof track.clusterID === "string" &&
    track.clusterID !== "" &&
    typeof track.nodeName === "string" &&
    track.nodeName !== "" &&
    typeof track.machineName === "string" &&
    typeof track.requestID === "string" &&
    track.requestID !== "" &&
    typeof track.operationName === "string" &&
    track.operationName.startsWith("join-rejoin-")
  );
}
