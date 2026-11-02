import assert from "node:assert/strict";
import test from "node:test";

import {
  filesPath,
  hasTerminalSession,
  isAdminPath,
  parseAdminRoute,
  containerClusterPath,
} from "../dist/test/utils/route.js";

test("admin routes preserve the storage class file browser context", () => {
  assert.equal(filesPath("shared/data"), "/files/shared%2Fdata");
  assert.equal(filesPath("shared/data", true), "/admin/files/shared%2Fdata");
  assert.deepEqual(parseAdminRoute("/admin/files/shared%2Fdata"), {
    page: "files",
    sub: "shared/data",
  });
});

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

test("admin path matching observes a route boundary", () => {
  assert.equal(isAdminPath("/admin"), true);
  assert.equal(isAdminPath("/admin/jobs"), true);
  assert.equal(isAdminPath("/administrator"), false);
});

test("terminal requires an access token", () => {
  const storage = (values) => ({ getItem: (key) => values[key] ?? null });
  assert.equal(
    hasTerminalSession(storage({ "rlark-auth-token": "token" })),
    true,
  );
  assert.equal(hasTerminalSession(storage({ "rlark-user-auth": "1" })), false);
  assert.equal(hasTerminalSession(storage({})), false);
});
