// GET /api/v1/site-nodes?cluster_id=: one machine's identity, heartbeat and
// site-network membership, joined by the Gateway.
export interface SiteNode {
  cluster_id: string;
  node_name: string;
  site_node_id: string;
  phase: string;
  online: boolean;
  tunnel: "verified" | "failed" | "unknown";
  rlark_node: string;
  last_seen_time?: string;
  boot_time?: string;
  reboots: number;
  labels?: Record<string, string>;
  robot_id?: string;
  network?: {
    state: string;
    site?: string;
    role?: string;
    address?: string;
    tunnel_address?: string;
    public_key?: string;
    link_summary?: string;
  };
}

// Expired is also a code withdrawn before it was used.
export type NodeAdmissionState = "Pending" | "Redeemed" | "Joined" | "Expired";

// GET /api/v1/node-admissions?cluster_id=: never carries a code.
export interface NodeAdmission {
  cluster_id: string;
  node_name: string;
  state: NodeAdmissionState;
  reason?: string;
  api_server: string;
  site?: string;
  address?: string;
  uplink?: string;
  labels?: Record<string, string>;
  taints?: string[];
  expires_at: string;
  issued_by?: string;
  issued_at?: string;
  redeemed_at?: string;
  public_key?: string;
  tunnel_address?: string;
}

// The one answer that carries the code and the machine's command.
export interface IssuedNodeAdmission {
  admission: NodeAdmission;
  code: string;
  command: string;
}

// A WireGuard public key is public; the table shows its beginning, which is
// what `wg show <interface> public-key` on the machine starts with.
export function keyFingerprint(key?: string): string {
  return key ? `${key.slice(0, 10)}…` : "";
}

// The machine's kind, from the labels it was admitted with.
export function machineModel(labels?: Record<string, string>): string {
  if (!labels) return "";
  return labels["rlark.io/robot-model"] ?? labels["rlark.io/node-category"] ?? "";
}

export function admissionStateClass(state: NodeAdmissionState): string {
  return ({ Pending: "status-pending", Redeemed: "status-pending", Joined: "status-online", Expired: "status-offline" } as const)[state];
}

export function admissionStateLabel(state: NodeAdmissionState, zh: boolean): string {
  if (!zh) return state;
  return ({ Pending: "待使用", Redeemed: "入网中", Joined: "已入网", Expired: "已过期或作废" } as const)[state];
}

// One "key=value" per line; blank lines are skipped.
export function parseLabelLines(text: string): { labels: Record<string, string>; error?: string } {
  const labels: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    const at = line.indexOf("=");
    if (at <= 0) return { labels, error: line };
    labels[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  return { labels };
}

// One "key[=value]:Effect" per line.
export function parseTaintLines(text: string): { taints: string[]; error?: string } {
  const taints: string[] = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    if (!/^[^:=\s]+(=[^:\s]*)?:(NoSchedule|PreferNoSchedule|NoExecute)$/.test(line)) return { taints, error: line };
    taints.push(line);
  }
  return { taints };
}

// What a robot of the S1U build is admitted with; the operator fills the robot ID.
export const s1uRobotLabels = (robotID: string) => [
  "rlark.io/node-category=robot",
  "rlark.io/robot-model=s1u",
  "rlark.io/robot-body=s1u",
  `rlark.io/robot-id=${robotID}`,
].join("\n");
export const robotTaint = "rlark.io/node-category=robot:NoSchedule";
