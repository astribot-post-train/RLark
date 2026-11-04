const session = new Map([["rlark-auth-token", "admin"]]);
globalThis.sessionStorage = {
  getItem: (key) => session.get(key) ?? null,
  setItem: (key, value) => session.set(key, value),
  removeItem: (key) => session.delete(key),
};
globalThis.window = { dispatchEvent: () => {} };
import assert from "node:assert/strict";
import { test } from "node:test";
import { Client, APIError } from "../dist/test/admin/container-runtime/api.js";
import {
  LoadGate,
  nodeState,
  nativeNodeState,
  heartbeatAge,
  clusterState,
  logText,
  clusterIntentFromForm,
} from "../dist/test/admin/container-runtime/models.js";

test("cluster form preserves each runtime and refuses an unknown runtime", async () => {
  const fields = {
    name: "cloud",
    agentNamespace: "agents",
    tenantID: "tenant",
  };
  const client = new Client();
  for (const runtime of ["Docker", "Raw"]) {
    const intent = clusterIntentFromForm("operation", { ...fields, runtime });
    const seen = [];
    globalThis.fetch = async (_path, init) => {
      seen.push(JSON.parse(init.body));
      throw new Error("response lost");
    };
    await assert.rejects(client.create(intent));
    await assert.rejects(client.create(intent));
    assert.deepEqual(seen, [intent, intent]);
    assert.equal(intent.runtime, runtime);
  }
  for (const runtime of ["", "docker", "Kubernetes", "Unknown"]) {
    assert.throws(() =>
      clusterIntentFromForm("operation", { ...fields, runtime }),
    );
  }
});

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
  const client = new Client();
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
  assert.equal(
    new Headers(request.init.headers).get("Authorization"),
    "Bearer admin",
  );
  assert.equal(request.init.cache, "no-store");
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
test("native connectivity and container Agent readiness remain independent", () => {
  const node = {
    bindingValid: true,
    held: false,
    ready: false,
    resources: { phase: "Online" },
  };
  assert.equal(nativeNodeState(node), "online");
  assert.equal(nodeState(node), "stale");
  assert.equal(node.schedulable, undefined);
  assert.equal(nativeNodeState({ resources: { phase: "Offline" } }), "offline");
  assert.equal(nativeNodeState({ resources: undefined }), "unknown");
});
test("transport rejects malformed inventory rather than reporting empty capacity", async () => {
  globalThis.fetch = async () =>
    new Response(JSON.stringify({ items: [{ name: "missing fields" }] }));
  await assert.rejects(
    new Client().list(),
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
  const client = new Client();
  globalThis.fetch = async () => new Response(JSON.stringify(inventory));
  assert.deepEqual(await client.detail("agents", "cloud"), {
    ...inventory,
    jobSubmissionEnabled: false,
    nodeRegistrationEnabled: false,
    enrollmentEnabled: false,
    enrollments: [],
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
    new Client().list(),
    (e) => e instanceof APIError && e.status === 503,
  );
});
test("target requests bind exact UID/resourceVersion/release and use only bearer auth", async () => {
  let request;
  globalThis.fetch = async (path, init) => {
    request = { path, init };
    return new Response("{}");
  };
  await new Client().target(
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
  assert.equal(
    new Headers(request.init.headers).get("Authorization"),
    "Bearer admin",
  );
  assert.equal(request.init.cache, "no-store");
  assert.equal(request.init.redirect, "error");
  assert.deepEqual(JSON.parse(request.init.body), {
    uid: "exact-cluster",
    resourceVersion: "42",
    targetAgentRelease: "v2",
    releaseUID: "exact-release",
  });
});

test("node scheduling sends an explicit intent with the companion UID and native resourceVersion", async () => {
  const calls = [];
  globalThis.fetch = async (path, init) => {
    calls.push({ path, init });
    return new Response("{}");
  };
  const client = new Client();
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
  assert.equal(
    new Headers(calls[0].init.headers).get("Authorization"),
    "Bearer admin",
  );
});

test("Docker stats reads preserve exact identity, cancellation and canonical uint64 counters", async () => {
  let captured;
  const controller = new AbortController();
  const allocation = { name: "lock", uid: "allocation", taskUID: "task" };
  globalThis.fetch = async (path, init) => {
    captured = { path, init };
    return new Response(
      JSON.stringify({
        sampledAt: new Date().toISOString(),
        cpuUsageNS: "18446744073709551615",
        memoryUsageBytes: "0",
      }),
    );
  };
  const client = new Client();
  const snapshot = await client.stats("agents", allocation, controller.signal);
  assert.equal(snapshot.cpuUsageNS, "18446744073709551615");
  assert.equal(snapshot.memoryUsageBytes, "0");
  assert.equal(snapshot.pids, undefined);
  const url = new URL(captured.path, "http://localhost");
  assert.equal(url.pathname, "/api/v1/container-runtime/stats");
  assert.deepEqual(Object.fromEntries(url.searchParams), {
    namespace: "agents",
    allocation: "lock",
    allocationUID: "allocation",
    taskUID: "task",
  });
  assert.equal(captured.init.signal, controller.signal);
  assert.equal(
    new Headers(captured.init.headers).get("Authorization"),
    "Bearer admin",
  );
  for (const value of ["01", "-1", "18446744073709551616", 1]) {
    globalThis.fetch = async () =>
      new Response(
        JSON.stringify({ sampledAt: new Date().toISOString(), pids: value }),
      );
    await assert.rejects(client.stats("agents", allocation), APIError);
  }
  for (const sampledAt of ["invalid", "2099-01-01T00:00:00Z", ""]) {
    globalThis.fetch = async () => new Response(JSON.stringify({ sampledAt }));
    await assert.rejects(client.stats("agents", allocation), APIError);
  }
});

test("diagnostic scope changes on allocation replacement, cleanup and capability revocation", async () => {
  const { canReadDiagnostic, diagnosticInventory } =
    await import("../dist/test/admin/container-runtime/models.js");
  const allocation = {
    uid: "allocation",
    taskUID: "task",
    nodeName: "node",
    deleting: false,
    cleanupConfirmed: false,
  };
  const detail = {
    cluster: { uid: "cluster", runtime: "Docker" },
    nodes: [
      {
        name: "node",
        capabilities: [
          "docker-diagnostics-v1",
          "docker-container-execution-v1",
        ],
      },
    ],
    allocations: [allocation],
  };
  const scope = diagnosticInventory(detail);
  assert.equal(canReadDiagnostic(detail, allocation), true);
  assert.notEqual(
    diagnosticInventory({
      ...detail,
      allocations: [{ ...allocation, taskUID: "replacement" }],
    }),
    scope,
  );
  assert.equal(
    canReadDiagnostic(detail, { ...allocation, cleanupConfirmed: true }),
    false,
  );
  assert.notEqual(
    diagnosticInventory({
      ...detail,
      nodes: [{ name: "node", capabilities: [] }],
    }),
    scope,
  );
  assert.notEqual(diagnosticInventory(undefined), scope);
});

test("native resource transport validates optional projection and recovers after malformed data", async () => {
  const resources = {
    name: "raw",
    namespace: "agents",
    generation: 2,
    phase: "Online",
    observedAt: new Date().toISOString(),
    capacity: { cpu: "2500m" },
    allocatable: { cpu: "2500m" },
    used: null,
    nodeInfo: { architecture: "amd64" },
    addresses: null,
    conditions: null,
  };
  const node = {
    name: "raw",
    uid: "companion",
    nativeUID: "native",
    instanceID: "instance",
    privateAddress: "10.0.0.1",
    environment: "test",
    networkScope: "private",
    role: "Worker",
    agentVersion: "test",
    resourceVersion: "2",
    ready: true,
    schedulable: false,
    busy: false,
    held: false,
    bindingValid: true,
    unschedulable: true,
    authorizedGPUUUIDs: null,
    discoveredGPUUUIDs: null,
    capabilities: null,
    conditions: null,
    lastHeartbeatTime: null,
    capacity: null,
  };
  const inventory = {
    cluster: {
      namespace: "agents",
      name: "cloud",
      uid: "cluster",
      resourceVersion: "2",
      tenantID: "tenant",
      runtime: "Raw",
      targetAgentRelease: "",
      bindingValid: true,
      deleting: false,
      nodeCount: 1,
      readyNodes: 1,
      busyNodes: 0,
      gpuCount: 0,
      allocationCount: 0,
      upgradeCount: 0,
      conditions: null,
    },
    nodes: [node],
    releases: [],
    allocations: [],
    upgrades: [],
  };
  const client = new Client();
  globalThis.fetch = async () => new Response(JSON.stringify(inventory));
  assert.equal(
    (await client.detail("agents", "cloud")).nodes[0].resources,
    undefined,
  );
  for (const invalid of [
    null,
    { ...resources, used: { cpu: 0 } },
    { ...resources, observedAt: "invalid" },
    { ...resources, generation: "2" },
    { ...resources, addresses: [{}] },
    { ...resources, nodeInfo: { architecture: 2 } },
  ]) {
    node.resources = invalid;
    await assert.rejects(
      client.detail("agents", "cloud"),
      (e) => e instanceof APIError && e.code === "invalid_response",
    );
  }
  node.resources = resources;
  assert.deepEqual(
    (await client.detail("agents", "cloud")).nodes[0].resources,
    resources,
  );
});
