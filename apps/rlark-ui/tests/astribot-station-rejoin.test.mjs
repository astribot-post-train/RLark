import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  planStationRejoin, rejoinPlanReason, stationRejoinFailure, stationRejoinProgress,
} from "../dist/test/astribot/lib/stationRejoin.js";

const api = await readFile(new URL("../src/astribot/api.ts", import.meta.url), "utf8");
const hook = await readFile(new URL("../src/astribot/components/useStationRejoin.ts", import.meta.url), "utf8");
const page = await readFile(new URL("../src/astribot/pages/Stations.tsx", import.meta.url), "utf8");
const card = await readFile(new URL("../src/astribot/pages/StationCard.tsx", import.meta.url), "utf8");

const admission = (join_phase, join_stage) => ({ join_phase, join_stage });
const join = (phase, stage, stages) => ({
  apiVersion: "rlinf.io/v1alpha1",
  kind: "NodeJoin",
  spec: { clusterID: "rlark-robot-s1u-01", nodeName: "s1u-robot", machine: { address: "10.0.0.5", port: 22, user: "root" } },
  status: { phase, stage, stages },
});

test("automatic rejoin retries only an existing Failed join at a stage that does not log in", () => {
  const clusterID = "rlark-robot-s1u-01";
  const nodeName = "s1u-robot";
  // After install the stages run in the management plane: no password is needed, so a retry is safe.
  for (const stage of ["node-ready", "station", "gpu"]) {
    assert.deepEqual(planStationRejoin({ clusterID, nodeName, admission: admission("Failed", stage) }),
      { kind: "retry", name: "rlark-robot-s1u-01.s1u-robot", stage }, stage);
  }
  // Up to install a retry logs in to the machine, whose password is never stored.
  for (const stage of ["precheck", "bundle", "declare", "tunnel", "install"]) {
    assert.deepEqual(planStationRejoin({ clusterID, nodeName, admission: admission("Failed", stage) }),
      { kind: "needs-password", stage }, stage);
  }
});

test("a join that is running, succeeded, absent, stage-less or cluster-less is never retried", () => {
  assert.deepEqual(planStationRejoin({ clusterID: "c", nodeName: "n", admission: admission("Running", "node-ready") }),
    { kind: "running", stage: "node-ready" });
  assert.deepEqual(planStationRejoin({ clusterID: "c", nodeName: "n", admission: admission("Succeeded", "gpu") }), { kind: "succeeded" });
  assert.deepEqual(planStationRejoin({ clusterID: "c", nodeName: "n", admission: undefined }), { kind: "no-record" });
  assert.deepEqual(planStationRejoin({ clusterID: "c", nodeName: "n" }), { kind: "no-record" });
  // A stage-less failure fails closed: the console cannot prove the retry needs no machine login.
  assert.deepEqual(planStationRejoin({ clusterID: "c", nodeName: "n", admission: admission("Failed", "") }), { kind: "no-stage" });
  assert.deepEqual(planStationRejoin({ clusterID: "c", nodeName: "n", admission: { join_phase: "Pending" } }), { kind: "no-record" });
  assert.deepEqual(planStationRejoin({ nodeName: "n", admission: admission("Failed", "node-ready") }), { kind: "no-cluster" });
  assert.deepEqual(planStationRejoin({ clusterID: "c", admission: admission("Failed", "node-ready") }), { kind: "no-cluster" });
});

test("every plan that cannot retry says why and points at the manual flow", () => {
  assert.equal(rejoinPlanReason({ kind: "retry", name: "c.n", stage: "node-ready" }, true), "");
  const password = rejoinPlanReason({ kind: "needs-password", stage: "install" }, true);
  assert.match(password, /「入网码兑换和安装」/);
  assert.match(password, /口令不会保存/);
  assert.match(rejoinPlanReason({ kind: "needs-password", stage: "install" }, false), /SSH password is never stored/i);
  assert.match(rejoinPlanReason({ kind: "succeeded" }, true), /不会为它新建记录/);
  assert.match(rejoinPlanReason({ kind: "running" }, true), /不能重复提交/);
  assert.match(rejoinPlanReason({ kind: "no-record" }, true), /不会新建记录/);
  assert.match(rejoinPlanReason({ kind: "no-stage" }, true), /无法确认/);
  assert.match(rejoinPlanReason({ kind: "no-cluster" }, true), /手动重新接入/);
});

test("a failed retry shows the failed stage and the reason the Gateway wrote", () => {
  const failed = stationRejoinFailure(join("Failed", "station", [
    { name: "node-ready", state: "Done" },
    { name: "station", state: "Failed", message: "3 分钟后入网登记是 \"Pending\"（看节点上 station-agent 的日志）" },
  ]), true);
  assert.match(failed, /「入网登记」/);
  assert.match(failed, /station-agent/);
  assert.match(stationRejoinFailure(join("Failed", "gpu"), true), /没有写明失败原因/);
  assert.match(stationRejoinFailure(undefined, true), /读不到/);
});

test("following a retry names the stage and its latest progress", () => {
  const progress = stationRejoinProgress(join("Running", "node-ready", [
    { name: "node-ready", state: "Running", message: "等待 kubelet Ready" },
  ]), true);
  assert.match(progress, /「节点 Ready」/);
  assert.match(progress, /等待 kubelet Ready/);
  assert.match(stationRejoinProgress(join("Succeeded", "gpu"), true), /接入已恢复/);
});

test("the automatic API call sends no credentials and the hook refuses a duplicate submission", () => {
  // retryStored posts an empty body: no password, no user, no field that could create or duplicate a record.
  assert.match(api, /async retryStored\(name: string\) \{[\s\S]*?body: \{\},/);
  const stored = api.slice(api.indexOf("async retryStored"), api.indexOf("async retry(name"));
  assert.doesNotMatch(stored, /password|user:/);
  // The synchronous in-flight set stops a second click before React state re-renders.
  assert.match(hook, /busy\.current\.has\(target\.key\)/);
  assert.match(hook, /busy\.current\.add\(target\.key\)/);
  assert.match(hook, /nodeJoinsApi\.retryStored\(target\.name\)/);
  // Following reads the cluster-scoped list, not an unscoped by-name read.
  assert.match(hook, /nodeJoinsApi\.list\(target\.clusterID\)/);
  // No retry loop: a failed attempt stops and hands the next step to the operator.
  assert.match(hook, /phase: "failed"/);
  assert.match(hook, /请手动重新接入/);
  assert.doesNotMatch(hook, /password:/);
});

test("the card's primary button never navigates; the manual page is only an explicit fallback", () => {
  // The page wires the primary press to the retry plan and the manual callback separately.
  assert.match(page, /rejoinPlan = planStationRejoin\(/);
  assert.match(page, /rejoin\.start\(\{/);
  assert.match(page, /rejoin\.explain\(key, rejoinPlanReason\(rejoinPlan, zh\)\)/);
  assert.match(page, /manual: \(\) => manualRejoin\(item\)/);
  // The card only renders the manual button after a failure, an impossible request or an unfinished wait.
  assert.match(card, /rejoin\.state\.phase === "failed" \|\| rejoin\.state\.phase === "unavailable" \|\| rejoin\.state\.phase === "timed-out"/);
  assert.match(card, /station-tile-rejoin-manual/);
  assert.match(card, /disabled=\{recheck\.pending \|\| rejoin\.pending\}/);
  // The old promise that the card never joins is gone; the tooltip now describes the automatic retry.
  assert.doesNotMatch(card, /不会自动入网或改动记录/);
  assert.match(card, /自动接入：复用这条节点已保存的 SSH 接入记录/);
});
