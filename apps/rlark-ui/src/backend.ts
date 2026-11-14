import { request, requestJson, withQuery, type LoginResponse } from "./api.js";
import type { IssuedNodeAdmission, NodeAdmission, SiteNode } from "./utils/siteNodeDisplay.js";
import type {
  AgentCertListItem,
  CRDDomain,
  CRDNode,
  CRDPod,
  SignAgentCertResponse,
} from "./types.js";
import type { CRDJob, CRDTask, CRDWorkflow } from "./crdModels.js";

type ItemList<T> = { items?: T[] };
type DataResponse<T> = { data?: T };
const crdRoot = "/api/v1/rlinf.io/v1alpha1";
const resourcePath = (resource: string, name?: string) =>
  `${crdRoot}/${resource}${name ? `/${encodeURIComponent(name)}` : ""}`;

export interface SystemConfigResponse {
  ssh?: { jumpHost?: string; jumpPort?: string };
  sshJumpHost?: string;
  sshJumpPort?: string;
  log?: {
    backend?: string;
    config?: {
      endpoint?: string;
      project?: string;
      logstore?: string;
      accessKeyId?: string;
      accessKeySecret?: string;
    };
  };
  deployment?: DeploymentConfig;
}

export interface DeploymentConfig {
  apiVersion?: string;
  kind?: string;
  plane?: "data";
  controlPlaneAddress?: string;
  sshAddress?: string;
  insecureSkipTlsVerify?: boolean;
  kubernetes?: {
    kubeconfig?: string;
    agentImage?: string;
    image?: string;
    imagePullPolicy?: "" | "Always" | "IfNotPresent" | "Never";
    imagePullSecrets?: string[];
    containerdSocket?: string;
  };
}

let systemConfigCache: SystemConfigResponse | undefined;
let systemConfigRequest: Promise<SystemConfigResponse> | undefined;

export interface SSHKeyItem {
  index: number;
  user: string;
  public_key: string;
  added_at: string;
}

export interface ImageRegistryItem {
  id: string;
  name: string;
  registry: string;
  username: string;
  clusterSelection: {
    mode: "None" | "Selected" | "All";
    clusters: string[];
  };
}

/** One tag with image metadata read by the Gateway; `error` marks unreadable metadata. */
export interface RegistryTagDetail {
  tag: string;
  digest?: string;
  created?: string;
  size?: number;
  platforms?: string[];
  error?: "unavailable";
}

export interface RegistryTagDetailPage {
  items: RegistryTagDetail[];
  next: string;
  /** Set by the console when the Gateway lacks `tag-details` and only tag names are known. */
  legacy?: boolean;
}

export interface StationItem {
  apiVersion: string;
  kind: "Station";
  metadata: {
    name: string;
    namespace: string;
    uid?: string;
    creationTimestamp?: string;
  };
  spec: {
    displayName: string;
    sequence: number;
    retired?: boolean;
    binding: {
      nodeName: string;
      nodeUID: string;
      systemUUID?: string;
      version: number;
      boundBy: string;
    };
  };
  status?: {
    phase?: "Bound" | "Offline" | "Conflict" | "Retired";
    bootTime?: string;
    lastSeenTime?: string;
    conditions?: Array<{
      type: string;
      status: "True" | "False" | "Unknown";
      reason?: string;
      message?: string;
      lastTransitionTime?: string;
    }>;
  };
}

export interface StationPageResponse {
  items: StationItem[];
  continue?: string;
}

export interface StationObservationItem {
  apiVersion: string;
  kind: "StationObservation";
  metadata: { name: string; namespace: string };
  status: {
    nodeUID: string;
    reportedAt: string;
    systemUUID?: string;
    bootTime?: string;
  };
}

export interface StationObservationPageResponse {
  items: StationObservationItem[];
  continue?: string;
}

export interface StationRebindOperation {
  apiVersion: string;
  kind: "StationRebind";
  metadata: { name: string; namespace: string; uid?: string };
  spec: {
    requestID: string;
    stationRef: { name: string; uid: string };
    expectedBindingVersion: number;
    targetNodeName: string;
    targetNodeUID: string;
    reason: string;
    requestedBy: string;
  };
  status?: {
    phase?: "Accepted" | "Applied" | "Rejected";
    conditions?: Array<{ type: string; status: string; reason?: string; message?: string }>;
  };
}

