import assert from "node:assert/strict";
import test from "node:test";
import { convertJobList } from "../dist/test/utils/crd.js";

function job(name) {
  return {
    metadata: { name },
    spec: {
      tasks: [{ role: "Actor", placement: {}, workload: { carrier: {} } }],
    },
  };
}

test("isolates incomplete and malformed jobs while retaining valid jobs", () => {
  const missingWorkload = job("missing-workload");
  delete missingWorkload.spec.tasks[0].workload;
  const missingPlacement = job("missing-placement");
  delete missingPlacement.spec.tasks[0].placement;
  const missingRole = job("missing-role");
  delete missingRole.spec.tasks[0].role;
  const missingCarrier = job("missing-carrier");
  delete missingCarrier.spec.tasks[0].workload.carrier;
  const malformed = job("malformed");
  malformed.spec.tags = [{ key: "tag", values: null }];

  const result = convertJobList([
    job("valid"), missingWorkload, missingPlacement, missingRole,
    missingCarrier, malformed, { metadata: { name: "empty" }, spec: {} },
  ]);
  assert.deepEqual(result.jobs.map((item) => item.name), ["valid", "empty"]);
  assert.deepEqual(result.invalidJobs, [
    "missing-workload", "missing-placement", "missing-role", "missing-carrier", "malformed",
  ]);
});

test("recovered data is shown on the next conversion", () => {
  const broken = job("recovered");
  delete broken.spec.tasks[0].workload;
  assert.deepEqual(convertJobList([broken]).invalidJobs, ["recovered"]);
  const result = convertJobList([job("recovered")]);
  assert.deepEqual(result.invalidJobs, []);
  assert.deepEqual(result.jobs.map((item) => item.name), ["recovered"]);
});
