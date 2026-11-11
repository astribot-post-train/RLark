import type { CRDNode } from "../types";

// The backend establishes node identity and observation freshness.
export interface NodeNetworkDisplay {
  accessMode?: "direct" | "gateway" | "unknown";
  wireguard?: {
    interfaces: Array<{ name: string; addresses: string[] }>;
    reportedAt?: string;
    state: "current" | "stale" | "unavailable";
  };
}

export interface NodeNetworkAddress {
  kind: "node" | "wg";
  address: string;
  interface?: string;
  cidr?: string;
}

export function nodeNetworkKey(node: CRDNode): string {
  return `${node.metadata.namespace ?? ""}/${node.metadata.name}`;
}

export function nodeNetworkModel(
  node: CRDNode,
  network?: NodeNetworkDisplay,
): {
  addresses: NodeNetworkAddress[];
  message: "unreported" | "gateway" | "stale" | "unavailable" | null;
  reportedAt?: string;
} {
  const primary =
    node.status?.addresses?.find((item) => item.type === "InternalIP")
      ?.address ?? node.status?.addresses?.[0]?.address;
  const addresses: NodeNetworkAddress[] = primary
    ? [{ kind: "node", address: primary }]
    : [];
  const wg = network?.wireguard;
  const seen = new Set<string>();
  if (wg && wg.state !== "unavailable") {
    for (const iface of wg.interfaces) {
      for (const raw of iface.addresses) {
        const cidr = raw.trim();
        const address = cidr.split("/")[0];
        const key = `${iface.name}/${address}`;
        if (!address || seen.has(key)) continue;
        seen.add(key);
        addresses.push({ kind: "wg", address, interface: iface.name, cidr });
      }
    }
  }
  const hasWG = addresses.some((entry) => entry.kind === "wg");
  const message =
    wg?.state === "unavailable"
      ? "unavailable"
      : wg?.state === "stale"
        ? "stale"
        : hasWG
          ? null
          : network?.accessMode === "gateway"
            ? "gateway"
            : "unreported";
  return { addresses, message, reportedAt: wg?.reportedAt };
}

export function nodeNetworkSearchText(
  node: CRDNode,
  network?: NodeNetworkDisplay,
): string {
  return nodeNetworkModel(node, network)
    .addresses.map((entry) => `${entry.address} ${entry.cidr ?? ""}`)
    .join(" ");
}