export interface StationRebindResponse {
  operation: StationRebindOperation;
  created: boolean;
}

export interface NodeOperation {
  apiVersion: string;
  kind: "NodeOperation";
  metadata: { name: string; namespace: string; uid?: string; creationTimestamp?: string };
  spec: {
    requestID: string;
    action: "Join" | "Remove" | "EnableWireGuard";
    clusterID: string;
    nodeName: string;
    nodeUID?: string;
    resetExistingKubeadm?: boolean;
    containerdRuntime?: "containerd" | "fleet";
    requestedBy: string;
  };
  status?: {
    phase?: "Pending" | "Running" | "Blocked" | "Succeeded" | "Failed";
    stage?: string;
    nodeUID?: string;
    conditions?: Array<{ type: string; status: string; reason?: string; message?: string }>;
  };
}

export interface NodeOperationPageResponse {
  items: NodeOperation[];
  continue?: string;
}

export interface LocalizedText {
  zh: string;
  en: string;
}

export interface ApiReferenceEndpoint {
  method: string;
  path: string;
  description: LocalizedText;
  example: unknown;
}

export interface ApiReferenceSection {
  id: string;
  title: LocalizedText;
  description: LocalizedText;
  endpoints: ApiReferenceEndpoint[] | null;
}

export interface ApiReferenceResponse {
  title: LocalizedText;
  description: LocalizedText;
  sections: ApiReferenceSection[];
}

export const authApi = {
  login(username: string, password: string) {
    return requestJson<LoginResponse>("/api/v1/auth/login", {
      method: "POST",
      body: { username, password },
    });
  },
};

export const apiReferenceApi = {
  get() {
    return requestJson<ApiReferenceResponse>("/api/v1/api-reference");
  },
};

export const clustersApi = {
  async list<T = unknown>() {
    const response = await requestJson<DataResponse<T[]>>("/api/v1/clusters");
    return response.data ?? [];
  },
  async get<T = unknown>(id: string) {
    const response = await requestJson<DataResponse<T>>(
      `/api/v1/clusters/${encodeURIComponent(id)}`,
    );
    return response.data;
  },
  lifecycle(
    id: string,
    action: "cordon" | "uncordon" | "drain" | "undrain" | "delist" | "restore",
  ) {
    return requestJson<DataResponse<unknown>>(
      `/api/v1/clusters/${encodeURIComponent(id)}/${action}`,
      { method: "POST" },
    );
  },
  remove(id: string) {
    return requestJson<DataResponse<unknown>>(
      `/api/v1/clusters/${encodeURIComponent(id)}/delete`,
      { method: "POST" },
    );
  },
};

function resourceApi<T>(resource: string) {
  return {
    async list(query: Record<string, string | undefined> = {}) {
      const response = await requestJson<ItemList<T>>(
        withQuery(resourcePath(resource), query),
      );
      return response.items ?? [];
    },
    get(name: string, query: Record<string, string | undefined> = {}) {
      return requestJson<T>(withQuery(resourcePath(resource, name), query));
    },
    create(body: object) {
      return requestJson<T>(resourcePath(resource), { method: "POST", body });
    },
    replace(name: string, body: object) {
      return requestJson<T>(resourcePath(resource, name), {
        method: "PUT",
        body,
      });
    },
    patch(name: string, body: object, namespace?: string) {
      return requestJson<T>(
        withQuery(resourcePath(resource, name), { namespace }),
        { method: "PATCH", body },
      );
    },
    remove(name: string) {
      return request(resourcePath(resource, name), { method: "DELETE" });
    },
  };
}

export const nodesApi = resourceApi<CRDNode>("nodes");
export const tasksApi = resourceApi<CRDTask>("tasks");
export const podsApi = {
  ...resourceApi<CRDPod>("pods"),
  events<T = unknown>(name: string) {
    return requestJson<T>(`${resourcePath("pods", name)}/events`);
  },
};
export const domainsApi = resourceApi<CRDDomain>("domains");
export const workflowsApi = {
  ...resourceApi<CRDWorkflow>("workflows"),
  setStopped(name: string, stopped: boolean) {
    return request(resourcePath("workflows", name), {
      method: "PATCH",
      body: { spec: { suspended: stopped } },
    });
  },
};

