import type { CRDNode } from "../../types";
import { formatChinaDateTime } from "../../utils/time.js";

// What the node pages show of a node's network: its WireGuard addresses as
// station-agent last reported them (GET /api/v1/node-networks/{name};
// absent when nothing was reported, unavailable when they cannot be read),
// and its site-network view from GET /api/v1/site-nodes (absent for a node
// that is not in the site network).
export interface NodeNetworkDisplay {
  wireguard?: {
    state: "current" | "stale" | "unavailable";
    interface?: string;
    addresses?: string[];
    reportedAt?: string;
  };
  site?: SiteNodeLinks;
}

// What each Hub a Leaf dials sees of it (NetworkEnrollment status.links):
// Unanswered = the Leaf's requests reach the Hub and no session forms,
// Silent = nothing reaches the Hub, Unknown = the Hub has no recent report.
export interface SiteNodeLink {
  hub: string;
  state: "Connected" | "Unanswered" | "Silent" | "Unknown";
  since: string;
  latest_handshake?: string;
}

export interface SiteNodeLinks {
  online: boolean;
  links: SiteNodeLink[];
  // Empty while every link is Connected.
  summary: string;
  // The node's own record of its tunnel; absent for a hub and before its first report.
  self?: NodeSelfLink;
}

// The node's own record of its tunnel (site-nodes link, the Node annotation rlark.io/link: its station-gateway's
// link.json as last reported once Station API answered). Times are Unix seconds of the node's clock.
export interface NodeSelfLink {
  state: "up" | "down";
  since: number;
  // What restarts the machine when its tunnel stays down; absent: a person does.
  restartOwner?: string;
  restarts: number;
  restart?: { id: number; at: number; why: string };
  // The last 10; step is down, recovered, uplink, wireguard, restart, unowned or rlark.
  actions: Array<{ at: number; step: string; result: string }>;
}

// down: a Hub hears the node badly or not at all, or the node's heartbeat
// stopped; unknown: no Hub has a recent report; connected otherwise.
export function nodeLinkHealth(site: SiteNodeLinks): "connected" | "down" | "unknown" {
  if (!site.online || site.links.some((link) => link.state === "Unanswered" || link.state === "Silent"))
    return "down";
  if (site.links.some((link) => link.state === "Unknown")) return "unknown";
  return "connected";
}

export function nodeNetworkKey(node: CRDNode): string {
  return `${node.metadata.namespace ?? ""}/${node.metadata.name}`;
}

export function nodeNetworkModel(node: CRDNode, network?: NodeNetworkDisplay) {
  const primary =
    node.status?.addresses?.find((item) => item.type === "InternalIP")?.address ?? node.status?.addresses?.[0]?.address;
  const addresses: Array<{ kind: "node" | "wg"; address: string; interface?: string; cidr?: string }> = primary
    ? [{ kind: "node", address: primary }]
    : [];
  const wg = network?.wireguard;
  if (wg && wg.state !== "unavailable") {
    for (const cidr of new Set((wg.addresses ?? []).map((raw) => raw.trim()))) {
      const address = cidr.split("/")[0];
      if (address) addresses.push({ kind: "wg", address, interface: wg.interface, cidr });
    }
  }
  const message: "unreported" | "stale" | "unavailable" | null =
    wg?.state === "unavailable" || wg?.state === "stale"
      ? wg.state
      : addresses.some((entry) => entry.kind === "wg")
        ? null
        : "unreported";
  return { addresses, message, reportedAt: wg?.reportedAt };
}

export function nodeNetworkSearchText(node: CRDNode, network?: NodeNetworkDisplay): string {
  return nodeNetworkModel(node, network)
    .addresses.map((entry) => `${entry.address} ${entry.cidr ?? ""}`)
    .join(" ");
}

