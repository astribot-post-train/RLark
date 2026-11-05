// Route registration for the container runtime admin console. Kept free of
// UI dependencies so upstream src/utils/route.ts (compiled into node tests)
// can import it with a single spread line.
export const containerRuntimePage = "container-clusters";

export const containerRuntimePages = [containerRuntimePage];

// Namespace and name remain separate URL segments for direct links and history.
export function containerClusterPath(namespace: string, name: string) {
  return `/admin/${containerRuntimePage}/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
}
