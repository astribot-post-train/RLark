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
export type Node = {
  resourceVersion: string;
  nativeUID: string;
  unschedulable: boolean;
  name: string;
  uid: string;
  instanceID: string;
  privateAddress: string;
  environment: string;
  networkScope: string;
  agentVersion: string;
  ready: boolean;
  schedulable: boolean;
  busy: boolean;
  held: boolean;
  bindingValid: boolean;
  capacity: Record<string, string> | null;
  authorizedGPUUUIDs: string[] | null;
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
  kind: string;
  phase: string;
  deleting: boolean;
  cleanupConfirmed: boolean;
};
export type Upgrade = {
  name: string;
  uid: string;
  nodeName: string;
  releaseName: string;
  phase: string;
  conditions: Condition[] | null;
};
export type Detail = {
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
function stringArray(value: unknown): boolean {
  return (
    value === null ||
    (Array.isArray(value) && value.every((v) => typeof v === "string"))
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
function detail(value: unknown): value is Detail {
  return (
    record(value) &&
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
        stringArray(n.authorizedGPUUUIDs) &&
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
  token: string,
  init: RequestInit = {},
  signal?: AbortSignal,
): Promise<unknown> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (init.body) headers["Content-Type"] = "application/json";
  const response = await fetch(path, {
    ...init,
    credentials: "omit",
    headers,
    signal,
    redirect: "error",
    cache: "no-store",
  });
  let body: unknown;
  try {
    body = await response.json();
  } catch {
    if (response.ok) throw malformed();
  }
  if (!response.ok) {
    const code =
      record(body) && typeof body.code === "string" ? body.code : "unavailable";
    const message =
      record(body) && typeof body.message === "string"
        ? body.message
        : "Management request failed";
    throw new APIError(response.status, code, message);
  }
  return body;
}
export async function login(
  username: string,
  password: string,
  signal?: AbortSignal,
): Promise<string> {
  const body = await request(
    "/api/v1/auth/login",
    "",
    { method: "POST", body: JSON.stringify({ username, password }) },
    signal,
  );
  if (
    !record(body) ||
    body.ok !== true ||
    body.role !== "admin" ||
    typeof body.token !== "string" ||
    !body.token
  )
    throw malformed();
  return body.token;
}
export class Client {
  constructor(private readonly token: string) {}
  async list(signal?: AbortSignal): Promise<Cluster[]> {
    const body = await request(
      "/api/v1/container-runtime/clusters",
      this.token,
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
    const body = await request(
      this.path(namespace, name),
      this.token,
      {},
      signal,
    );
    if (!detail(body)) throw malformed();
    return body;
  }
  async target(
    cluster: Cluster,
    targetAgentRelease: string,
    releaseUID: string,
    signal?: AbortSignal,
  ): Promise<void> {
    await request(
      this.path(cluster.namespace, cluster.name),
      this.token,
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
      this.token,
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
  async create(
    intent: ClusterIntent,
    signal?: AbortSignal,
  ): Promise<{ namespace: string; name: string }> {
    const body = await request(
      "/api/v1/container-runtime/clusters",
      this.token,
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
