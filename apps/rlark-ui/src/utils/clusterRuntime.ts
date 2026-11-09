/**
 * Cluster runtime detection for the job creation form.
 *
 * RLark supports both Kubernetes clusters (native Ray workloads with container images)
 * and Raw container-runtime clusters (process workloads with environments). The job
 * creation form needs to adapt its UI and Task generation based on which runtime the
 * selected cluster uses.
 */

import type { CRDNodeLite } from "../types";

export type ClusterRuntimeInfo = {
  runtime: "Kubernetes" | "Raw";
  agentNamespace?: string;
};

/**
 * Returns nodes that belong to the given cluster, respecting runtime-specific matching.
 *
 * - Kubernetes clusters: nodes in `namespace === clusterId`
 * - Raw clusters: nodes in `agentNamespace` from the runtime map
 */
export function nodesForCluster(
  allNodes: CRDNodeLite[],
  clusterId: string,
  runtimeMap: Map<string, ClusterRuntimeInfo>,
): CRDNodeLite[] {
  const info = runtimeMap.get(clusterId);
  if (!info) {
    // Fallback: treat as Kubernetes if not in the map
    return allNodes.filter((n) => n.metadata.namespace === clusterId);
  }
  if (info.runtime === "Raw" && info.agentNamespace) {
    return allNodes.filter((n) => n.metadata.namespace === info.agentNamespace);
  }
  return allNodes.filter((n) => n.metadata.namespace === clusterId);
}

/**
 * Builds the runtime map from the container-runtime cluster list
 * (GET /api/v1/container-runtime/clusters). Each item's `name` is the RLark
 * cluster ID and its `namespace` is where that cluster's Node objects live.
 */
export function runtimeMapFromContainerClusters(
  clusters: Array<{ name: string; namespace: string; runtime: string }>,
): Map<string, ClusterRuntimeInfo> {
  const map = new Map<string, ClusterRuntimeInfo>();
  for (const cluster of clusters) {
    if (cluster.runtime !== "Raw") continue;
    map.set(cluster.name, { runtime: "Raw", agentNamespace: cluster.namespace });
  }
  return map;
}

export function isRawCluster(
  clusterId: string,
  runtimeMap: Map<string, ClusterRuntimeInfo>,
): boolean {
  return runtimeMap.get(clusterId)?.runtime === "Raw";
}
