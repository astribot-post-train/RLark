import type { Cluster, Node } from "./api";
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
