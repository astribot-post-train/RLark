import type {
  Cluster,
  Node,
  RawJobRequest,
  NodeRegistration,
  Detail,
  Allocation,
} from "./api";

// Explicit arrays preserve whitespace and empty arguments without shell parsing
// or inheriting the browser/Agent environment. Server validation is authoritative.
export function rawJobIntent(
  operationID: string,
  clusterUID: string,
  environment: string,
  argvText: string,
  envText: string,
  directory: string,
): RawJobRequest {
  const argv: unknown = JSON.parse(argvText);
  const env: unknown = JSON.parse(envText);
  const canonical = (s: string) =>
    s.startsWith("/") &&
    !s.includes("\0") &&
    (s === "/" ||
      (!s.endsWith("/") &&
        s
          .split("/")
          .slice(1)
          .every((p) => p !== "" && p !== "." && p !== "..")));
  if (
    !environment ||
    environment.trim() !== environment ||
    environment.length > 128 ||
    !Array.isArray(argv) ||
    !argv.length ||
    !argv.every((v) => typeof v === "string" && !v.includes("\0")) ||
    !canonical(argv[0]) ||
    !canonical(directory) ||
    !Array.isArray(env) ||
    !env.every((v) => typeof v === "string" && !v.includes("\0"))
  ) {
    throw new Error("Invalid Raw process");
  }
  const keys = new Set<string>();
  for (const entry of env) {
    const key = entry.slice(0, entry.indexOf("="));
    if (
      !/^[A-Za-z_][A-Za-z0-9_]*=/.test(entry) ||
      keys.has(key) ||
      key === "CUDA_VISIBLE_DEVICES" ||
      key === "NVIDIA_VISIBLE_DEVICES"
    ) {
      throw new Error("Invalid Raw environment");
    }
    keys.add(key);
  }
  const value = {
    operationID,
    clusterUID,
    environment,
    process: { argv, env, directory },
  };
  if (new TextEncoder().encode(JSON.stringify(value)).length > 65536)
    throw new Error("Raw process exceeds request limit");
  return value;
}
// Logs are opaque bytes on the wire. Decode only bounded snapshots and replace
// terminal/bidirectional controls before React renders the result as text.
export function logText(data: string): string {
  if (data.length > 87384) throw new Error("Log snapshot exceeds 64 KiB");
  const bytes = Uint8Array.from(atob(data), (v) => v.charCodeAt(0));
  if (bytes.length > 65536) throw new Error("Log snapshot exceeds 64 KiB");
  return new TextDecoder()
    .decode(bytes)
    .replace(
      /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/g,
      "�",
    );
}
export function nodeState(node: Node): "ready" | "stale" | "held" | "invalid" {
  if (!node.bindingValid) return "invalid";
  if (node.held) return "held";
  if (!node.ready) return "stale";
  return "ready";
}
export function clusterState(
  cluster: Cluster,
): "ready" | "degraded" | "invalid" | "deleting" {
  if (cluster.deleting) return "deleting";
  if (!cluster.bindingValid) return "invalid";
  if (cluster.readyNodes === 0) return "degraded";
  return "ready";
}
export function heartbeatAge(value: string | null, now = Date.now()): string {
  const parsed = value ? Date.parse(value) : NaN;
  if (!Number.isFinite(parsed)) return "—";
  return `${Math.round(Math.max(0, now - parsed) / 1000)}s`;
}
// A disposed/older read may never publish data after selection, refresh or logout.
export class LoadGate {
  private generation = 0;
  begin(): () => boolean {
    const generation = ++this.generation;
    return () => generation === this.generation;
  }
  cancel(): void {
    this.generation++;
  }
}

// Registration authorizes existing hardware. Never infer GPU entitlement from
// an empty list or silently change incarnation/private network identity.
export function nodeRegistrationIntent(
  operationID: string,
  clusterUID: string,
  fields: {
    name: string;
    instanceID: string;
    privateAddress: string;
    environment: string;
    networkScope: string;
    gpuText: string;
  },
): NodeRegistration {
  const authorizedGPUUUIDs: unknown = JSON.parse(fields.gpuText);
  const octets = fields.privateAddress.split(".");
  const privateIPv4 =
    octets.length === 4 &&
    octets.every((v) => /^(0|[1-9][0-9]{0,2})$/.test(v) && Number(v) <= 255) &&
    (octets[0] === "10" ||
      (octets[0] === "172" &&
        Number(octets[1]) >= 16 &&
        Number(octets[1]) <= 31) ||
      (octets[0] === "192" && octets[1] === "168"));
  if (
    !operationID ||
    !clusterUID ||
    fields.name.length > 253 ||
    !fields.name
      .split(".")
      .every((v) => /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(v)) ||
    !fields.instanceID.trim() ||
    !fields.environment.trim() ||
    !fields.networkScope.trim() ||
    !privateIPv4 ||
    !Array.isArray(authorizedGPUUUIDs) ||
    !authorizedGPUUUIDs.length ||
    !authorizedGPUUUIDs.every(
      (v) => typeof v === "string" && v.startsWith("GPU-") && !/[\s,]/.test(v),
    ) ||
    new Set(authorizedGPUUUIDs).size !== authorizedGPUUUIDs.length
  )
    throw new Error("Invalid node registration");
  const { gpuText: _gpuText, ...identity } = fields;
  const value = {
    operationID,
    clusterUID,
    ...identity,
    authorizedGPUUUIDs: [...authorizedGPUUUIDs].sort(),
  };
  if (new TextEncoder().encode(JSON.stringify(value)).length > 65536)
    throw new Error("Node registration exceeds request limit");
  return value;
}

// UI visibility is advisory; the API independently revalidates each read.
export function canReadDiagnostic(
  detail: Detail,
  allocation: Allocation,
): boolean {
  if (!allocation.taskUID || allocation.deleting) return false;
  if (detail.cluster.runtime === "Raw") return true;
  const node = detail.nodes.find((n) => n.name === allocation.nodeName);
  return (
    !allocation.cleanupConfirmed &&
    !!node?.capabilities?.includes("docker-diagnostics-v1") &&
    !!node?.capabilities?.includes("docker-container-execution-v1")
  );
}
// A changed identity/capability set fences both displayed and in-flight results.
export function diagnosticInventory(detail: Detail | undefined): string {
  if (!detail) return "";
  return JSON.stringify([
    detail.cluster.uid,
    detail.cluster.runtime,
    detail.allocations
      .filter((a) => canReadDiagnostic(detail, a))
      .map((a) => [
        a.uid,
        a.taskUID,
        a.nodeName,
        detail.nodes.find((n) => n.name === a.nodeName)?.uid ?? "",
        detail.nodes.find((n) => n.name === a.nodeName)?.instanceID ?? "",
      ])
      .sort((a, b) => a[0].localeCompare(b[0])),
  ]);
}
