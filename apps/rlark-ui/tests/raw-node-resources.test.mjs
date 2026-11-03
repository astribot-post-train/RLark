import assert from "node:assert/strict";
import test from "node:test";
import { rawResourceState } from "../dist/test/utils/rawNodeResources.js";
import { getNodeResourceSummary } from "../dist/test/utils/nodeResources.js";
import { nativeResourceNode } from "../dist/test/admin/container-runtime/api.js";

function node(now = Date.now()) {
  return {
    metadata: {
      name: "raw",
      generation: 2,
      annotations: {
        "containers.rlark.io/resources-observed-at": new Date(
          now,
        ).toISOString(),
      },
    },
    spec: { agentType: "Raw" },
    status: {
      phase: "Online",
      capacity: { cpu: "2500m", memory: "8Gi", "nvidia.com/gpu": "1" },
      allocatable: { "nvidia.com/gpu": "1" },
      used: { "nvidia.com/gpu": "0" },
      conditions: [
        { type: "RawResourcesReady", status: "True", observedGeneration: 2 },
        {
          type: "RawSchedulingReady",
          status: "True",
          observedGeneration: 2,
          reason: "RawNodeAvailable",
        },
      ],
    },
  };
}
test("Raw resources reject missing, stale, future and outdated observations", () => {
  const now = Date.now();
  for (const mode of [
    "missing",
    "stale",
    "future",
    "generation",
    "resources-invalid",
  ]) {
    const n = node(now);
    if (mode === "missing") n.metadata.annotations = {};
    if (mode === "stale")
      n.metadata.annotations["containers.rlark.io/resources-observed-at"] =
        new Date(now - 45001).toISOString();
    if (mode === "future")
      n.metadata.annotations["containers.rlark.io/resources-observed-at"] =
        new Date(now + 1).toISOString();
    if (mode === "generation") n.metadata.generation++;
    if (mode === "resources-invalid") n.status.conditions[0].status = "False";
    assert.equal(rawResourceState(n, now).current, false, mode);
    assert.equal(rawResourceState(n, now).schedulable, false, mode);
  }
  assert.equal(rawResourceState(node(now), now).schedulable, true);
});
test("Raw GPU summary never assumes an absent reservation means free", () => {
  const n = node();
  assert.equal(getNodeResourceSummary(n, false).primary, "1 / 1 GPU");
  delete n.status.used["nvidia.com/gpu"];
  assert.equal(getNodeResourceSummary(n, false).primary, "— / 1 GPU");
  n.status.used["nvidia.com/gpu"] = "0";
  n.spec.unschedulable = true;
  assert.equal(getNodeResourceSummary(n, false).primary, "0 / 1 GPU");
  n.spec.unschedulable = false;
  n.status.conditions[1].status = "False";
  n.status.conditions[1].reason = "AgentBusy";
  assert.equal(getNodeResourceSummary(n, false).primary, "0 / 1 GPU");
  n.metadata.annotations = {};
  assert.equal(getNodeResourceSummary(n, false).primary, "— / 1 GPU");
});
test("container detail adapter preserves native resource semantics and unknowns", () => {
  assert.equal(nativeResourceNode({ name: "legacy" }, "Raw"), undefined);
  const r = {
    name: "raw",
    namespace: "agents",
    generation: 3,
    phase: "Online",
    capacity: { cpu: "2500m" },
    allocatable: { cpu: "2500m" },
    used: null,
    nodeInfo: { architecture: "amd64", kernelVersion: "test" },
    addresses: null,
    conditions: null,
  };
  const n = nativeResourceNode({ resources: r, unschedulable: true }, "Raw");
  assert.deepEqual(n.status.capacity, r.capacity);
  assert.equal(n.metadata.generation, 3);
  assert.deepEqual(n.status.used, {});
  assert.equal(rawResourceState(n).schedulable, false);
});
