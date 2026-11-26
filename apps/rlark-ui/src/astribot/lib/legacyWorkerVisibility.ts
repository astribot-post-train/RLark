import type { CRDNode } from "../../types";

const CONTROL_PLANE_ROLE_LABELS = [
  "node-role.kubernetes.io/control-plane",
  "node-role.kubernetes.io/master",
];

export function isControlPlaneLabels(labels: Record<string, string> | undefined): boolean {
  if (!labels) return false;
  const kubernetesRole = labels["kubernetes.io/role"];
  return (
    CONTROL_PLANE_ROLE_LABELS.some((label) => label in labels) ||
    kubernetesRole === "master" ||
    kubernetesRole === "control-plane"
  );
}

export function isLegacyBusinessWorkerNode(node: CRDNode, hasKnownCategory: boolean): boolean {
  const labels = node.metadata.labels ?? {};
  const isControlPlane = isControlPlaneLabels(labels);
  const resources = {
    ...(node.status?.capacity ?? {}),
    ...(node.status?.allocatable ?? {}),
  };
  const hasWorkloadDevice = Object.entries(resources).some(
    ([name, value]) =>
      Number.parseFloat(value) > 0 &&
      (/(^|[./-])gpu($|[./-])/i.test(name) ||
        name === "rlinf.io/device" ||
        name.startsWith("rlinf.io/device-")),
  );
  return (
    (!isControlPlane || labels["rlark.io/enabled"] === "true") &&
    (hasKnownCategory ||
      hasWorkloadDevice)
  );
}
