// Fields the Raw resource check reads. They are optional on purpose: an
// upstream CRDNode is assignable without the shared type declaring Raw-only
// fields such as metadata.generation or status.conditions.
export type RawObservedNode = {
  metadata: {
    generation?: number;
    annotations?: Record<string, string>;
  };
  spec?: { unschedulable?: boolean };
  status?: {
    phase?: string;
    conditions?: Array<{
      type: string;
      status: string;
      reason?: string;
      observedGeneration?: number;
    }>;
  };
};

// Match the extension's server-observed resource freshness bound. Old agents
// without an observation timestamp must never imply free resources.
export function rawResourceState(node: RawObservedNode, now = Date.now()) {
  const observedAt =
    node.metadata.annotations?.["containers.rlark.io/resources-observed-at"];
  const observed = Date.parse(observedAt ?? "");
  const conditions = node.status?.conditions ?? [];
  const resources = conditions.find((c) => c.type === "RawResourcesReady");
  const scheduling = conditions.find((c) => c.type === "RawSchedulingReady");
  const current =
    Number.isFinite(observed) &&
    observed <= now &&
    now - observed <= 45000 &&
    resources?.status === "True" &&
    (node.metadata.generation === undefined ||
      resources.observedGeneration === node.metadata.generation);
  const schedulable =
    current &&
    scheduling?.status === "True" &&
    (node.metadata.generation === undefined ||
      scheduling.observedGeneration === node.metadata.generation) &&
    node.status?.phase === "Online" &&
    !node.spec?.unschedulable;
  return {
    current,
    schedulable,
    observedAt,
    reason: !current
      ? "ResourceObservationUnavailable"
      : (scheduling?.reason ?? "SchedulingObservationUnavailable"),
  };
}
