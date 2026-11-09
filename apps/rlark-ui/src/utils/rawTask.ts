/**
 * Raw Task generation for container-runtime clusters.
 *
 * Raw tasks run processes in container environments (not Kubernetes Pods) and use
 * the raw.rlark.io/process workload type. The contract is defined in
 * extensions/container-runtime/pkg/taskcontract/raw.go.
 */

export type RawRoleConfig = {
  environment: string;
  argv: string[];
  env: string[];
  directory: string;
  nodeSelector: string;
  replicas: number;
  prepareScript?: string;
};

/**
 * Validates that a path is canonical: absolute, normalized, no special components.
 * This matches the validation in admin/container-runtime/models.ts.
 */
function isCanonicalPath(s: string): boolean {
  return (
    s.startsWith("/") &&
    !s.includes("\0") &&
    (s === "/" ||
      (!s.endsWith("/") &&
        s
          .split("/")
          .slice(1)
          .every((p) => p !== "" && p !== "." && p !== "..")))
  );
}

/**
 * Generates a Raw Task from role configuration.
 * Returns null and an error message if validation fails.
 */
export function rawTaskFromRole(
  roleName: string,
  config: RawRoleConfig,
): { task: unknown; error?: string } | { task: null; error: string } {
  // Validation matching rawJobIntent in admin/container-runtime/models.ts
  if (!config.environment || config.environment.trim() !== config.environment) {
    return { task: null, error: "Environment name cannot be empty or have leading/trailing whitespace" };
  }
  if (config.environment.length > 128) {
    return { task: null, error: "Environment name is too long (max 128 characters)" };
  }
  if (!Array.isArray(config.argv) || config.argv.length === 0) {
    return { task: null, error: "Process argv cannot be empty" };
  }
  if (!config.argv.every((v) => typeof v === "string" && !v.includes("\0"))) {
    return { task: null, error: "Process argv contains invalid arguments" };
  }
  if (!isCanonicalPath(config.argv[0])) {
    return { task: null, error: "Process argv[0] must be an absolute canonical path" };
  }
  if (!isCanonicalPath(config.directory)) {
    return { task: null, error: "Working directory must be an absolute canonical path" };
  }
  if (!Array.isArray(config.env) || !config.env.every((v) => typeof v === "string" && !v.includes("\0"))) {
    return { task: null, error: "Environment variables contain invalid entries" };
  }

  const parseNodeSelector = (s: string): Record<string, string> => {
    const result: Record<string, string> = {};
    for (const pair of s.split(",")) {
      const trimmed = pair.trim();
      if (!trimmed) continue;
      const [key, value] = trimmed.split("=", 2);
      if (key && value !== undefined) {
        result[key.trim()] = value.trim();
      }
    }
    return result;
  };

  const task = {
    name: roleName,
    role: "Actor",
    placement: {
      nodeSelector: parseNodeSelector(config.nodeSelector),
      environment: config.environment,
    },
    runtime: { kind: "Raw" },
    workload: {
      kind: "raw.rlark.io/process",
      version: "v1",
      carrier: {
        type: "raw.rlark.io/process",
        version: "v1",
      },
      parameters: {
        argv: config.argv,
        env: config.env,
        directory: config.directory,
      },
    },
    replicas: config.replicas,
  };

  return { task };
}
