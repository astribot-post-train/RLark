import assert from "node:assert/strict";
import { test } from "node:test";
import { Client, login, APIError } from "../.test-build/api.js";
import {
  LoadGate,
  nodeState,
  heartbeatAge,
  clusterState,
} from "../.test-build/models.js";

test("stale requests cannot publish after refresh or logout", () => {
  const gate = new LoadGate();
  const old = gate.begin();
  assert.equal(old(), true);
  const fresh = gate.begin();
  assert.equal(old(), false);
  assert.equal(fresh(), true);
  gate.cancel();
  assert.equal(fresh(), false);
});
test("capacity absence is distinct from a ready idle node", () => {
  assert.equal(nodeState({ bindingValid: false, ready: true }), "invalid");
  assert.equal(
    nodeState({ bindingValid: true, held: true, ready: true }),
    "held",
  );
  assert.equal(nodeState({ bindingValid: true, ready: false }), "stale");
  assert.equal(clusterState({ bindingValid: true, readyNodes: 0 }), "degraded");
  assert.equal(heartbeatAge("invalid"), "—");
  assert.equal(heartbeatAge(null), "—");
});
test("transport rejects malformed inventory rather than reporting empty capacity", async () => {
  globalThis.fetch = async () =>
    new Response(JSON.stringify({ items: [{ name: "missing fields" }] }));
  await assert.rejects(
    new Client("secret").list(),
    (e) => e instanceof APIError && e.code === "invalid_response",
  );
});
test("server errors preserve status without replacing inventory with zeros", async () => {
  globalThis.fetch = async () =>
    new Response(
      JSON.stringify({
        code: "unavailable",
        message: "management unavailable",
      }),
      { status: 503 },
    );
  await assert.rejects(
    new Client("secret").list(),
    (e) => e instanceof APIError && e.status === 503,
  );
});
test("target requests bind exact UID/resourceVersion/release and use only bearer auth", async () => {
  let request;
  globalThis.fetch = async (path, init) => {
    request = { path, init };
    return new Response("{}");
  };
  await new Client("secret").target(
    {
      namespace: "ns",
      name: "cluster",
      uid: "exact-cluster",
      resourceVersion: "42",
    },
    "v2",
    "exact-release",
  );
  assert.equal(request.path, "/api/v1/container-runtime/clusters/ns/cluster");
  assert.equal(request.init.headers.Authorization, "Bearer secret");
  assert.equal(request.init.credentials, "omit");
  assert.equal(request.init.redirect, "error");
  assert.deepEqual(JSON.parse(request.init.body), {
    uid: "exact-cluster",
    resourceVersion: "42",
    targetAgentRelease: "v2",
    releaseUID: "exact-release",
  });
});
test("create recovery retains operation identity and login requires admin contract", async () => {
  let bodies = [];
  globalThis.fetch = async (path, init) => {
    bodies.push(JSON.parse(init.body));
    return new Response(
      JSON.stringify({ namespace: "ns", name: "cluster", uid: "exact" }),
    );
  };
  const intent = {
    operationID: "same-uuid",
    name: "cluster",
    agentNamespace: "ns",
    tenantID: "t",
    runtime: "Raw",
  };
  const client = new Client("secret");
  await client.create(intent);
  await client.create(intent);
  assert.deepEqual(bodies, [intent, intent]);
  globalThis.fetch = async () =>
    new Response(JSON.stringify({ ok: true, role: "user", token: "ignored" }));
  await assert.rejects(login("u", "p"), APIError);
});

test("node scheduling sends an explicit intent with the companion UID and native resourceVersion", async () => {
  const calls = [];
  globalThis.fetch = async (path, init) => {
    calls.push({ path, init });
    return new Response("{}");
  };
  const client = new Client("admin");
  const node = {
    name: "node-1",
    uid: "companion-uid",
    nativeUID: "native-uid",
    resourceVersion: "123",
  };
  await client.nodeScheduling(
    { namespace: "agents", name: "cloud" },
    node,
    true,
  );
  await client.nodeScheduling(
    { namespace: "agents", name: "cloud" },
    node,
    false,
  );
  assert.deepEqual(
    calls.map((c) => JSON.parse(c.init.body)),
    [
      { uid: "companion-uid", resourceVersion: "123", unschedulable: true },
      { uid: "companion-uid", resourceVersion: "123", unschedulable: false },
    ],
  );
  assert.equal(
    calls[0].path,
    "/api/v1/container-runtime/clusters/agents/cloud/nodes/node-1",
  );
  assert.equal(calls[0].init.headers.Authorization, "Bearer admin");
});
