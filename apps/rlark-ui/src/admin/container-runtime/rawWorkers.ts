// Raw (container-runtime) Tasks run as supervised processes, not Pods, so the
// Job detail page's Pod-based Worker list never shows them. This derives one
// row per Raw Task from the Task itself: the node comes from the controller's
// allocation target annotation, which is scheduling intent pinned to exactly
// one node, and the phase from the Task status.

export const allocationTargetAnnotation =
  "containers.rlark.io/allocation-target";

/** The subset of a native Task this module reads. */
export interface RawTaskLike {
  metadata?: {
    name?: string;
    namespace?: string;
    annotations?: Record<string, string>;
  };
  spec?: { runtime?: { kind?: string } };
  status?: { phase?: string; message?: string };
}

export interface RawWorkerRow {
  taskName: string;
  namespace: string;
  node: string;
  phase: string;
  message?: string;
}

/** Node name from the allocation target, or undefined when absent or malformed. */
export function allocationTargetNode(task: RawTaskLike): string | undefined {
  const raw = task.metadata?.annotations?.[allocationTargetAnnotation];
  if (!raw) return undefined;
  try {
    const target = JSON.parse(raw) as { node?: { name?: unknown } };
    const name = target?.node?.name;
    return typeof name === "string" && name !== "" ? name : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Rows for Raw Tasks that have no Pod row. `podTaskNames` holds the task names
 * already represented by Pods, so a mixed Job's Kubernetes member is not
 * listed twice.
 */
export function rawWorkerRows(
  tasks: readonly RawTaskLike[],
  podTaskNames: ReadonlySet<string>,
): RawWorkerRow[] {
  const rows: RawWorkerRow[] = [];
  for (const task of tasks) {
    const name = task.metadata?.name ?? "";
    if (
      task.spec?.runtime?.kind !== "Raw" ||
      name === "" ||
      podTaskNames.has(name)
    )
      continue;
    rows.push({
      taskName: name,
      namespace: task.metadata?.namespace ?? "",
      node: allocationTargetNode(task) ?? "—",
      phase: task.status?.phase || "Pending",
      message: task.status?.message || undefined,
    });
  }
  return rows;
}
