import assert from "node:assert/strict";
import test from "node:test";

import {
  clearRejoinObservations,
  describeRejoin,
  isRobotRejoinOperation,
  machinesForRejoin,
  newRejoinRequestID,
  pickRejoinOperation,
  publishRejoinObservation,
  readOperationPages,
  readRejoinObservation,
  readRejoinTracks,
  rejoinBusy,
  rejoinCoarseKey,
  rejoinConditionReason,
  rejoinObservation,
  rejoinStatusKey,
  robotModelForRejoin,
  subscribeRejoinObservation,
  writeRejoinTrack,
} from "../dist/test/astribot/lib/robotRejoinTask.js";

const operation = (name, nodeName, clusterID, phase, extra = {}) => ({
  metadata: { name, creationTimestamp: extra.created ?? "2026-11-27T06:00:00Z" },
  spec: { action: "Join", nodeName, clusterID, requestID: extra.requestID ?? `rejoin-${name}` },
  status: { phase, stage: extra.stage, conditions: extra.conditions },
});

test("a rejoin task is kept on its own cluster, node and operation id", () => {
  const now = Date.parse("2026-11-27T07:00:00Z");
  const ops = [
    operation("join-rejoin-aaaa", "s1u-02-x86", "cluster-a", "Running", { stage: "Install", requestID: "rejoin-card1" }),
    operation("join-rejoin-bbbb", "s1u-01-x86", "cluster-a", "Failed", { requestID: "rejoin-card2", conditions: [{ type: "Failed", status: "False", reason: "EntryUnreachable", message: "hub silent" }] }),
    operation("join-rejoin-cccc", "s1u-02-x86", "cluster-b", "Running", { requestID: "rejoin-card3" }),
    operation("node-op-dddd", "s1u-02-x86", "cluster-a", "Running", { requestID: "remove-1" }),
  ];
  ops[3].spec.action = "Remove";
  const picked = pickRejoinOperation(ops, "cluster-a", "s1u-02-x86", undefined, now, false);
  assert.equal(picked.metadata.name, "join-rejoin-aaaa");
  assert.equal(pickRejoinOperation(ops, "cluster-a", "s1u-01-x86", "join-rejoin-bbbb", now, true).metadata.name, "join-rejoin-bbbb");
  assert.equal(pickRejoinOperation(ops, "cluster-b", "s1u-02-x86", undefined, now, false).metadata.name, "join-rejoin-cccc");
  assert.equal(pickRejoinOperation(ops, "cluster-a", "s1u-09-x86", undefined, now, false), undefined);
  assert.equal(isRobotRejoinOperation(ops[3], "cluster-a", "s1u-02-x86"), false);
});

test("progress, success and failure use the operation's own phase and condition", () => {
  const running = describeRejoin("Running", "Install", "", true);
  assert.match(running.text, /重接进行中/);
  assert.match(running.text, /安装并入网/);
  assert.equal(running.alert, false);
  const done = describeRejoin("Succeeded", "VerifyAddons", "", false);
  assert.equal(done.text, "Rejoin finished");
  const failed = operation("join-rejoin-bbbb", "s1u-01-x86", "cluster-a", "Failed", {
    conditions: [
      { type: "Ready", status: "True", reason: "Ignored", message: "not the failure" },
      { type: "Failed", status: "False", reason: "EntryUnreachable", message: "the entry device did not answer" },
    ],
  });
  const reason = rejoinConditionReason(failed);
  assert.match(reason, /EntryUnreachable/);
  assert.match(reason, /the entry device did not answer/);
  const view = describeRejoin("Failed", "Preflight", reason, true);
  assert.equal(view.alert, true);
  assert.match(view.text, /重接失败：EntryUnreachable: the entry device did not answer/);
  const blocked = describeRejoin("Blocked", "", "credentials missing", false);
  assert.match(blocked.text, /Rejoin blocked: credentials missing/);
});

test("the remembered task is per cluster and node, and a request id is a DNS label", () => {
  const saved = [];
  const storage = {
    getItem: () => JSON.stringify(saved),
    setItem: (_key, value) => { saved.splice(0, saved.length, ...JSON.parse(value)); },
  };
  writeRejoinTrack(storage, { clusterID: "cluster-a", nodeName: "s1u-02-x86", machineName: "m1", requestID: "card1", operationName: "join-rejoin-aaaa" });
  writeRejoinTrack(storage, { clusterID: "cluster-a", nodeName: "s1u-02-x86", machineName: "m1", requestID: "card2", operationName: "join-rejoin-bbbb" });
  writeRejoinTrack(storage, { clusterID: "cluster-b", nodeName: "s1u-02-x86", machineName: "m9", requestID: "card3", operationName: "join-rejoin-cccc" });
  const tracks = readRejoinTracks(storage);
  assert.equal(tracks.length, 2);
  assert.equal(tracks.find((track) => track.clusterID === "cluster-a").operationName, "join-rejoin-bbbb");
  assert.equal(tracks.find((track) => track.clusterID === "cluster-b").operationName, "join-rejoin-cccc");
  writeRejoinTrack(storage, { clusterID: "cluster-a", nodeName: "s1u-02-x86", machineName: "m1", requestID: "card4", operationName: "not-a-rejoin" });
  assert.equal(readRejoinTracks(storage).find((track) => track.clusterID === "cluster-a").operationName, "join-rejoin-bbbb");
  const id = newRejoinRequestID(1_700_000_000_000, 0.5);
  assert.match(id, /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/);
  assert.ok(id.length <= 63);
  assert.equal(robotModelForRejoin("S1U"), "s1u");
  assert.equal(robotModelForRejoin("not a model"), undefined);
});

