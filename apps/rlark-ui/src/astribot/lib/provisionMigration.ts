export interface MigrationIdentity {
  uid: string;
  resource_version: string;
}

export type MigrationCandidateKey =
  | "cluster"
  | "site-network"
  | "station-read"
  | "station-operations"
  | "release"
  | "agent-certificate";

export interface MigrationCandidate {
  key: MigrationCandidateKey;
  name: string;
  namespace?: string;
  created_at?: string;
  state: "review" | "recorded" | "absent" | "blocked";
  identity?: MigrationIdentity;
}

export interface MigrationPreview {
  provision_uid: string;
  provision_resource_version: string;
  candidates: MigrationCandidate[];
  native_state: "review" | "recorded" | "blocked";
}

export interface MigrationConfirmation {
  provision_uid: string;
  provision_resource_version: string;
  evidence: string;
  cluster?: MigrationIdentity;
  site_network?: MigrationIdentity;
  credentials: Partial<Record<MigrationCandidateKey, MigrationIdentity>>;
  native_cluster_uid?: string;
}

export function migrationConfirmation(
  preview: MigrationPreview,
  selected: ReadonlySet<MigrationCandidateKey>,
  evidence: string,
  nativeUID?: string,
): MigrationConfirmation | undefined {
  const reference = evidence.trim();
  if (!reference || reference.length > 256) return undefined;
  const cluster = preview.candidates.find((item) => item.key === "cluster");
  if (
    !cluster ||
    cluster.state === "blocked" ||
    (cluster.state === "review" &&
      (!cluster.identity || !selected.has("cluster")))
  ) {
    return undefined;
  }
  const result: MigrationConfirmation = {
    provision_uid: preview.provision_uid,
    provision_resource_version: preview.provision_resource_version,
    evidence: reference,
    credentials: {},
  };
  if (cluster.state === "recorded" || cluster.state === "review") {
    if (!cluster.identity) return undefined;
    result.cluster = cluster.identity;
  }
  if (nativeUID && preview.native_state === "review")
    result.native_cluster_uid = nativeUID;

  let reviewed =
    cluster.state === "review" || Boolean(result.native_cluster_uid);
  for (const item of preview.candidates) {
    if (
      item.key === "cluster" ||
      item.state !== "review" ||
      !item.identity ||
      !selected.has(item.key)
    )
      continue;
    reviewed = true;
    if (item.key === "site-network") result.site_network = item.identity;
    else result.credentials[item.key] = item.identity;
  }
  return reviewed ? result : undefined;
}
