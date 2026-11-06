import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const readSource = (path) =>
  readFile(new URL(`../../src/${path}`, import.meta.url), "utf8");

const [containerStyles, containerClusters] = await Promise.all([
  readSource("admin/container-runtime/style.css"),
  readSource("admin/container-runtime/ContainerClustersPage.tsx"),
]);

test("container cluster page uses the shared page shell", () => {
  assert.match(
    containerClusters,
    /className="page-content resource-page container-runtime-page"/,
  );
  assert.match(containerClusters, /className="container-cluster-picker"/);
  assert.doesNotMatch(containerClusters, /className="cluster-picker"/);
  assert.match(containerClusters, /className="node-table"/);
  assert.match(containerClusters, /className="node-detail-table"/);
  assert.match(containerClusters, /expandedNodes\.has\(n\.uid\)/);
  assert.doesNotMatch(containerClusters, /collapsedNodes/);
  for (const field of [
    "t.privateIP",
    "t.nativeState",
    "t.agentState",
    "t.version",
    "t.heartbeat",
    "t.nativeAddresses",
    "t.capacity",
    "t.gpu",
    "t.capabilities",
  ]) {
    assert.ok(
      containerClusters.includes(field),
      `missing node table field ${field}`,
    );
  }
  assert.match(
    containerStyles,
    /\.container-runtime-page \.container-cluster-picker,[\s\S]*?padding: 16px;/,
  );
});
