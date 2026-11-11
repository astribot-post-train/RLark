import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const page = readFileSync("src/pages/Stations.tsx", "utf8");
const backend = readFileSync("src/backend.ts", "utf8");
const mock = readFileSync("src/mockBackend.ts", "utf8");

test("Station rebind is gated on conflicts and uses verified observation identity", () => {
  assert.match(page, /selected\.status\?\.phase === "Conflict"/);
  assert.match(page, /stationsApi\.observations\(selectedCluster\.id/);
  assert.match(page, /target_node_uid: submitted\.target\.status\.nodeUID/);
  assert.match(page, /station_uid: submitted\.stationUID/);
  assert.match(page, /expected_binding_version: submitted\.bindingVersion/);
  assert.match(page, /stationsApi\.getRebind\(trackedName, trackedClusterID/);
  assert.match(page, /Retry same request/);
});

test("Station backend supports operation submission and mock mode does not fabricate data", () => {
  assert.match(backend, /createRebind\(input:/);
  assert.match(backend, /getRebind\(name: string, clusterID: string/);
  assert.match(mock, /path\.startsWith\("\/api\/v1\/station\/"\)/);
  assert.match(mock, /Station is unavailable in mock mode/);
  assert.doesNotMatch(mock, /MockRebindApplied|station-uid-replacement|MockStationRebindOperation/);
});

test("Station details open in a dismissible drawer without auto-selecting a row", () => {
  assert.match(page, /className="addon-drawer station-drawer" role="dialog" aria-modal="true"/);
  assert.match(page, /event\.key === "Escape"\) closeDrawer\(\)/);
  assert.match(page, /className="addon-drawer-overlay" onClick=\{closeDrawer\}/);
  assert.doesNotMatch(page, /\?\? visible\[0\]/);
  assert.match(page, /PageToolbar/);
  assert.match(page, /stationNetworkState\(item\)/);
});
