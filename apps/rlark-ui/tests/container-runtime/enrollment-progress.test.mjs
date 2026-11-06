import assert from "node:assert/strict";
import { test } from "node:test";
import {
  currentEnrollment,
  enrollmentReasonText,
  lastFailedEnrollment,
} from "../../dist/test/admin/container-runtime/enrollment.js";

const op = (uid, phase, cancelRequested = false) => ({ uid, phase, cancelRequested });

// The console API lists enrollments newest first; the controller runs the
// oldest unfinished one.
test("current enrollment is the oldest unfinished operation; newer ones are queued", () => {
  const { active, queued } = currentEnrollment({
    enrollments: [
      op("third", ""),
      op("second", "Waiting"),
      op("cancelling", "Waiting", true),
      op("first", "Waiting"),
      op("done", "Succeeded"),
    ],
  });
  assert.equal(active?.uid, "first");
  assert.deepEqual(queued.map((o) => o.uid), ["third", "second"]);
});

test("the form explains the newest operation only when it failed", () => {
  assert.equal(
    lastFailedEnrollment({ enrollments: [op("new", "Failed"), op("old", "Succeeded")] })?.uid,
    "new",
  );
  assert.equal(
    lastFailedEnrollment({ enrollments: [op("new", "Succeeded"), op("old", "Failed")] }),
    undefined,
  );
  // A cancelled operation is the operator's own choice, not a failure to explain.
  assert.equal(
    lastFailedEnrollment({ enrollments: [op("c", "Cancelled", true), op("old", "Failed")] })?.uid,
    "old",
  );
});

test("no current enrollment when every operation is terminal or cancelling", () => {
  const { active, queued } = currentEnrollment({
    enrollments: [op("a", "Failed"), op("b", "Cancelled"), op("c", "Waiting", true)],
  });
  assert.equal(active, undefined);
  assert.deepEqual(queued, []);
});

test("enrollment reasons explain SSH reachability and keep unknown reasons verbatim", () => {
  assert.match(enrollmentReasonText("SSHUnavailable", true), /SSH 不可达.*安全组/);
  assert.match(enrollmentReasonText("SSHUnavailable", false), /unreachable.*security group/);
  assert.match(enrollmentReasonText("PrivateAddressChanged", true), /重新提交接入即可自动替换旧记录/);
  assert.match(enrollmentReasonText("ReplacementBlocked", true), /自动完成替换/);
  assert.match(enrollmentReasonText("SSHAddressDenied", true), /allowedWorkerCIDRs/);
  assert.equal(enrollmentReasonText("SomeNewReason", true), "SomeNewReason");
  assert.equal(enrollmentReasonText(undefined, false), "");
});

test("re-enrollment sends only the SSH endpoint; identity comes from the probe", async () => {
  const { enrollmentIntent } = await import(
    "../../dist/test/admin/container-runtime/enrollment.js"
  );
  const req = enrollmentIntent(
    "7c9e6679-7425-40de-944b-e07fc1f90ae7",
    "cluster",
    "",
    [
      { name: "ksyun-a", role: "Gateway", address: "36.103.180.181", port: 10001, nodeUID: "gw", privateAddress: "10.1.3.214" },
      { name: "ksyun-b", role: "Worker", address: "10.0.3.85", port: 22, nodeUID: "stale", privateAddress: "10.1.2.91" },
    ],
  );
  // The gateway keeps its record so a running gateway is reused.
  assert.deepEqual(req.nodes[0], { name: "ksyun-a", role: "Gateway", address: "36.103.180.181", port: 10001, nodeUID: "gw" });
  // A restarted worker must not carry its previous identity.
  assert.deepEqual(req.nodes[1], { name: "ksyun-b", role: "Worker", address: "10.0.3.85", port: 22 });
});