export const jobsApi = {
  ...resourceApi<CRDJob>("jobs"),
  async listTags<T = { key: string; values: string[] }>() {
    const response = await requestJson<ItemList<T>>(
      `${resourcePath("jobs")}/tags`,
    );
    return response.items ?? [];
  },
  setStopped(name: string, stopped: boolean) {
    // 停止：PATCH spec.suspended=true
    // 启动：POST /start（统一接口，活跃或归档都走这里）
    if (stopped) {
      return request(resourcePath("jobs", name), {
        method: "PATCH",
        body: { spec: { suspended: true } },
      });
    }
    return request(`${resourcePath("jobs", name)}/start`, {
      method: "POST",
    });
  },
  logs<T>(name: string, query: Record<string, string | undefined>) {
    return requestJson<T>(
      withQuery(`${resourcePath("jobs", name)}/logs`, query),
    );
  },
  logLabelValues<T>(name: string, query: Record<string, string | undefined>) {
    return requestJson<T>(
      withQuery(`${resourcePath("jobs", name)}/logs/label-values`, query),
    );
  },
};

export const systemConfigApi = {
  get(options: { refresh?: boolean } = {}) {
    if (!options.refresh && systemConfigCache) {
      return Promise.resolve(systemConfigCache);
    }
    if (!options.refresh && systemConfigRequest) return systemConfigRequest;

    const pending = requestJson<SystemConfigResponse>("/api/v1/system-config")
      .then((config) => {
        systemConfigCache = config;
        return config;
      })
      .finally(() => {
        if (systemConfigRequest === pending) systemConfigRequest = undefined;
      });
    systemConfigRequest = pending;
    return pending;
  },
  async update(body: object) {
    const config = await requestJson<SystemConfigResponse>(
      "/api/v1/system-config",
      { method: "PUT", body },
    );
    systemConfigCache = config;
    return config;
  },
};

export const sshKeysApi = {
  list(signal?: AbortSignal) {
    return requestJson<SSHKeyItem[]>("/api/v1/ssh-user-keys", { signal });
  },
  create(user: string, publicKey: string) {
    return request("/api/v1/ssh-user-keys", {
      method: "POST",
      body: { user, public_key: publicKey },
    });
  },
  remove(user: string, index: number) {
    return request(withQuery(`/api/v1/ssh-user-keys/${index}`, { user }), {
      method: "DELETE",
    });
  },
};

export const certificatesApi = {
  list() {
    return requestJson<AgentCertListItem[]>("/api/v1/certificates/agent");
  },
  sign(clusterId: string) {
    return requestJson<SignAgentCertResponse>("/api/v1/certificates/agent", {
      method: "POST",
      body: { cluster_id: clusterId },
    });
  },
  get(clusterId: string) {
    return requestJson<SignAgentCertResponse>(
      `/api/v1/certificates/agent/${encodeURIComponent(clusterId)}`,
    );
  },
};

export const imagesApi = {
  async list<T = unknown>() {
    const response = await requestJson<ItemList<T>>("/api/v1/images");
    return response.items ?? [];
  },
};

