import assert from "node:assert/strict";
import test from "node:test";
import { migrationConfirmation } from "../../dist/test/astribot/lib/provisionMigration.js";

const identity = { uid: "original", resource_version: "18" };
const candidate = (key, state, includeIdentity = true) => ({
  key,
  name: key,
  state,
  ...(includeIdentity ? { identity } : {}),
});
const preview = (...candidates) => ({
  provision_uid: "provision",
  provision_resource_version: "23",
  native_state: "review",
  candidates,
});

test("confirmation carries the snapshots of explicitly selected candidates", () => {
  const value = preview(
    candidate("cluster", "recorded"),
    candidate("site-network", "review"),
    candidate("station-read", "review"),
    candidate("release", "blocked", false),
  );
  assert.deepEqual(
    migrationConfirmation(
      value,
      new Set(["site-network", "release"]),
      " CHG-15 ",
      "native",
    ),
    {
      provision_uid: "provision",
      provision_resource_version: "23",
      evidence: "CHG-15",
      cluster: identity,
      site_network: identity,
      credentials: {},
      native_cluster_uid: "native",
    },
  );
});

test("live Cluster must be reviewed and a blocked Cluster prevents writes", () => {
  const value = preview(
    candidate("cluster", "review"),
    candidate("release", "review"),
  );
  assert.equal(
    migrationConfirmation(value, new Set(["release"]), "CHG"),
    undefined,
  );
  assert.deepEqual(
    migrationConfirmation(value, new Set(["cluster", "release"]), "CHG")
      .credentials,
    { release: identity },
  );
  value.candidates[0].state = "blocked";
  assert.equal(
    migrationConfirmation(value, new Set(["cluster", "release"]), "CHG"),
    undefined,
  );
});

test("no evidence or no new reviewed resources cannot be submitted", () => {
  assert.equal(
    migrationConfirmation(
      preview(candidate("cluster", "recorded")),
      new Set(),
      "CHG",
    ),
    undefined,
  );
  assert.equal(
    migrationConfirmation(
      preview(candidate("cluster", "review")),
      new Set(["cluster"]),
      " ",
    ),
    undefined,
  );
});
