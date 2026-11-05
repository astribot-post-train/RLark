import type {
  Detail,
  EnrollmentNode,
  EnrollmentRequest,
  EnrollmentTarget,
  Node,
} from "./api";

export function enrollmentTargets(detail: Detail): EnrollmentTarget[] {
  const latest = new Map<string, EnrollmentTarget>();
  for (const op of detail.enrollments) {
    for (const target of op.targets) {
      if (!latest.has(target.name)) latest.set(target.name, { ...target });
    }
  }
  for (const node of detail.nodes) {
    const target = latest.get(node.name);
    if (target && (!target.nodeUID || target.nodeUID === node.uid))
      target.nodeUID = node.uid;
    else if (!target)
      latest.set(node.name, {
        name: node.name,
        nodeUID: node.uid,
        address: node.privateAddress,
        role: node.role || "Worker",
        port: 22,
      });
  }
  return latest.size
    ? [...latest.values()]
    : [{ name: "node-a", address: "", role: "Gateway", port: 22 }];
}

export function enrollmentIntent(
  operationID: string,
  clusterUID: string,
  release: string,
  nodes: EnrollmentTarget[],
): EnrollmentRequest {
  const ipv4 = (address: string) =>
    /^(\d{1,3}\.){3}\d{1,3}$/.test(address) &&
    address.split(".").every((v) => Number(v) <= 255);
  if (
    !nodes.length ||
    nodes.length > 64 ||
    nodes.filter((n) => n.role === "Gateway").length !== 1 ||
    new Set(nodes.map((n) => n.name)).size !== nodes.length ||
    new Set(nodes.map((n) => n.address)).size !== nodes.length ||
    nodes.some(
      (n) =>
        !n.name ||
        !/^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$/.test(n.name) ||
        !ipv4(n.address) ||
        !Number.isInteger(n.port) ||
        n.port < 1 ||
        n.port > 65535,
    )
  )
    throw new Error(
      "Invalid node topology / 节点名称、IP、端口和网关角色配置无效",
    );
  return {
    operationID,
    clusterUID,
    release,
    nodes: nodes.map((n) => ({ ...n })),
  };
}

export function parseRetainedEnrollment(
  serialized: string | null,
  clusterUID: string,
): EnrollmentRequest | undefined {
  try {
    const value: unknown = JSON.parse(serialized || "null");
    if (
      value &&
      typeof value === "object" &&
      "clusterUID" in value &&
      value.clusterUID === clusterUID &&
      "nodes" in value &&
      Array.isArray(value.nodes) &&
      "operationID" in value &&
      typeof value.operationID === "string" &&
      "release" in value &&
      typeof value.release === "string"
    ) {
      return enrollmentIntent(
        value.operationID,
        clusterUID,
        value.release,
        value.nodes as EnrollmentTarget[],
      );
    }
  } catch {
    return undefined;
  }
  return undefined;
}

export function nodeProbe(
  detail: Detail,
  node: Node,
): EnrollmentNode | undefined {
  for (const op of detail.enrollments) {
    const status = op.nodes?.find(
      (n) => n.name === node.name && n.nodeUID === node.uid,
    );
    if (status) return status;
  }
}

export function probeState(
  probe: EnrollmentNode | undefined,
  now = Date.now(),
): string {
  const at = Date.parse(probe?.probedAt ?? "");
  return !Number.isFinite(at) || at > now || now - at > 60000
    ? "Unknown"
    : probe?.reachability || "Unknown";
}
