import type { CRDNode } from "../../types";
import { requestJson, ApiError as SharedApiError } from "../../api.js";
export type Condition = {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  observedGeneration?: number;
};
export type Cluster = {
  namespace: string;
  name: string;
  uid: string;
  resourceVersion: string;
  tenantID: string;
  runtime: "Raw" | "Docker";
  targetAgentRelease: string;
  bindingValid: boolean;
  deleting: boolean;
  nodeCount: number;
  readyNodes: number;
  busyNodes: number;
  gpuCount: number;
  allocationCount: number;
  upgradeCount: number;
  conditions: Condition[] | null;
  lastAgentUpgrade?: {
    operation: { name: string };
    nodeRecord: { name: string };
    release: { name: string };
    completedAt: string;
  };
};
export type NativeResources = {
  name: string;
  namespace: string;
  generation?: number;
  phase: string;
  observedAt?: string;
  capacity: Record<string, string> | null;
  allocatable: Record<string, string> | null;
  used: Record<string, string> | null;
  nodeInfo: NonNullable<CRDNode["status"]>["nodeInfo"];
  addresses: Array<{ type: string; address: string }> | null;
  conditions: Condition[] | null;
};
export type Node = {
  resources?: NativeResources;
  resourceVersion: string;
  nativeUID: string;
  unschedulable: boolean;
  name: string;
  uid: string;
  instanceID: string;
  privateAddress: string;
  environment: string;
  networkScope: string;
  role: "Gateway" | "Worker" | "";
  agentVersion: string;
  ready: boolean;
  schedulable: boolean;
  busy: boolean;
  held: boolean;
  bindingValid: boolean;
  capacity: Record<string, string> | null;
  authorizedGPUUUIDs: string[] | null;
  discoveredGPUUUIDs: string[] | null;
  capabilities: string[] | null;
  lastHeartbeatTime: string | null;
  conditions: Condition[] | null;
};
export type Release = {
  name: string;
  uid: string;
  version: string;
  os: string;
  architecture: string;
  sha256: string;
  available: boolean;
};
export type Allocation = {
  name: string;
  uid: string;
  nodeName: string;
  jobName: string;
  taskName: string;
  taskUID: string;
  kind: string;
  phase: string;
  deleting: boolean;
  cleanupConfirmed: boolean;
};
export type StatsSnapshot = {
  sampledAt: string;
  cpuUsageNS?: string;
  memoryUsageBytes?: string;
  memoryLimitBytes?: string;
  pids?: string;
  blockReadBytes?: string;
  blockWriteBytes?: string;
};
export type LogSnapshot = {
  data: string;
  truncated: boolean;
  retentionLimitReached: boolean;
};
export type JobSummary = {
  name: string;
  uid: string;
  resourceVersion: string;
  bindingUID: string;
  operationID: string;
  phase: string;
  observed: boolean;
  suspended: boolean;
  activated: boolean;
  bindingValid: boolean;
};
export type RawJobRequest = {
  operationID: string;
  clusterUID: string;
  environment: string;
  process: { argv: string[]; env: string[]; directory: string };
};
export type Upgrade = {
  name: string;
  uid: string;
  nodeName: string;
  releaseName: string;
  phase: string;
  conditions: Condition[] | null;
};
export type NodeRegistration = {
  operationID: string;
  clusterUID: string;
  name: string;
  instanceID: string;
  privateAddress: string;
  environment: string;
  networkScope: string;
  role: "Gateway" | "Worker";
  authorizedGPUUUIDs: string[];
};
export type RegisteredNode = {
  namespace: string;
  name: string;
  uid: string;
  nativeUID: string;
  clusterUID: string;
  operationID: string;
};
export type EnrollmentTarget = {name: string; role: "Gateway" | "Worker"; address: string; port: number; nodeUID?: string; privateAddress?: string};
export type EnrollmentRequest = {operationID: string; clusterUID: string; release: string; nodes: EnrollmentTarget[]};
export type EnrollmentNode = {name: string; nodeUID?: string; reachability?: string; probedAt?: string; phase?: string; reason?: string; services?: string[]};
export type Enrollment = {name: string; uid: string; operationID: string; releaseName: string; releaseVersion: string; phase: string; cancelRequested?: boolean; targets: EnrollmentTarget[]; nodes: EnrollmentNode[] | null; conditions: Condition[] | null};
export type Detail = {
  enrollmentEnabled: boolean;
  enrollments: Enrollment[];
  nodeRegistrationEnabled: boolean;
  jobSubmissionEnabled: boolean;
  cluster: Cluster;
  nodes: Node[];
  releases: Release[];
  allocations: Allocation[];
  upgrades: Upgrade[];
};
export type ClusterIntent = {
  operationID: string;
  name: string;
  tenantID: string;
  agentNamespace: string;
  runtime: "Raw" | "Docker";
};
export class APIError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}
const record = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === "object" && !Array.isArray(value);
function strings(value: Record<string, unknown>, fields: string[]) {
  return fields.every((key) => typeof value[key] === "string");
}
function booleans(value: Record<string, unknown>, fields: string[]) {
  return fields.every((key) => typeof value[key] === "boolean");
}
function conditions(value: unknown): boolean {
  return (
    value === null ||
    (Array.isArray(value) &&
      value.every(
        (c) =>
          record(c) &&
          strings(c, ["type", "status"]) &&
          ["reason", "message"].every(
            (k) => c[k] === undefined || typeof c[k] === "string",
          ) &&
          (c.observedGeneration === undefined ||
            typeof c.observedGeneration === "number"),
      ))
  );
}
const uint64 = (value: unknown): value is string => {
  if (
    typeof value !== "string" ||
    value.length > 20 ||
    !/^(0|[1-9][0-9]*)$/.test(value)
  )
    return false;
  try {
    return BigInt(value) <= 18446744073709551615n;
  } catch {
    return false;
  }
};
function statsSnapshot(value: unknown): value is StatsSnapshot {
  if (
    !record(value) ||
    typeof value.sampledAt !== "string" ||
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(
      value.sampledAt,
    ) ||
    !Number.isFinite(Date.parse(value.sampledAt)) ||
    Date.parse(value.sampledAt) > Date.now() + 60000
  )
    return false;
  return [
    "cpuUsageNS",
    "memoryUsageBytes",
    "memoryLimitBytes",
    "pids",
    "blockReadBytes",
    "blockWriteBytes",
  ].every((k) => value[k] === undefined || uint64(value[k]));
}
function stringArray(value: unknown): boolean {
  return (
    value === null ||
    (Array.isArray(value) && value.every((v) => typeof v === "string"))
  );
}
function enrollment(value: unknown): value is Enrollment {
  return record(value) && strings(value, ["name", "uid", "operationID", "releaseName", "releaseVersion", "phase"]) && (value.cancelRequested===undefined || typeof value.cancelRequested==="boolean") && conditions(value.conditions) &&
    Array.isArray(value.targets) && value.targets.every(t => record(t) && strings(t,["name","role","address"]) && ["Gateway","Worker"].includes(String(t.role)) && typeof t.port === "number" && ["nodeUID","privateAddress"].every(k=>t[k]===undefined || typeof t[k]==="string")) &&
    (value.nodes === null || Array.isArray(value.nodes) && value.nodes.every(n => record(n) && strings(n,["name"]) && ["nodeUID","reachability","probedAt","phase","reason"].every(k=>n[k]===undefined || typeof n[k]==="string") && (n.services===undefined || stringArray(n.services))));
}
function jobSummary(value: unknown): value is JobSummary {
  return (
    record(value) &&
    strings(value, [
      "name",
      "uid",
      "resourceVersion",
      "bindingUID",
      "operationID",
      "phase",
    ]) &&
    booleans(value, ["observed", "suspended", "activated", "bindingValid"])
  );
}
function cluster(value: unknown): value is Cluster {
  return (
    record(value) &&
    strings(value, [
      "namespace",
      "name",
      "uid",
      "resourceVersion",
      "tenantID",
      "runtime",
      "targetAgentRelease",
    ]) &&
    (value.runtime === "Raw" || value.runtime === "Docker") &&
    (value.gpuAuthorizationPolicy === undefined || typeof value.gpuAuthorizationPolicy === "string") &&
    booleans(value, ["bindingValid", "deleting"]) &&
    conditions(value.conditions) &&
    [
      "nodeCount",
      "readyNodes",
      "busyNodes",
      "gpuCount",
      "allocationCount",
      "upgradeCount",
    ].every(
      (key) =>
        typeof value[key] === "number" &&
        Number.isSafeInteger(value[key]) &&
        (value[key] as number) >= 0,
    ) &&
    (value.lastAgentUpgrade === undefined ||
      (record(value.lastAgentUpgrade) &&
        strings(value.lastAgentUpgrade, ["completedAt"]) &&
        ["operation", "nodeRecord", "release"].every(
          (k) =>
            record(value.lastAgentUpgrade) &&
            record(value.lastAgentUpgrade[k]) &&
            strings(value.lastAgentUpgrade[k] as Record<string, unknown>, [
              "name",
            ]),
        )))
  );
}
function resourceMap(value: unknown) {
  return (
    value === null ||
    (record(value) && Object.values(value).every((v) => typeof v === "string"))
  );
}
function nativeResources(value: unknown): boolean {
  if (value === undefined) return true;
  return (
    record(value) &&
    strings(value, ["name", "namespace", "phase"]) &&
    (value.generation === undefined ||
      (typeof value.generation === "number" &&
        Number.isSafeInteger(value.generation) &&
        value.generation >= 0)) &&
    (value.observedAt === undefined ||
      (typeof value.observedAt === "string" &&
        Number.isFinite(Date.parse(value.observedAt)))) &&
    resourceMap(value.capacity) &&
    resourceMap(value.allocatable) &&
    resourceMap(value.used) &&
    (value.nodeInfo === null ||
      (record(value.nodeInfo) &&
        Object.values(value.nodeInfo).every((v) => typeof v === "string"))) &&
    (value.addresses === null ||
      (Array.isArray(value.addresses) &&
        value.addresses.every(
          (v) => record(v) && strings(v, ["type", "address"]),
        ))) &&
    conditions(value.conditions)
  );
}
// Adapt the validated extension DTO to the native read-only presentation.
export function nativeResourceNode(
  node: Node,
  runtime: "Raw" | "Docker",
): CRDNode | undefined {
  const r = node.resources;
  if (!r) return undefined;
  return {
    apiVersion: "rlinf.io/v1alpha1",
    kind: "Node",
    metadata: {
      name: r.name,
      namespace: r.namespace,
      generation: r.generation,
      annotations: r.observedAt
        ? { "containers.rlark.io/resources-observed-at": r.observedAt }
        : {},
    },
    spec: { agentType: runtime, unschedulable: node.unschedulable },
    status: {
      phase: r.phase,
      capacity: r.capacity ?? {},
      allocatable: r.allocatable ?? {},
      used: r.used ?? {},
      nodeInfo: r.nodeInfo ?? undefined,
      addresses: r.addresses ?? [],
      conditions: r.conditions ?? [],
    },
  };
}
function detail(value: unknown): value is Detail {
  return (
    record(value) &&
    typeof value.enrollmentEnabled === "boolean" && Array.isArray(value.enrollments) && value.enrollments.every(enrollment) &&
    typeof value.jobSubmissionEnabled === "boolean" &&
    typeof value.nodeRegistrationEnabled === "boolean" &&
    cluster(value.cluster) &&
    Array.isArray(value.nodes) &&
    value.nodes.every(
      (n) =>
        record(n) &&
        strings(n, [
          "name",
          "uid",
          "instanceID",
          "privateAddress",
          "environment",
          "networkScope",
          "role",
          "agentVersion",
          "resourceVersion",
          "nativeUID",
        ]) &&
        booleans(n, [
          "ready",
          "schedulable",
          "busy",
          "held",
          "bindingValid",
          "unschedulable",
        ]) &&
        nativeResources(n.resources) &&
        stringArray(n.authorizedGPUUUIDs) &&
        stringArray(n.discoveredGPUUUIDs) &&
        stringArray(n.capabilities) &&
        conditions(n.conditions) &&
        (n.lastHeartbeatTime === null ||
          (typeof n.lastHeartbeatTime === "string" &&
            Number.isFinite(Date.parse(n.lastHeartbeatTime)))) &&
        (n.capacity === null ||
          (record(n.capacity) &&
            Object.values(n.capacity).every((v) => typeof v === "string"))),
    ) &&
    Array.isArray(value.releases) &&
    value.releases.every(
      (r) =>
        record(r) &&
        strings(r, [
          "name",
          "uid",
          "version",
          "os",
          "architecture",
          "sha256",
        ]) &&
        booleans(r, ["available"]),
    ) &&
    Array.isArray(value.allocations) &&
    value.allocations.every(
      (a) =>
        record(a) &&
        strings(a, [
          "name",
          "uid",
          "nodeName",
          "jobName",
          "taskName",
          "taskUID",
          "kind",
          "phase",
        ]) &&
        booleans(a, ["deleting", "cleanupConfirmed"]),
    ) &&
    Array.isArray(value.upgrades) &&
    value.upgrades.every(
      (u) =>
        record(u) &&
        strings(u, ["name", "uid", "nodeName", "releaseName", "phase"]) &&
        conditions(u.conditions),
    )
  );
}
const malformed = () =>
  new APIError(
    502,
    "invalid_response",
    "Management response is incomplete or invalid",
  );
