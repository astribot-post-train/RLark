import assert from "node:assert/strict";
import test from "node:test";
import {
  allocationTargetAnnotation,
  allocationTargetNode,
  rawWorkerRows,
} from "../../dist/test/admin/container-runtime/rawWorkers.js";

function task(name, kind, { annotation, phase, message } = {}) {
  return {
    metadata: {
      name,
      namespace: "ksyun-container-agents",
      annotations:
        annotation === undefined
          ? {}
          : { [allocationTargetAnnotation]: annotation },
    },
    spec: { runtime: { kind } },
    status: { phase, message },
  };
}

const target = (node) =>
  JSON.stringify({
    node: { namespace: "ksyun-container-agents", name: node, uid: "u" },
  });

test("Raw task row carries the node from the allocation target", () => {
  const rows = rawWorkerRows(
    [
      task("job-rollout", "Raw", {
        annotation: target("ksyun-b"),
        phase: "Running",
      }),
    ],
    new Set(),
  );
  assert.deepEqual(rows, [
    {
      taskName: "job-rollout",
      namespace: "ksyun-container-agents",
      node: "ksyun-b",
      phase: "Running",
      message: undefined,
    },
  ]);
});

test("Kubernetes tasks and tasks with Pod rows are not listed again", () => {
  const rows = rawWorkerRows(
    [
      task("job-env", "Kubernetes", { phase: "Running" }),
      task("job-rollout", "Raw", { annotation: target("ksyun-b") }),
    ],
    new Set(["job-rollout"]),
  );
  assert.deepEqual(rows, []);
});

test("missing or malformed allocation target shows no node", () => {
  assert.equal(allocationTargetNode(task("a", "Raw")), undefined);
  assert.equal(
    allocationTargetNode(task("b", "Raw", { annotation: "{not json" })),
    undefined,
  );
  assert.equal(
    allocationTargetNode(
      task("c", "Raw", { annotation: '{"node":{"name":7}}' }),
    ),
    undefined,
  );
  const [row] = rawWorkerRows(
    [task("d", "Raw", { annotation: "{" })],
    new Set(),
  );
  assert.equal(row.node, "—");
  assert.equal(row.phase, "Pending");
});

test("failed Raw task keeps its status message", () => {
  const [row] = rawWorkerRows(
    [
      task("job-rollout", "Raw", {
        annotation: target("ksyun-c"),
        phase: "Failed",
        message: "exit 4",
      }),
    ],
    new Set(),
  );
  assert.equal(row.phase, "Failed");
  assert.equal(row.message, "exit 4");
});
