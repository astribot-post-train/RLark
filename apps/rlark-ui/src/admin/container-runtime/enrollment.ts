import type {
  Detail,
  Enrollment,
  EnrollmentNode,
  EnrollmentRequest,
  EnrollmentTarget,
  Node,
} from "./api";

export const terminalEnrollmentPhases = ["Succeeded", "Failed", "Cancelled"];

// Enrollment operations still in progress (not terminal, not cancelling).
export function activeEnrollments(detail: Detail): number {
  return detail.enrollments.filter(
    (op) => !terminalEnrollmentPhases.includes(op.phase) && !op.cancelRequested,
  ).length;
}

// The console API lists a cluster's enrollments newest first, while the
// controller runs them oldest first (later ones wait with
// EarlierOperationPending). The running operation is therefore the oldest
// unfinished one; newer unfinished operations are queued behind it.
export function currentEnrollment(
  detail: Detail,
): { active?: Enrollment; queued: Enrollment[] } {
  const open = detail.enrollments.filter(
    (op) => !terminalEnrollmentPhases.includes(op.phase) && !op.cancelRequested,
  );
  return { active: open[open.length - 1], queued: open.slice(0, -1) };
}

// Newest finished operation when it failed, so the form can explain why the
// last submission did not join every node. Returns undefined while an
// operation is still running or when the newest one did not fail.
export function lastFailedEnrollment(detail: Detail): Enrollment | undefined {
  const newest = detail.enrollments.find((op) => !op.cancelRequested);
  return newest?.phase === "Failed" ? newest : undefined;
}

// Operator-facing explanation for enrollment reasons. Unknown reasons are
// returned verbatim so new controller reasons stay visible.
const enrollmentReasons: Record<string, { zh: string; en: string }> = {
  SSHUnavailable: {
    zh: "A 到该节点的 SSH 不可达：请确认实例已启动、sshd 在运行、安全组放行该端口，且 A 与该地址网络互通",
    en: "SSH from A to this node is unreachable: check the instance is running, sshd is up, the security group allows the port, and A can route to the address",
  },
  SSHAddressDenied: {
    zh: "SSH 地址不在控制器允许的网段内（接入配置的 allowedGatewayCIDRs / allowedWorkerCIDRs）；需管理员放行该地址后重新提交",
    en: "The SSH address is outside the controller's allowed ranges (allowedGatewayCIDRs / allowedWorkerCIDRs in the enrollment profile); an administrator must allow it before resubmitting",
  },
  // Completed operations only report these; a new submission replaces the
  // node's old record automatically.
  PrivateAddressChanged: {
    zh: "节点私网地址已变化（例如重启），重新提交接入即可自动替换旧记录",
    en: "The node's private address changed (for example after a restart); submit again to replace the old record",
  },
  NotebookReplaced: {
    zh: "节点已是新的实例（例如重启），重新提交接入即可自动替换旧记录",
    en: "The node is a new instance (for example after a restart); submit again to replace the old record",
  },
  ReplacementBlocked: {
    zh: "该节点的旧记录仍有任务或升级在占用，结束后会自动完成替换并继续接入",
    en: "The node's old record is still held by a task or an upgrade; replacement continues automatically once it is released",
  },
  EarlierOperationPending: {
    zh: "排在更早的接入操作之后，等待其结束",
    en: "Queued behind an earlier enrollment operation",
  },
  ExistingAgentNotReady: {
    zh: "已有 Agent 尚未就绪",
    en: "The existing Agent is not ready",
  },
  PackageUnavailable: {
    zh: "Agent 安装包不可用",
    en: "The Agent package is unavailable",
  },
  ServiceStartFailed: { zh: "服务启动失败", en: "A service failed to start" },
  NodeUnavailable: { zh: "节点不可用", en: "The node is unavailable" },
  ReleaseChanged: {
    zh: "固定的 Agent 版本已变化",
    en: "The pinned Agent release changed",
  },
};

export function enrollmentReasonText(
  reason: string | undefined,
  zh: boolean,
): string {
  if (!reason) return "";
  const label = enrollmentReasons[reason];
  return label ? (zh ? label.zh : label.en) : reason;
}

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
    // Identity comes from the controller's probe, so a restarted node rejoins
    // by name with only its SSH endpoint. A Gateway keeps its record UID so the
    // controller can reuse an already running gateway instead of reinstalling.
    nodes: nodes.map(({ name, role, address, port, nodeUID }) =>
      role === "Gateway" && nodeUID
        ? { name, role, address, port, nodeUID }
        : { name, role, address, port },
    ),
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
