import assert from "node:assert/strict";
import test from "node:test";

import {
  AUTH_ROLE_KEY,
  AUTH_TOKEN_KEY,
  ApiError,
  clearAuthSession,
  errorMessage,
  hasAuthSession,
  storeAuthSession,
  UNAUTHORIZED_EVENT,
  request,
} from "../dist/test/api.js";
import {
  apiReferenceApi,
  clustersApi,
  domainsApi,
  imageRegistriesApi,
  nodesApi,
  stationsApi,
  systemConfigApi,
} from "../dist/test/backend.js";

test("Station APIs encode Cluster IDs, names, and pagination parameters", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  const calls = [];
  globalThis.fetch = async (input, init) => {
    calls.push({ input: String(input), init });
    return new Response(JSON.stringify({ items: [], continue: "" }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };
  const controller = new AbortController();
  await stationsApi.list({ cluster_id: "cluster/a", limit: "200", continue: "token +/=" }, controller.signal);
  await stationsApi.get("station/a", "cluster/a", controller.signal);
  await stationsApi.observations("cluster/a", controller.signal);
  await stationsApi.createRebind({
    cluster_id: "cluster/a",
    request_id: "request-1",
    station_name: "station/a",
    station_uid: "uid-1",
    expected_binding_version: 2,
    target_node_name: "node-a",
    target_node_uid: "node-uid-a",
    reason: "approved replacement",
  });
  await stationsApi.getRebind("rebind/a", "cluster/a", controller.signal);
  assert.equal(calls[0].input, "/api/v1/station/stations?cluster_id=cluster%2Fa&limit=200&continue=token+%2B%2F%3D");
  assert.equal(calls[1].input, "/api/v1/station/stations/station%2Fa?cluster_id=cluster%2Fa");
  assert.equal(calls[2].input, "/api/v1/station/observations?cluster_id=cluster%2Fa&limit=500");
  assert.equal(calls[3].input, "/api/v1/station/rebinds");
  assert.deepEqual(JSON.parse(calls[3].init.body), {
    cluster_id: "cluster/a",
    request_id: "request-1",
    station_name: "station/a",
    station_uid: "uid-1",
    expected_binding_version: 2,
    target_node_name: "node-a",
    target_node_uid: "node-uid-a",
    reason: "approved replacement",
  });
  assert.equal(calls[4].input, "/api/v1/station/rebinds/rebind%2Fa?cluster_id=cluster%2Fa");
  assert.ok(calls.filter((call) => call.init.signal).every((call) => call.init.signal === controller.signal));
});

test("registry browse API encodes parameters and forwards cancellation", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  const calls = [];
  globalThis.fetch = async (input, init) => {
    calls.push({ input: String(input), init });
    return new Response(
      JSON.stringify({ items: [], next: "", digest: "sha256:test" }),
      {
        status: 200,
        headers: { "Content-Type": "application/json" },
      },
    );
  };
  const controller = new AbortController();
  await imageRegistriesApi.list(controller.signal);
  await imageRegistriesApi.repositories("id/a", "a/b", controller.signal);
  await imageRegistriesApi.tags("id/a", "a/b", "v1", controller.signal);
  await imageRegistriesApi.manifest("id/a", "a/b", "v1", controller.signal);
  await imageRegistriesApi.password("id/a", controller.signal);
  assert.equal(
    calls[1].input,
    "/api/v1/image-registries/id%2Fa/repositories?last=a%2Fb",
  );
  assert.equal(
    calls[2].input,
    "/api/v1/image-registries/id%2Fa/tags?repository=a%2Fb&last=v1",
  );
  assert.equal(
    calls[3].input,
    "/api/v1/image-registries/id%2Fa/manifest?repository=a%2Fb&tag=v1",
  );
  assert.equal(calls[0].input, "/api/v1/image-registries");
  assert.equal(calls[4].input, "/api/v1/image-registries/id%2Fa/password");
  assert.ok(calls.every((call) => call.init.signal === controller.signal));
});

test("registry browsing propagates authorization and upstream failures and can recover", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  let status = 403;
  globalThis.fetch = async () =>
    new Response(
      JSON.stringify(
        status === 200
          ? { items: ["image"], next: "" }
          : { message: "unavailable" },
      ),
      { status, headers: { "Content-Type": "application/json" } },
    );
  await assert.rejects(
    () => imageRegistriesApi.list(),
    (error) => error instanceof ApiError && error.status === 403,
  );
  status = 502;
  await assert.rejects(
    () => imageRegistriesApi.repositories("registry"),
    (error) => error instanceof ApiError && error.status === 502,
  );
  status = 200;
  assert.deepEqual(await imageRegistriesApi.repositories("registry"), {
    items: ["image"],
    next: "",
  });
});

function storage() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