// One short line for a list row: the worst state first, the Hubs it names.
export function nodeLinkLine(site: SiteNodeLinks, zh: boolean): string {
  const hubs = (state: SiteNodeLink["state"]) =>
    site.links.filter((link) => link.state === state).map((link) => link.hub.split("/")[1] ?? link.hub).join(zh ? "、" : ", ");
  if (!site.online) return zh ? "心跳中断" : "Heartbeat lost";
  const silent = hubs("Silent");
  if (silent) return zh ? `${silent} 收不到它的包` : `Nothing arrives at ${silent}`;
  const unanswered = hubs("Unanswered");
  if (unanswered) return zh ? `请求到了 ${unanswered}，回包回不来` : `Requests reach ${unanswered}, answers do not get back`;
  const unknown = hubs("Unknown");
  if (unknown) return zh ? `${unknown} 没有近况` : `No current report from ${unknown}`;
  if (!site.links.length) return zh ? "在线" : "Online";
  return zh ? `${site.links.length} 个 Hub 都已连通` : `Connected to all ${site.links.length} hubs`;
}

// Station API alone writes the record, as typed JSON.
export function nodeSelfLink(raw?: string): NodeSelfLink | undefined {
  return raw ? (JSON.parse(raw) as NodeSelfLink) : undefined;
}

const selfSteps: Record<string, [string, string]> = {
  down: ["断网", "down"],
  recovered: ["恢复", "recovered"],
  uplink: ["重新拉起网卡", "uplink taken up again"],
  wireguard: ["重建 WireGuard", "WireGuard made anew"],
  restart: ["重启", "restart"],
  unowned: ["需人工重启", "needs a person to restart"],
  rlark: ["Station API", "Station API"],
};

export function nodeSelfStep(step: string, zh: boolean): string {
  return selfSteps[step]?.[zh ? 0 : 1] ?? step;
}

// MM-DD HH:MM in China time.
export function nodeSelfTime(at: number): string {
  return formatChinaDateTime(new Date(at * 1000).toISOString()).slice(5, 16);
}

// The hubs hear the node down and nothing on it owns its restart (by the owner it last reported): a person restarts it.
export function nodeNeedsPerson(site: SiteNodeLinks): boolean {
  return !!site.self && !site.self.restartOwner && nodeLinkHealth(site) === "down";
}

// One short line of what the node did of itself: down since when and its last step, or when it recovered and after
// how long; "" for nothing to say. The record reaches RLark only once the node is back, so whether a person must
// restart it now comes from the hubs' links and the owner it last reported; a past outage nothing restarted is marked.
export function nodeSelfLine(site: SiteNodeLinks, zh: boolean): string {
  const link = site.self;
  if (!link) return "";
  const person = nodeNeedsPerson(site) ? nodeSelfStep("unowned", zh) : "";
  const newest = [...link.actions].reverse();
  if (!newest.length) return person;
  const lastDown = newest.findIndex((action) => action.step === "down");
  const unowned = newest.slice(0, lastDown < 0 ? undefined : lastDown).some((action) => action.step === "unowned");
  const mark = person ? ` · ${person}` : unowned ? (zh ? " · 未自动重启" : " · not restarted automatically") : "";
  if (link.state === "down")
    return `${zh ? "断网自 " : "Down since "}${nodeSelfTime(link.since)} · ${zh ? "最近：" : "last: "}${nodeSelfStep(newest[0].step, zh)}${mark}`;
  const recovered = newest.find((action) => action.step === "recovered");
  if (!recovered) return `${zh ? "通 自 " : "Up since "}${nodeSelfTime(link.since)}`;
  const down = newest.find((action) => action.step === "down" && action.at <= recovered.at);
  const minutes = down ? Math.max(1, Math.round((recovered.at - down.at) / 60)) : undefined;
  const after = minutes === undefined ? "" : zh ? `（断 ${minutes} 分钟）` : ` after ${minutes} min`;
  return `${zh ? `${nodeSelfTime(recovered.at)} 恢复` : `Recovered ${nodeSelfTime(recovered.at)}`}${after}${mark}`;
}
