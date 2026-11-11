import { useEffect, useMemo, useState } from "react";
import { requestJson, withQuery } from "../api";
import { clustersApi } from "../backend";
import type { ClusterSummary, CRDNode } from "../types";
import type { NodeNetworkDisplay } from "../utils/nodeNetwork";

type Summary = {
  state: "current" | "stale" | "unreported";
  interface?: string;
  addresses?: string[];
  reportedAt?: string;
};

export function useNodeNetworks(nodes: CRDNode[], enabled: boolean) {
  const [networks, setNetworks] = useState<Record<string, NodeNetworkDisplay>>(
    {},
  );
  const keys = useMemo(
    () =>
      JSON.stringify(
        nodes
          .map((node) => [node.metadata.namespace ?? "", node.metadata.name])
          .sort(),
      ),
    [nodes],
  );
  useEffect(() => {
    if (!enabled) return;
    const targets = JSON.parse(keys) as Array<[string, string]>;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const controller = new AbortController();
    async function refresh() {
      const next: Record<string, NodeNetworkDisplay> = {};
      try {
        const clusters = await clustersApi.list<ClusterSummary>();
        const mapping = new Map<string, string>();
        const ambiguous = new Set<string>();
        for (const cluster of clusters) {
          const ns = cluster.agentNamespace;
          if (!ns || cluster.agentNamespaceConflict || mapping.has(ns)) {
            if (ns) ambiguous.add(ns);
            continue;
          }
          mapping.set(ns, cluster.id);
        }
        let cursor = 0;
        const worker = async () => {
          while (!stopped && cursor < targets.length) {
            const [namespace, name] = targets[cursor++];
            const key = `${namespace}/${name}`;
            const clusterID = mapping.get(namespace);
            if (!clusterID || ambiguous.has(namespace)) {
              next[key] = {
                wireguard: { state: "unavailable", interfaces: [] },
              };
              continue;
            }
            try {
              const summary = await requestJson<Summary>(
                withQuery(`/api/v1/node-networks/${encodeURIComponent(name)}`, {
                  cluster_id: clusterID,
                }),
                { signal: controller.signal },
              );
              if (summary.state !== "unreported")
                next[key] = {
                  wireguard: {
                    state: summary.state,
                    interfaces: summary.interface
                      ? [
                          {
                            name: summary.interface,
                            addresses: summary.addresses ?? [],
                          },
                        ]
                      : [],
                    reportedAt: summary.reportedAt,
                  },
                };
            } catch {
              next[key] = {
                wireguard: { state: "unavailable", interfaces: [] },
              };
            }
          }
        };
        await Promise.all(
          Array.from({ length: Math.min(6, targets.length) }, worker),
        );
      } catch {
        for (const [namespace, name] of targets)
          next[`${namespace}/${name}`] = {
            wireguard: { state: "unavailable", interfaces: [] },
          };
      }
      if (!stopped) {
        setNetworks(next);
        timer = setTimeout(refresh, 30000);
      }
    }
    setNetworks({});
    void refresh();
    return () => {
      stopped = true;
      controller.abort();
      if (timer) clearTimeout(timer);
    };
  }, [keys, enabled]);
  return networks;
}