export const imageRegistriesApi = {
  password(id: string, signal?: AbortSignal) {
    return requestJson<{ password: string }>(`/api/v1/image-registries/${encodeURIComponent(id)}/password`, { signal });
  },
  list(signal?: AbortSignal) {
    return requestJson<ImageRegistryItem[]>("/api/v1/image-registries", { signal });
  },
  create(body: object) {
    return request("/api/v1/image-registries", { method: "POST", body });
  },
  update(id: string, body: object) {
    return request(`/api/v1/image-registries/${encodeURIComponent(id)}`, {
      method: "PUT",
      body,
    });
  },
  remove(id: string) {
    return request(`/api/v1/image-registries/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  },
  repositories(id: string, last = "", signal?: AbortSignal) {
    const query = new URLSearchParams(last ? { last } : {});
    return requestJson<{ items: string[]; next: string }>(
      `/api/v1/image-registries/${encodeURIComponent(id)}/repositories?${query}`,
      { signal },
    );
  },
  tags(id: string, repository: string, last = "", signal?: AbortSignal) {
    const query = new URLSearchParams({
      repository,
      ...(last ? { last } : {}),
    });
    return requestJson<{ items: string[]; next: string }>(
      `/api/v1/image-registries/${encodeURIComponent(id)}/tags?${query}`,
      { signal },
    );
  },
  tagDetails(id: string, repository: string, last = "", signal?: AbortSignal) {
    const query = new URLSearchParams({
      repository,
      ...(last ? { last } : {}),
    });
    return requestJson<RegistryTagDetailPage>(
      `/api/v1/image-registries/${encodeURIComponent(id)}/tag-details?${query}`,
      { signal },
    );
  },
  manifest(id: string, repository: string, tag: string, signal?: AbortSignal) {
    const query = new URLSearchParams({ repository, tag });
    return requestJson<{ digest: string }>(
      `/api/v1/image-registries/${encodeURIComponent(id)}/manifest?${query}`,
      { signal },
    );
  },
};

export const stationsApi = {
  list(query: Record<string, string | undefined> = {}, signal?: AbortSignal) {
    return requestJson<StationPageResponse>(
      withQuery("/api/v1/station/stations", query),
      { signal },
    );
  },
  get(name: string, clusterID: string, signal?: AbortSignal) {
    const query = new URLSearchParams({ cluster_id: clusterID });
    return requestJson<StationItem>(
      `/api/v1/station/stations/${encodeURIComponent(name)}?${query}`,
      { signal },
    );
  },
  observations(clusterID: string, signal?: AbortSignal) {
    const query = new URLSearchParams({ cluster_id: clusterID, limit: "500" });
    return requestJson<StationObservationPageResponse>(
      `/api/v1/station/observations?${query}`,
      { signal },
    );
  },
  createRebind(input: {
    cluster_id: string;
    request_id: string;
    station_name: string;
    station_uid: string;
    expected_binding_version: number;
    target_node_name: string;
    target_node_uid: string;
    target_system_uuid?: string;
    reason: string;
  }) {
    return requestJson<StationRebindResponse>("/api/v1/station/rebinds", {
      method: "POST",
      body: input,
    });
  },
  getRebind(name: string, clusterID: string, signal?: AbortSignal) {
    const query = new URLSearchParams({ cluster_id: clusterID });
    return requestJson<StationRebindOperation>(
      `/api/v1/station/rebinds/${encodeURIComponent(name)}?${query}`,
      { signal },
    );
  },
  createNodeOperation(input: {
    cluster_id: string;
    request_id: string;
    action: "Join" | "Remove";
    node_name: string;
    node_uid?: string;
    reset_existing_kubeadm?: boolean;
    containerd_runtime?: "containerd" | "fleet";
    ssh_address: string;
    ssh_port: number;
    ssh_user: string;
    host_key_fingerprint: string;
    ssh_password?: string;
    ssh_private_key?: string;
    sudo_password?: string;
  }) {
    return requestJson<NodeOperation>("/api/v1/station/node-operations", {
      method: "POST",
      body: input,
    });
  },
  listNodeOperations(clusterID: string, signal?: AbortSignal) {
    const query = new URLSearchParams({ cluster_id: clusterID, limit: "100" });
    return requestJson<NodeOperationPageResponse>(
      `/api/v1/station/node-operations?${query}`,
      { signal },
    );
  },
  getNodeOperation(name: string, clusterID: string, signal?: AbortSignal) {
    const query = new URLSearchParams({ cluster_id: clusterID });
    return requestJson<NodeOperation>(
      `/api/v1/station/node-operations/${encodeURIComponent(name)}?${query}`,
      { signal },
    );
  },
};

export const siteNodesApi = {
  list(clusterID: string, signal?: AbortSignal) {
    return requestJson<{ items: SiteNode[] }>(
      withQuery("/api/v1/site-nodes", { cluster_id: clusterID }),
      { signal },
    );
  },
};

// Join codes. The code is in the answer of issue and reissue only; the
// management plane keeps its hash.
export const nodeAdmissionsApi = {
  list(clusterID: string, signal?: AbortSignal) {
    return requestJson<{ items: NodeAdmission[] }>(
      withQuery("/api/v1/station/node-admissions", { cluster_id: clusterID }),
      { signal },
    );
  },
  issue(input: {
    cluster_id: string;
    node_name: string;
    api_server: string;
    site?: string;
    address?: string;
    uplink?: string;
    labels?: Record<string, string>;
    taints?: string[];
    ttl_seconds?: number;
  }) {
    return requestJson<IssuedNodeAdmission>("/api/v1/station/node-admissions", {
      method: "POST",
      body: input,
    });
  },
  reissue(name: string, clusterID: string, ttlSeconds?: number) {
    return requestJson<IssuedNodeAdmission>(
      withQuery(`/api/v1/station/node-admissions/${encodeURIComponent(name)}/reissue`, { cluster_id: clusterID }),
      { method: "POST", body: ttlSeconds ? { ttl_seconds: ttlSeconds } : {} },
    );
  },
  revoke(name: string, clusterID: string) {
    return requestJson<{ admission: NodeAdmission }>(
      withQuery(`/api/v1/station/node-admissions/${encodeURIComponent(name)}/revoke`, { cluster_id: clusterID }),
      { method: "POST" },
    );
  },
};

export const storageClassesApi = {
  async list<T = unknown>(cluster?: string) {
    const response = await requestJson<DataResponse<Record<string, T>>>(
      withQuery("/api/v1/storage/storageclass", { clusters: cluster }),
    );
    return response.data ?? {};
  },
  // providers 返回后端维护的提供商目录；value 是提交给后端的合法取值，
  // name 仅用于展示。前端不再硬编码该列表。
  async providers() {
    const response = await requestJson<
      DataResponse<Array<{ name: string; value: string }>>
    >("/api/v1/storage/storageclass/provider");
    return response.data ?? [];
  },
  create(body: object) {
    return request("/api/v1/storage/storageclass", { method: "POST", body });
  },
  update(name: string, body: object) {
    return request(`/api/v1/storage/storageclass/${encodeURIComponent(name)}`, {
      method: "PUT",
      body,
    });
  },
  remove(name: string) {
    return request(`/api/v1/storage/storageclass/${encodeURIComponent(name)}`, {
      method: "DELETE",
    });
  },
};

const storageObjectPath = (storageClass: string) =>
  `/api/v1/storage/storageclass/${encodeURIComponent(storageClass)}`;

export const storageObjectsApi = {
  list<T>(storageClass: string, prefix: string) {
    return requestJson<T>(
      withQuery(`${storageObjectPath(storageClass)}/list`, {
        prefix,
        maxKeys: 100,
      }),
    );
  },
  upload(storageClass: string, formData: FormData) {
    return request(`${storageObjectPath(storageClass)}/upload`, {
      method: "POST",
      body: formData,
    });
  },
  download<T>(storageClass: string, key: string) {
    return requestJson<T>(
      withQuery(
        `${storageObjectPath(storageClass)}/object/${encodeURIComponent(key)}`,
        { expire: 3600 },
      ),
    );
  },
  remove(storageClass: string, key: string) {
    return request(
      `${storageObjectPath(storageClass)}/object/${encodeURIComponent(key)}`,
      { method: "DELETE" },
    );
  },
};

export const addonsApi = {
  async catalog<T = unknown>() {
    const response = await requestJson<DataResponse<T[]>>("/api/v1/addons");
    return response.data ?? [];
  },
  async installed<T = unknown>(cluster?: string) {
    const response = await requestJson<DataResponse<T[]>>(
      withQuery("/api/v1/installed-addons", { cluster }),
    );
    return response.data ?? [];
  },
  install(clusterId: string, body: object) {
    return request(`/api/v1/clusters/${encodeURIComponent(clusterId)}/addons`, {
      method: "POST",
      body,
    });
  },
  update(clusterId: string, name: string, body: object) {
    return request(
      `/api/v1/clusters/${encodeURIComponent(clusterId)}/addons/${encodeURIComponent(name)}`,
      { method: "PUT", body },
    );
  },
  remove(clusterId: string, name: string) {
    return request(
      `/api/v1/clusters/${encodeURIComponent(clusterId)}/addons/${encodeURIComponent(name)}`,
      { method: "DELETE" },
    );
  },
};

export const terminalApi = {
  createSocket(podName: string) {
    const protocol = location.protocol === "https:" ? "wss:" : "ws:";
    return new WebSocket(
      `${protocol}//${location.host}${resourcePath("pods", podName)}/terminal`,
    );
  },
};
