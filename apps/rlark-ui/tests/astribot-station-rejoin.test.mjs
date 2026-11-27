import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { assessRobotRejoin } from "../dist/test/astribot/lib/robotRejoin.js";

const page = await readFile(new URL("../src/astribot/pages/Stations.tsx", import.meta.url), "utf8");
const card = await readFile(new URL("../src/astribot/pages/StationCard.tsx", import.meta.url), "utf8");
const api = await readFile(new URL("../src/astribot/api.ts", import.meta.url), "utf8");
const joinNode = await readFile(new URL("../src/astribot/pages/JoinNode.tsx", import.meta.url), "utf8");

const assess = (admission, extra = {}) => assessRobotRejoin({
  zh: true, stationPhase: "Offline", siteDataState: "ready", admission, ...extra,
});
const admission = (state, fields = {}) => ({ state, ...fields });

test("an unreadable or still-loading site view is reported as unknown, not as a reconnect path", () => {
  const result = assess(undefined, { siteDataState: "unavailable" });
  assert.equal(result.kind, "no-data");
  assert.match(result.message, /没有可安全触发的远程重连命令/);
  const loading = assess(undefined, { siteDataState: "loading" });
  assert.equal(loading.kind, "no-data");
  assert.match(loading.message, /还在加载/);
  assert.doesNotMatch(loading.message, /没有这台机器人的入网记录/);
});

test("an identity conflict is never auto-applied under the old configuration", () => {
  assert.equal(assess(admission("Joined"), { identityVerified: "False" }).kind, "identity-conflict");
  assert.equal(assess(admission("Joined"), { stationPhase: "Conflict" }).kind, "identity-conflict");
  assert.match(assess(admission("Joined"), { stationPhase: "Conflict" }).message, /不能按旧配置自动重连/);
});

test("a robot carrying a Server SSH join record is told not to use that entry", () => {
  const result = assess(admission("Joined", { join_phase: "Failed", join_stage: "install" }));
  assert.equal(result.kind, "server-record");
  assert.match(result.message, /SSH 入网只支持 Server/);
  assert.match(result.message, /入网码兑换和安装/);
  assert.match(result.message, /扫描发现/);
});

test("a joined robot that is offline recovers by self-heal; the console starts nothing", () => {
  const result = assess(admission("Joined", { self_healed: "节点移除后又以同一个 Node 入网" }));
  assert.equal(result.kind, "joined-offline");
  assert.match(result.message, /已经入网/);
  assert.match(result.message, /station-agent 恢复上报后会自动恢复/);
  assert.match(result.message, /没有可幂等触发的重连接口/);
  assert.match(result.message, /不会替你发起新的接入/);
  assert.match(result.message, /RLark 已自动处理过：节点移除后又以同一个 Node 入网/);
});

test("a stopped robot join names the Machine/NodeClass path and does not pretend it is idempotent", () => {
  const result = assess(admission("Redeemed", { reason: "3 分钟后节点还不是 Ready" }));
  assert.equal(result.kind, "join-stopped");
  assert.match(result.message, /Machine 与 NodeClass 入网/);
  assert.match(result.message, /机型凭据保存在 RLark/);
  assert.match(result.message, /不能当成幂等重连盲目触发/);
  assert.match(result.message, /原因：3 分钟后节点还不是 Ready/);
  assert.equal(assess(admission("Redeemed", { stuck: true })).kind, "join-stopped");
  assert.equal(assess(admission("SomeState")).kind, "join-stopped");
});

test("a code still waiting or expired points at reissue, not at an automatic command", () => {
  const pending = assess(admission("Pending"));
  assert.equal(pending.kind, "pending");
  assert.match(pending.message, /重发入网码/);
  assert.match(pending.message, /不能代替机器执行入网命令/);
  const expired = assess(admission("Expired"));
  assert.equal(expired.kind, "expired");
  assert.match(expired.message, /已过期或作废/);
  assert.equal(assess(undefined).kind, "no-admission");
  assert.match(assess(undefined).message, /没有这台机器人的入网记录/);
});

test("the card's Rejoin explains and never navigates; the manual entry is Add node's machine workspace", () => {
  // The page computes the assessment from data it already holds and keeps it on the card.
  assert.match(page, /assessRobotRejoin\(\{/);
  assert.match(page, /setRejoinNotice\(\(current\) => \(\{ \.\.\.current, \[key\]: assessment\.message \}\)\)/);
  // The explicit manual entry opens the machine workspace with the cluster preselected and no SSH prefill.
  assert.match(page, /const manualRobotRejoin = \(item: StationItem\) =>/);
  assert.match(page, /onJoin\(undefined, \{ \.\.\.\(cluster \? \{ clusterID: cluster\.id \} : \{\}\), method: "machines" \}\)/);
  // The robot card path has no NodeJoin call at all.
  assert.doesNotMatch(page, /nodeJoinsApi/);
  assert.doesNotMatch(api, /retryStored/);
  // The primary button only presses the assessment; the manual button appears after the notice.
  assert.match(card, /onClick=\{rejoin\.press\}/);
  assert.match(card, /onClick=\{rejoin\.manual\}/);
  assert.match(card, /rejoin\.notice && <button[\s\S]*?station-tile-rejoin-manual/);
  assert.doesNotMatch(card, /rejoin\.pending/);
  assert.match(page, /机器人不通过 SSH 入网/);
});

test("the add-node page honours the machine-workspace target without pre-filling SSH", () => {
  // A target with method "machines" opens the scan/machine workspace and still selects its cluster.
  assert.match(joinNode, /sshJoin\?\.method \?\? \(sshJoin \? "ssh" : "machines"\)/);
  assert.match(joinNode, /useState\(reissued\?\.admission\.cluster_id \?\? sshJoin\?\.clusterID \?\? ""\)/);
});