test("only one joined machine of this cluster and node is eligible", () => {
  const machines = [
    { name: "m1", phase: "Joined", node_name: "s1u-02-x86", machine_type: "Robot", cluster_id: "cluster-a" },
    { name: "m2", phase: "Joined", node_name: "s1u-02-x86", machine_type: "Robot", cluster_id: "cluster-b" },
    { name: "m3", phase: "Discovered", node_name: "s1u-02-x86", machine_type: "Robot", cluster_id: "cluster-a" },
    { name: "m4", phase: "Joined", node_name: "s1u-02-x86", machine_type: "Server", cluster_id: "cluster-a" },
  ];
  assert.deepEqual(machinesForRejoin(machines, "cluster-a", "s1u-02-x86").map((machine) => machine.name), ["m1"]);
  assert.deepEqual(machinesForRejoin(machines, "cluster-c", "s1u-02-x86"), []);
});

test("an operation that does not name this cluster is not this card's task", () => {
  const unnamed = operation("join-rejoin-eeee", "s1u-02-x86", undefined, "Running", { requestID: "rejoin-card9" });
  assert.equal(isRobotRejoinOperation(unnamed, "cluster-a", "s1u-02-x86"), false);
  assert.equal(pickRejoinOperation([unnamed], "cluster-a", "s1u-02-x86", "join-rejoin-eeee", Date.parse("2026-11-27T07:00:00Z"), true), undefined);
});

test("list pages follow continue and stop when the token repeats", async () => {
  const calls = [];
  const items = await readOperationPages(async (pageToken) => {
    calls.push(pageToken);
    if (!pageToken) return { items: [operation("join-rejoin-aaaa", "n", "c", "Running")], continue: "page-2" };
    return { items: [operation("join-rejoin-bbbb", "n", "c", "Failed")], continue: "page-2" };
  });
  assert.deepEqual(calls, [undefined, "page-2"]);
  assert.deepEqual(items.map((item) => item.metadata.name), ["join-rejoin-aaaa", "join-rejoin-bbbb"]);
});

test("the accept sketch is not a phase, and a stage change does not change the page state key", () => {
  const track = { clusterID: "cluster-a", nodeName: "s1u-02-x86", machineName: "m1", requestID: "card", operationName: "join-rejoin-aaaa" };
  const reading = rejoinObservation({ track, confirmed: false }, true);
  assert.match(reading.text, /正在读取 join-rejoin-aaaa/);
  assert.equal(reading.alert, false);
  const preflight = operation("join-rejoin-aaaa", "s1u-02-x86", "cluster-a", "Running", { stage: "Preflight", requestID: "rejoin-card" });
  const install = operation("join-rejoin-aaaa", "s1u-02-x86", "cluster-a", "Running", { stage: "Install", requestID: "rejoin-card" });
  assert.equal(rejoinCoarseKey({ track, operation: preflight, confirmed: true }), rejoinCoarseKey({ track, operation: install, confirmed: true }));
  assert.notEqual(rejoinStatusKey(preflight), rejoinStatusKey(install));
  assert.match(rejoinObservation({ track, operation: install, confirmed: true }, true).text, /安装并入网/);
  assert.equal(rejoinBusy({ track, confirmed: false }), true);
  assert.equal(rejoinBusy({ track, operation: install, confirmed: true }), true);
  assert.equal(rejoinBusy({ error: "重接没有提交：没有 Machine" }), false);
  const failed = operation("join-rejoin-aaaa", "s1u-02-x86", "cluster-a", "Failed", {
    requestID: "rejoin-card",
    conditions: [{ type: "Failed", status: "False", reason: "EntryUnreachable", message: "hub silent" }],
  });
  assert.equal(rejoinBusy({ track, operation: failed, confirmed: true }), false);
  const failedView = rejoinObservation({ track, operation: failed, confirmed: true }, true);
  assert.equal(failedView.alert, true);
  assert.match(failedView.text, /EntryUnreachable: hub silent/);
});

test("a stage update is published only to that card", () => {
  clearRejoinObservations();
  const seen = [];
  const stopA = subscribeRejoinObservation("ns/a", () => seen.push(readRejoinObservation("ns/a")?.text ?? ""));
  const stopB = subscribeRejoinObservation("ns/b", () => seen.push("b"));
  const view = { phase: "Running", text: "重接进行中 · 安装并入网", title: "重接进行中 · 安装并入网", alert: false };
  publishRejoinObservation("ns/a", view);
  publishRejoinObservation("ns/a", view);
  assert.deepEqual(seen, ["重接进行中 · 安装并入网"]);
  assert.equal(readRejoinObservation("ns/b"), undefined);
  stopA();
  stopB();
  clearRejoinObservations();
});
