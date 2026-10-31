import assert from "node:assert/strict";
import { test } from "node:test";
import { Client, APIError } from "../.test-build/api.js";
import { rawJobIntent } from "../.test-build/models.js";

const summary = {
  name: "raw-operation",
  uid: "job-uid",
  resourceVersion: "42",
  bindingUID: "binding-uid",
  operationID: "same-operation",
  phase: "Running",
  observed: true,
  suspended: false,
  activated: true,
  bindingValid: true,
};
test("Raw form preserves empty/quoted arguments and rejects unsafe input before submission", () => {
  const args = ["/usr/bin/python3", "-c", 'print(" hello ")', "", " space "];
  const request = rawJobIntent(
    "operation",
    "cluster",
    "prepared",
    JSON.stringify(args),
    '["NAME= a=b "]',
    "/work",
  );
  assert.deepEqual(request.process.argv, args);
  assert.deepEqual(request.process.env, ["NAME= a=b "]);
  for (const [argv, env, dir] of [
    ["python train.py", "[]", "/work"],
    ['["python"]', "[]", "/work"],
    ['["/usr/bin/python3"]', "[]", "/work/../secret"],
    ['["/usr/bin/python3"]', '["CUDA_VISIBLE_DEVICES=0"]', "/work"],
    ['["/usr/bin/python3"]', '["NVIDIA_VISIBLE_DEVICES=all"]', "/work"],
    ['["/usr/bin/python3"]', '["A=1","A=2"]', "/work"],
    ["[1]", "[]", "/work"],
    ['["/usr/bin/python3"]', '["A"]', "/work"],
    ['["/usr/bin/python3"]', "{}", "/work"],
  ])
    assert.throws(() =>
      rawJobIntent("operation", "cluster", "prepared", argv, env, dir),
    );
});
test("Job retry preserves identical intent, exact suspend preconditions, cancellation and auth", async () => {
  const calls = [];
  const controller = new AbortController();
  globalThis.fetch = async (path, init) => {
    calls.push({ path, init });
    return new Response(
      JSON.stringify(
        path.endsWith("/jobs") && init.method !== "POST"
          ? { items: [summary] }
          : summary,
      ),
    );
  };
  const client = new Client("admin");
  const req = rawJobIntent(
    "same-operation",
    "cluster-uid",
    "prepared",
    '["/usr/bin/python3","train.py"]',
    "[]",
    "/work",
  );
  await client.submitJob("agents", "cloud", req, controller.signal);
  await client.submitJob("agents", "cloud", req, controller.signal);
  assert.equal(calls[0].init.body, calls[1].init.body);
  assert.deepEqual(await client.jobs("agents", "cloud"), [summary]);
  await client.suspendJob("agents", "cloud", summary, controller.signal);
  assert.equal(
    calls[3].path,
    "/api/v1/container-runtime/clusters/agents/cloud/jobs/raw-operation/suspend",
  );
  assert.deepEqual(JSON.parse(calls[3].init.body), {
    uid: "job-uid",
    resourceVersion: "42",
  });
  assert.equal(calls[3].init.signal, controller.signal);
  assert.equal(calls[0].init.headers.Authorization, "Bearer admin");
  assert.equal(calls[0].init.credentials, "omit");
  assert.equal(calls[0].init.redirect, "error");
  globalThis.fetch = async () => new Response('{"items":[{"name":"invalid"}]}');
  await assert.rejects(
    client.jobs("agents", "cloud"),
    (e) => e instanceof APIError && e.code === "invalid_response",
  );
  globalThis.fetch = async () =>
    new Response('{"code":"conflict","message":"refresh"}', { status: 409 });
  await assert.rejects(
    client.suspendJob("agents", "cloud", summary),
    (e) => e instanceof APIError && e.status === 409,
  );
});
