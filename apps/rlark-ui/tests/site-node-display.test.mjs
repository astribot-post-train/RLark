import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  admissionStateClass,
  admissionStateLabel,
  keyFingerprint,
  machineModel,
  parseLabelLines,
  parseTaintLines,
  s1uRobotLabels,
} from "../dist/test/utils/siteNodeDisplay.js";

test("a WireGuard key shows as the beginning wg show prints", () => {
  assert.equal(keyFingerprint("hG3m0e1yZr0u8m5h2a+4yR8Hn0Lz3pQkV2c7xW9eJ1s="), "hG3m0e1yZr…");
  assert.equal(keyFingerprint(undefined), "");
});

test("the machine's model comes from its robot model, else its category", () => {
  assert.equal(machineModel({ "rlark.io/robot-model": "s1u", "rlark.io/node-category": "robot" }), "s1u");
  assert.equal(machineModel({ "rlark.io/node-category": "gpu" }), "gpu");
  assert.equal(machineModel(undefined), "");
});

test("label and taint lines are read one per line and a bad line is named", () => {
  assert.deepEqual(parseLabelLines(s1uRobotLabels("s1u-09")).labels, {
    "rlark.io/node-category": "robot", "rlark.io/robot-model": "s1u", "rlark.io/robot-body": "s1u", "rlark.io/robot-id": "s1u-09",
  });
  assert.equal(parseLabelLines("a=b\n\nnovalue").error, "novalue");
  assert.deepEqual(parseTaintLines("rlark.io/node-category=robot:NoSchedule\nk:NoExecute").taints, ["rlark.io/node-category=robot:NoSchedule", "k:NoExecute"]);
  assert.equal(parseTaintLines("k=v").error, "k=v");
});

test("every admission state has a pill and a label", () => {
  for (const state of ["Pending", "Redeemed", "Joined", "Expired"]) {
    assert.match(admissionStateClass(state), /^status-/);
    assert.ok(admissionStateLabel(state, true));
  }
});

test("the code is shown once, in the answer of issue or reissue, and never listed", () => {
  const drawer = readFileSync("src/pages/JoinNodeDrawer.tsx", "utf8");
  const page = readFileSync("src/pages/Stations.tsx", "utf8");
  const backend = readFileSync("src/backend.ts", "utf8");
  assert.match(drawer, /只显示这一次/);
  assert.match(drawer, /<CopyButton value=\{issued\.command\}/);
  assert.match(page, /nodeAdmissionsApi\.reissue\(admission\.node_name, admission\.cluster_id\)/);
  assert.match(page, /admission\.state === "Pending" && <button/);
  assert.doesNotMatch(page, /admission\.code|codeHash|code_hash/);
  assert.match(backend, /"\/api\/v1\/station\/node-admissions"/);
});
