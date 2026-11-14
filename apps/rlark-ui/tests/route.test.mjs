import assert from "node:assert/strict";
import test from "node:test";

import {
  filesPath,
  isAdminPath,
  parseAdminRoute,
} from "../dist/test/utils/route.js";

test("admin routes preserve the storage class file browser context", () => {
  assert.equal(filesPath("shared/data"), "/files/shared%2Fdata");
  assert.equal(filesPath("shared/data", true), "/admin/files/shared%2Fdata");
  assert.deepEqual(parseAdminRoute("/admin/files/shared%2Fdata"), {
    page: "files",
    sub: "shared/data",
  });
});

test("admin path matching observes a route boundary", () => {
  assert.equal(isAdminPath("/admin"), true);
  assert.equal(isAdminPath("/admin/jobs"), true);
  assert.equal(isAdminPath("/administrator"), false);
});
