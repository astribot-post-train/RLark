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
  assert.match(enrollmentReasonText("PrivateAddressChanged", true), /不会自动接管/);
  assert.match(enrollmentReasonText("SSHAddressDenied", true), /allowedWorkerCIDRs/);
  assert.equal(enrollmentReasonText("SomeNewReason", true), "SomeNewReason");
  assert.equal(enrollmentReasonText(undefined, false), "");
});
