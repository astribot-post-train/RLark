import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { parseAdminRoute } from "../../dist/test/utils/route.js";
import { containerClusterPath } from "../../dist/test/admin/container-runtime/routes.js";

const readSource = (path) =>
  readFile(new URL(`../../src/${path}`, import.meta.url), "utf8");

test("container cluster URLs restore selection on reload and history navigation", () => {
  const path = containerClusterPath("cloud-agents", "ksyun");
  assert.equal(path, "/admin/container-clusters/cloud-agents/ksyun");
  assert.deepEqual(parseAdminRoute(path), {
    page: "container-clusters",
    sub: "cloud-agents/ksyun",
  });
  assert.deepEqual(parseAdminRoute("/admin/container-clusters"), {
    page: "container-clusters",
    sub: "",
  });
});

test("upstream files reference the extension through single registration lines", async () => {
  const [constants, route, adminApp, clusters] = await Promise.all([
    readSource("constants.ts"),
    readSource("utils/route.ts"),
    readSource("admin/AdminApp.tsx"),
    readSource("pages/Clusters.tsx"),
  ]);
  assert.match(constants, /^\s+containerRuntimeNavItem,$/m);
  assert.match(route, /^\s+\.\.\.containerRuntimePages,$/m);
  assert.match(adminApp, /<ContainerRuntimeRoute lang=\{lang\} sub=\{adminSub\} setSub=\{setAdminSub\} \/>/);
  assert.match(clusters, /agentType === "Raw"\)\s+return <RawNodeDetail/);
  for (const source of [constants, route, adminApp]) {
    assert.doesNotMatch(source, /"container-clusters"/);
  }
});
