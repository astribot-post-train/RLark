import assert from "node:assert/strict";
import test from "node:test";
import { machinesForRejoin } from "../dist/test/astribot/lib/robotRejoinTask.js";

const machine = (fields = {}) => ({
  name: "machine-s1u-02",
  site: "shenzhen",
  node_class: "s1u",
  machine_type: "Robot",
  phase: "Joined",
  admissible: false,
  joining: false,
  cluster_id: "rlark-robot-s1u-01",
  node_name: "s1u-02-x86",
  ...fields,
});

test("rejoin selects joined machines and machines whose failed rejoin can be retried", () => {
  const candidates = machinesForRejoin([
    machine(),
    machine({ phase: "Admitted", join: { phase: "Blocked" } }),
    machine({ phase: "Admitted", join: { phase: "Failed" } }),
    machine({ phase: "Admitted", join: { phase: "Running" } }),
    machine({ phase: "Admitted" }),
    machine({ phase: "Admitted", join: { phase: "Blocked" }, cluster_id: "other-cluster" }),
    machine({ phase: "Admitted", join: { phase: "Blocked" }, machine_type: "Server" }),
  ], "rlark-robot-s1u-01", "s1u-02-x86");

  assert.deepEqual(candidates.map(({ phase, join }) => [phase, join?.phase]), [
    ["Joined", undefined],
    ["Admitted", "Blocked"],
    ["Admitted", "Failed"],
  ]);
});
