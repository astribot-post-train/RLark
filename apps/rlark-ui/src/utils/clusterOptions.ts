export type RegistryClusterOption = { id: string; name: string };

// Registry delivery uses the historical namespace suffix, never a display name.
export function registryClusterOption(cluster: {
  id?: string;
  name?: string;
}): RegistryClusterOption {
  const identity = cluster.id || cluster.name || "";
  return {
    id: identity.replace(/^rlark-/, ""),
    name: cluster.name || identity,
  };
}
