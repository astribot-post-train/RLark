import type { Cluster, Node } from "./api";
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