async function request(
  path: string,
  init: RequestInit = {},
  signal?: AbortSignal,
): Promise<unknown> {
  try {
    return await requestJson<unknown>(path, {
      ...init,
      signal,
      redirect: "error",
      cache: "no-store",
    });
  } catch (error) {
    if (error instanceof SharedApiError) {
      const body = error.body;
      throw new APIError(
        error.status,
        record(body) && typeof body.code === "string"
          ? body.code
          : "unavailable",
        error.message,
      );
    }
    if (error instanceof SyntaxError) throw malformed();
    throw error;
  }
}
export class Client {
  async cancelEnrollment(cluster: Cluster, operation: Enrollment, signal?: AbortSignal): Promise<Enrollment> {
    const body = await request(`${this.path(cluster.namespace,cluster.name)}/enrollments/${encodeURIComponent(operation.name)}/cancel`, {method:"POST",body:JSON.stringify({uid:operation.uid,clusterUID:cluster.uid})},signal);
    if (!enrollment(body) || body.uid!==operation.uid || body.operationID!==operation.operationID) throw malformed();
    return body;
  }
  async startNodes(cluster: Cluster, value: EnrollmentRequest, signal?: AbortSignal): Promise<Enrollment> {
    const body = await request(`${this.path(cluster.namespace,cluster.name)}/start`, {method:"POST",body:JSON.stringify(value)},signal);
    if (!enrollment(body) || body.operationID !== value.operationID) throw malformed();
    return body;
  }
  async list(signal?: AbortSignal): Promise<Cluster[]> {
    const body = await request(
      "/api/v1/container-runtime/clusters",
      {},
      signal,
    );
    if (
      !record(body) ||
      !Array.isArray(body.items) ||
      !body.items.every(cluster)
    )
      throw malformed();
    return body.items;
  }
  async detail(
    namespace: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<Detail> {
    const body = await request(this.path(namespace, name), {}, signal);
    // Older consoles omit this additive capability. Keep inventory available
    // during upgrades without enabling writes the server did not advertise.
    const normalized = record(body)
      ? {
          ...body,
          enrollmentEnabled: body.enrollmentEnabled === undefined ? false : body.enrollmentEnabled,
          enrollments: body.enrollments === undefined ? [] : body.enrollments,
          jobSubmissionEnabled:
            body.jobSubmissionEnabled === undefined
              ? false
              : body.jobSubmissionEnabled,
          nodeRegistrationEnabled:
            body.nodeRegistrationEnabled === undefined
              ? false
              : body.nodeRegistrationEnabled,
        }
      : body;
    if (!detail(normalized)) throw malformed();
    return normalized;
  }
  async registerNode(
    namespace: string,
    name: string,
    value: NodeRegistration,
    signal?: AbortSignal,
  ): Promise<RegisteredNode> {
    const body = await request(
      `${this.path(namespace, name)}/nodes`,
      { method: "POST", body: JSON.stringify(value) },
      signal,
    );
    if (
      !record(body) ||
      !strings(body, [
        "namespace",
        "name",
        "uid",
        "nativeUID",
        "clusterUID",
        "operationID",
      ]) ||
      !body.uid ||
      !body.nativeUID ||
      body.namespace !== namespace ||
      body.name !== value.name ||
      body.clusterUID !== value.clusterUID ||
      body.operationID !== value.operationID
    )
      throw malformed();
    return body as RegisteredNode;
  }
  async target(
    cluster: Cluster,
    targetAgentRelease: string,
    releaseUID: string,
    signal?: AbortSignal,
  ): Promise<void> {
    await request(
      this.path(cluster.namespace, cluster.name),
      {
        method: "PATCH",
        body: JSON.stringify({
          uid: cluster.uid,
          resourceVersion: cluster.resourceVersion,
          targetAgentRelease,
          releaseUID,
        }),
      },
      signal,
    );
  }
  async nodeScheduling(
    cluster: Cluster,
    node: Node,
    unschedulable: boolean,
    signal?: AbortSignal,
  ): Promise<void> {
    await request(
      `${this.path(cluster.namespace, cluster.name)}/nodes/${encodeURIComponent(node.name)}`,
      {
        method: "PATCH",
        body: JSON.stringify({
          uid: node.uid,
          resourceVersion: node.resourceVersion,
          unschedulable,
        }),
      },
      signal,
    );
  }
  async logs(
    namespace: string,
    allocation: Allocation,
    signal?: AbortSignal,
  ): Promise<LogSnapshot> {
    const query = new URLSearchParams({
      namespace,
      allocation: allocation.name,
      allocationUID: allocation.uid,
      taskUID: allocation.taskUID,
    });
    const body = await request(
      `/api/v1/container-runtime/logs?${query}`,
      {},
      signal,
    );
    if (
      !record(body) ||
      typeof body.data !== "string" ||
      typeof body.truncated !== "boolean" ||
      typeof body.retentionLimitReached !== "boolean"
    )
      throw malformed();
    return body as LogSnapshot;
  }
  async stats(
    namespace: string,
    allocation: Allocation,
    signal?: AbortSignal,
  ): Promise<StatsSnapshot> {
    const query = new URLSearchParams({
      namespace,
      allocation: allocation.name,
      allocationUID: allocation.uid,
      taskUID: allocation.taskUID,
    });
    const body = await request(
      `/api/v1/container-runtime/stats?${query}`,
      {},
      signal,
    );
    if (!statsSnapshot(body)) throw malformed();
    return body;
  }
  async jobs(
    namespace: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<JobSummary[]> {
    const body = await request(
      `/api/v1/container-runtime/clusters/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/jobs`,
      {},
      signal,
    );
    if (
      !record(body) ||
      !Array.isArray(body.items) ||
      !body.items.every(jobSummary)
    )
      throw malformed();
    return body.items;
  }
  async submitJob(
    namespace: string,
    name: string,
    value: RawJobRequest,
    signal?: AbortSignal,
  ): Promise<JobSummary> {
    const body = await request(
      `/api/v1/container-runtime/clusters/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/jobs`,
      { method: "POST", body: JSON.stringify(value) },
      signal,
    );
    if (!jobSummary(body)) throw malformed();
    return body;
  }
  async suspendJob(
    namespace: string,
    cluster: string,
    job: JobSummary,
    signal?: AbortSignal,
  ): Promise<JobSummary> {
    const body = await request(
      `/api/v1/container-runtime/clusters/${encodeURIComponent(namespace)}/${encodeURIComponent(cluster)}/jobs/${encodeURIComponent(job.name)}/suspend`,
      {
        method: "PATCH",
        body: JSON.stringify({
          uid: job.uid,
          resourceVersion: job.resourceVersion,
        }),
      },
      signal,
    );
    if (!jobSummary(body)) throw malformed();
    return body;
  }
  async create(
    intent: ClusterIntent,
    signal?: AbortSignal,
  ): Promise<{ namespace: string; name: string }> {
    const body = await request(
      "/api/v1/container-runtime/clusters",
      { method: "POST", body: JSON.stringify(intent) },
      signal,
    );
    if (!record(body) || !strings(body, ["namespace", "name", "uid"]))
      throw malformed();
    return { namespace: body.namespace as string, name: body.name as string };
  }
  private path(namespace: string, name: string) {
    return `/api/v1/container-runtime/clusters/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
  }
}