test("request attaches the JWT and preserves headers", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  storeAuthSession("jwt-token", "admin");
  globalThis.fetch = async (_input, init) => {
    assert.equal(init.headers.get("Authorization"), "Bearer jwt-token");
    assert.equal(init.headers.get("Content-Type"), "application/json");
    return new Response(null, { status: 200 });
  };

  const response = await request("/api/v1/clusters", {
    headers: { "Content-Type": "application/json" },
  });
  assert.equal(response.status, 200);
  assert.equal(hasAuthSession("admin"), true);
});

test("request clears the session after a 401", async () => {
  globalThis.sessionStorage = storage();
  let eventType = "";
  globalThis.window = { dispatchEvent: (event) => (eventType = event.type) };
  sessionStorage.setItem(AUTH_TOKEN_KEY, "expired");
  sessionStorage.setItem(AUTH_ROLE_KEY, "user");
  globalThis.fetch = async () => new Response(null, { status: 401 });

  await assert.rejects(() => request("/api/v1/clusters"));
  assert.equal(sessionStorage.getItem(AUTH_TOKEN_KEY), null);
  assert.equal(eventType, UNAUTHORIZED_EVENT);
  clearAuthSession();
});

test("request reports the detail of canonical and legacy error bodies", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  const bodies = [
    { code: "RESOURCE_ALREADY_EXISTS", message: "public key already exists" },
    { error: "public key already exists" },
  ];
  globalThis.fetch = async () =>
    new Response(JSON.stringify(bodies.shift()), {
      status: 409,
      headers: { "Content-Type": "application/json" },
    });

  for (let attempt = 0; attempt < 2; attempt += 1) {
    const error = await request("/api/v1/ssh-user-keys", {
      method: "POST",
      body: {},
    }).then(
      () => null,
      (reason) => reason,
    );
    assert.ok(error instanceof ApiError);
    assert.equal(error.status, 409);
    assert.equal(error.message, "HTTP 409: public key already exists");
    assert.equal(errorMessage(error.body), "public key already exists");
  }
});

test("request falls back to the raw body when it carries no detail", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  globalThis.fetch = async () =>
    new Response("upstream failure", { status: 502 });

  const error = await request("/api/v1/clusters").then(
    () => null,
    (reason) => reason,
  );
  assert.equal(error.message, "HTTP 502: upstream failure");
  assert.equal(errorMessage(error.body), "");
});

test("cluster lifecycle actions use the gateway lifecycle route", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  const calls = [];
  globalThis.fetch = async (input, init) => {
    calls.push({ input: String(input), init });
    return new Response(JSON.stringify({ data: {} }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  await clustersApi.lifecycle("cluster/a", "delete");

  assert.equal(calls[0].input, "/api/v1/clusters/cluster%2Fa/delete");
  assert.equal(calls[0].init.method, "POST");
});

test("resource APIs encode paths, query values, and JSON bodies", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  const calls = [];
  globalThis.fetch = async (input, init) => {
    calls.push({ input: String(input), init });
    return new Response(JSON.stringify({ items: [] }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  await nodesApi.list({ labelSelector: "rlark.io/cluster-id=a/b" });
  await domainsApi.create({ metadata: { name: "domain-a" } });

  assert.equal(
    calls[0].input,
    "/api/v1/rlinf.io/v1alpha1/nodes?labelSelector=rlark.io%2Fcluster-id%3Da%2Fb",
  );
  assert.equal(calls[1].init.method, "POST");
  assert.equal(calls[1].init.headers.get("Content-Type"), "application/json");
  assert.equal(calls[1].init.body, '{"metadata":{"name":"domain-a"}}');
});

test("system config API shares reads and refreshes its cache after update", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  let calls = 0;
  globalThis.fetch = async (_input, init) => {
    calls += 1;
    const config =
      init?.method === "PUT"
        ? { ssh: { jumpHost: "new.example.com", jumpPort: "22" } }
        : { ssh: { jumpHost: "old.example.com", jumpPort: "22" } };
    return new Response(JSON.stringify(config), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  const [first, second] = await Promise.all([
    systemConfigApi.get({ refresh: true }),
    systemConfigApi.get(),
  ]);
  assert.equal(calls, 1);
  assert.equal(first.ssh.jumpHost, "old.example.com");
  assert.equal(second.ssh.jumpHost, "old.example.com");

  await systemConfigApi.update({ ssh: { jumpHost: "new.example.com" } });
  const cached = await systemConfigApi.get();
  assert.equal(calls, 2);
  assert.equal(cached.ssh.jumpHost, "new.example.com");
});

test("API reference is loaded from Gateway", async () => {
  globalThis.sessionStorage = storage();
  globalThis.window = { dispatchEvent() {} };
  globalThis.fetch = async (input) => {
    assert.equal(String(input), "/api/v1/api-reference");
    return new Response(
      JSON.stringify({
        title: { zh: "接口参考", en: "API Reference" },
        description: { zh: "接口", en: "APIs" },
        sections: [],
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  };

  const reference = await apiReferenceApi.get();
  assert.equal(reference.title.en, "API Reference");
});
