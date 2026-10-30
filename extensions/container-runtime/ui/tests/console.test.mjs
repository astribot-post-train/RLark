import assert from "node:assert/strict";
import { test } from "node:test";
import { Client, login, APIError } from "../.test-build/api.js";
import {
  LoadGate,
  nodeState,
  heartbeatAge,
  clusterState,
  logText,
} from "../.test-build/models.js";

test("log reads pin exact allocation and Task identity and preserve cancellation/auth", async () => {
  let request;
  const controller = new AbortController();
  globalThis.fetch = async (path, init) => {
    request = { path, init };
    return new Response(
      JSON.stringify({
        data: "bG9nCg==",
        truncated: false,
        retentionLimitReached: false,
      }),
    );
  };
  const client = new Client("admin");
  const snapshot = await client.logs(
    "agents",
    { name: "lock", uid: "exact-allocation", taskUID: "exact-task" },
    controller.signal,
  );
  const url = new URL(request.path, "http://localhost");
  assert.equal(url.pathname, "/api/v1/container-runtime/logs");
  assert.deepEqual(Object.fromEntries(url.searchParams), {
    namespace: "agents",
    allocation: "lock",
    allocationUID: "exact-allocation",
    taskUID: "exact-task",
  });
  assert.equal(request.init.headers.Authorization, "Bearer admin");
  assert.equal(request.init.credentials, "omit");
  assert.equal(request.init.redirect, "error");
  assert.equal(request.init.signal, controller.signal);
  assert.equal(logText(snapshot.data), "log\n");
  globalThis.fetch = async () =>
    new Response(JSON.stringify({ data: "", truncated: "false" }));
  await assert.rejects(
    client.logs("agents", { name: "lock", uid: "uid", taskUID: "task" }),
    APIError,
  );
});

test("log rendering bounds decoded bytes and neutralizes control sequences", () => {
  const data = btoa(
    String.fromCharCode(
      ...new TextEncoder().encode("日志\n<script>text</script>\x1b[31m\u202e"),
    ),
  );
  assert.equal(logText(data), "日志\n<script>text</script>�[31m�");
  assert.equal(logText(btoa("X".repeat(65536))).length, 65536);
  assert.throws(() => logText(btoa("X".repeat(65537))));
  assert.throws(() => logText("invalid base64!"));
});

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
test("legacy console inventory loads with submission disabled and rejects invalid capabilities", async () => {
  const inventory = {
    cluster: {
      namespace: "agents",
      name: "cloud",
      uid: "cluster-uid",
      resourceVersion: "42",
      tenantID: "tenant",
      runtime: "Raw",
      targetAgentRelease: "",
      bindingValid: true,
      deleting: false,
      nodeCount: 0,
      readyNodes: 0,
      busyNodes: 0,
      gpuCount: 0,
      allocationCount: 0,
      upgradeCount: 0,
      conditions: null,
    },
    nodes: [],
    releases: [],
    allocations: [],
    upgrades: [],
  };
  const client = new Client("admin");
  globalThis.fetch = async () => new Response(JSON.stringify(inventory));
  assert.deepEqual(await client.detail("agents", "cloud"), {
    ...inventory,
    jobSubmissionEnabled: false,
    nodeRegistrationEnabled: false,
  });
  for (const capability of [false, true]) {
    globalThis.fetch = async () =>
      new Response(
        JSON.stringify({ ...inventory, jobSubmissionEnabled: capability }),
      );
    assert.equal(
      (await client.detail("agents", "cloud")).jobSubmissionEnabled,
      capability,
    );
  }
  for (const capability of [null, "false", 0]) {
    globalThis.fetch = async () =>
      new Response(
        JSON.stringify({ ...inventory, jobSubmissionEnabled: capability }),
      );
    await assert.rejects(
      client.detail("agents", "cloud"),
      (e) => e instanceof APIError && e.code === "invalid_response",
    );
  }
  for (const capability of [true, false, null, "false", 0]) {
    globalThis.fetch = async () =>
      new Response(
        JSON.stringify({ ...inventory, nodeRegistrationEnabled: capability }),
      );
    if (typeof capability === "boolean")
      assert.equal(
        (await client.detail("agents", "cloud")).nodeRegistrationEnabled,
        capability,
      );
    else await assert.rejects(client.detail("agents", "cloud"), APIError);
  }
  globalThis.fetch = async () =>
    new Response(JSON.stringify({ ...inventory, nodes: null }));
  await assert.rejects(client.detail("agents", "cloud"), APIError);
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
