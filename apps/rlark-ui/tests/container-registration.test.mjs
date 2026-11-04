const session = new Map([["rlark-auth-token", "admin"]]);
globalThis.sessionStorage = {
  getItem: (key) => session.get(key) ?? null,
  setItem: (key, value) => session.set(key, value),
  removeItem: (key) => session.delete(key),
};
globalThis.window = { dispatchEvent: () => {} };
import assert from "node:assert/strict";
import { test } from "node:test";
import { APIError, Client } from "../dist/test/admin/container-runtime/api.js";
import { nodeRegistrationIntent } from "../dist/test/admin/container-runtime/models.js";

const fields = {
  name: "gpu-2",
  instanceID: "verified-incarnation",
  privateAddress: "10.0.3.86",
  environment: "prepared",
  networkScope: "private",
  role: "Worker",
  gpuText: '["GPU-b","GPU-a"]',
};
test("registration permits automatic GPU discovery and preserves explicit identity", () => {
  const original = structuredClone(fields);
  const request = nodeRegistrationIntent("operation", "cluster", fields);
  assert.deepEqual(fields, original);
  assert.deepEqual(request.authorizedGPUUUIDs, ["GPU-a", "GPU-b"]);
  assert.equal(request.role, "Worker");
  assert.equal(request.instanceID, fields.instanceID);
  for (const change of [
    { name: "../foreign" },
    { instanceID: "" },
    { environment: " " },
    { networkScope: "" },
    { privateAddress: "8.8.8.8" },
    { privateAddress: "172.32.0.1" },
    { privateAddress: "10.0.0.999" },
    { privateAddress: "010.0.0.1" },
    { role: "Unknown" },
    { gpuText: '["GPU-a","GPU-a"]' },
    { gpuText: '["all"]' },
    { gpuText: '["GPU-1,2"]' },
    { gpuText: '["GPU-1 2"]' },
    { gpuText: "{}" },
  ])
    assert.throws(() =>
      nodeRegistrationIntent("operation", "cluster", { ...fields, ...change }),
    );
  assert.deepEqual(
    nodeRegistrationIntent("operation", "cluster", { ...fields, gpuText: "[]" }).authorizedGPUUUIDs,
    [],
  );
  assert.throws(() =>
    nodeRegistrationIntent("operation", "cluster", {
      ...fields,
      instanceID: "x".repeat(65536),
    }),
  );
});
test("lost registration response retries identical input and rejects foreign acknowledgement", async () => {
  const calls = [];
  const controller = new AbortController();
  const value = nodeRegistrationIntent("same-operation", "cluster-uid", fields);
  const result = {
    namespace: "agents",
    name: fields.name,
    uid: "companion-uid",
    nativeUID: "native-uid",
    clusterUID: value.clusterUID,
    operationID: value.operationID,
  };
  globalThis.fetch = async (path, init) => {
    calls.push({ path, init });
    if (calls.length === 1) throw new TypeError("response lost after create");
    return new Response(JSON.stringify(result));
  };
  const client = new Client();
  await assert.rejects(
    client.registerNode("agents", "cloud", value, controller.signal),
    TypeError,
  );
  assert.deepEqual(
    await client.registerNode("agents", "cloud", value, controller.signal),
    result,
  );
  assert.equal(calls[0].init.body, calls[1].init.body);
  assert.equal(
    calls[1].path,
    "/api/v1/container-runtime/clusters/agents/cloud/nodes",
  );
  assert.equal(calls[1].init.signal, controller.signal);
  assert.equal(
    new Headers(calls[1].init.headers).get("Authorization"),
    "Bearer admin",
  );
  assert.equal(calls[1].init.cache, "no-store");
  assert.equal(calls[1].init.redirect, "error");
  for (const change of [
    { namespace: "foreign" },
    { name: "other" },
    { clusterUID: "replacement" },
    { operationID: "foreign" },
    { uid: "" },
    { nativeUID: "" },
  ]) {
    globalThis.fetch = async () =>
      new Response(JSON.stringify({ ...result, ...change }));
    await assert.rejects(
      client.registerNode("agents", "cloud", value),
      (e) => e instanceof APIError && e.status === 502,
    );
  }
  assert.deepEqual(
    value,
    nodeRegistrationIntent("same-operation", "cluster-uid", fields),
  );
});
