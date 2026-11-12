import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const page = readFileSync("src/pages/Stations.tsx", "utf8");
const backend = readFileSync("src/backend.ts", "utf8");
const mock = readFileSync("src/mockBackend.ts", "utf8");

test("Station rebind is available for offline or conflicting bindings and uses verified observations", () => {
  assert.match(page, /stationPhase\(selected\) === "Conflict" \|\| stationPhase\(selected\) === "Offline"/);
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

test("node onboarding uses a confirmed host fingerprint and polls its durable operation", () => {
  assert.match(page, /stationsApi\.createNodeOperation\(/);
  assert.match(page, /host_key_fingerprint: joinForm\.fingerprint\.trim\(\)/);
  assert.match(page, /stationsApi\.getNodeOperation\(joinOperationName, joinClusterID/);
  assert.match(page, /stationsApi\.listNodeOperations\(clusterID/);
  assert.match(page, /setShowNodeOperations/);
  assert.match(page, /operation\.status\?\.phase === "Succeeded"/);
  assert.match(page, /reset_existing_kubeadm: true/);
  assert.match(page, /containerd_runtime: joinForm\.containerdRuntime/);
  assert.match(page, /\/run\/containerd-fleet\/containerd\.sock/);
  assert.match(page, /Reset stale kubeadm state before joining/);
  assert.match(page, /credentials are stored only for this short-lived operation|凭证只为本次操作短期保存/i);
  assert.match(page, /condition\.reason === "StationRebindRequired"/);
  assert.match(page, /target_node_uid: operation\.status\.nodeUID/);
  assert.match(page, /Approve rebind and continue Join/);
  assert.match(backend, /listNodeOperations\(clusterID: string/);
});

test("node removal submits the displayed Node UID and requires explicit confirmation", () => {
  assert.match(page, /const openRemove = \(station: StationItem\)/);
  assert.match(page, /node_uid: joinForm\.nodeUID/);
  assert.match(page, /checked=\{removeConfirmed\}/);
  assert.match(page, /joinAction === "Remove" && \(!joinForm\.nodeUID \|\| !removeConfirmed\)/);
  assert.match(page, /station\.status\?\.phase === "Conflict"/);
});

test("Station details open in a dismissible drawer without auto-selecting a row", () => {
  assert.match(page, /className="addon-drawer station-drawer" role="dialog" aria-modal="true"/);
  assert.match(page, /event\.key === "Escape"\) closeDrawer\(\)/);
  assert.match(page, /className="addon-drawer-overlay" onClick=\{closeDrawer\}/);
  assert.doesNotMatch(page, /\?\? visible\[0\]/);
  assert.match(page, /PageToolbar/);
  assert.match(page, /stationNetworkState\(item\)/);
});
