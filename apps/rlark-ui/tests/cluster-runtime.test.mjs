import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  nodesForCluster,
  isRawCluster,
  runtimeMapFromContainerClusters,
} from "../dist/test/utils/clusterRuntime.js";

describe("Cluster runtime utilities", () => {
  it("matches Kubernetes cluster nodes by namespace === clusterId", () => {
    const nodes = [
      { metadata: { namespace: "k8s-cluster-1", name: "node-1" } },
      { metadata: { namespace: "k8s-cluster-2", name: "node-2" } },
    ];
    const runtimeMap = new Map();
    const result = nodesForCluster(nodes, "k8s-cluster-1", runtimeMap);
    assert.equal(result.length, 1);
    assert.equal(result[0].metadata.name, "node-1");
  });

  it("matches Raw cluster nodes by agentNamespace", () => {
    const nodes = [
      { metadata: { namespace: "ksyun-container-agents", name: "ksyun-a" } },
      { metadata: { namespace: "ksyun-container-agents", name: "ksyun-b" } },
      { metadata: { namespace: "rlark-robot-s1u-01", name: "s1u-01-x86" } },
    ];
    const runtimeMap = new Map([
      ["ksyun-containers", { runtime: "Raw", agentNamespace: "ksyun-container-agents" }],
    ]);
    const result = nodesForCluster(nodes, "ksyun-containers", runtimeMap);
    assert.equal(result.length, 2);
    assert.equal(result[0].metadata.name, "ksyun-a");
    assert.equal(result[1].metadata.name, "ksyun-b");
  });

  it("maps container-runtime clusters to their agent namespace", () => {
    const map = runtimeMapFromContainerClusters([
      { name: "ksyun-containers", namespace: "ksyun-container-agents", runtime: "Raw" },
      { name: "docker-pool", namespace: "docker-agents", runtime: "Docker" },
    ]);
    assert.deepEqual(map.get("ksyun-containers"), {
      runtime: "Raw",
      agentNamespace: "ksyun-container-agents",
    });
    assert.equal(map.has("docker-pool"), false);
  });

  it("identifies Raw clusters correctly", () => {
    const runtimeMap = new Map([
      ["ksyun-containers", { runtime: "Raw", agentNamespace: "ksyun-container-agents" }],
      ["k8s-cluster", { runtime: "Kubernetes" }],
    ]);
    assert.equal(isRawCluster("ksyun-containers", runtimeMap), true);
    assert.equal(isRawCluster("k8s-cluster", runtimeMap), false);
    assert.equal(isRawCluster("unknown", runtimeMap), false);
  });
});
