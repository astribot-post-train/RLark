import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  imageRegistryReference,
  nextRegistryPage,
  previousRegistryPage,
} from "../dist/test/utils/registryPicker.js";

test("registry references use the selected configured address", () => {
  assert.equal(
    imageRegistryReference("10.11.14.128:5443", "team/image", "v3"),
    "10.11.14.128:5443/team/image:v3",
  );
  assert.equal(
    imageRegistryReference("other.example", "image", "latest"),
    "other.example/image:latest",
  );
});

test("repository and tag pages can advance, return, and reject repeated cursors", () => {
  let cursors = [""];
  cursors = nextRegistryPage(cursors, "repo-50");
  cursors = nextRegistryPage(cursors, "repo-100");
  assert.deepEqual(cursors, ["", "repo-50", "repo-100"]);
  assert.deepEqual(nextRegistryPage(cursors, "repo-50"), cursors);
  assert.deepEqual(nextRegistryPage(cursors, ""), cursors);
  cursors = previousRegistryPage(cursors);
  assert.deepEqual(cursors, ["", "repo-50"]);
  assert.deepEqual(previousRegistryPage(previousRegistryPage(cursors)), [""]);
});

test("picker separates unverified history from configured registries and exposes retry", () => {
  const source = readFileSync("src/components/RegistryImagePicker.tsx", "utf8");
  assert.doesNotMatch(source, /192\.168\.31\.2:5443|PRIVATE_REGISTRY/);
  assert.match(source, /最近使用（可能已失效/);
  assert.match(source, /imageRegistriesApi\s*\.list\(controller\.signal\)/);
  assert.match(
    source,
    /imageRegistriesApi\.repositories\(registryId, cursor, controller\.signal\)/,
  );
  assert.match(
    source,
    /imageRegistriesApi\.tags\(\s*registryId,\s*repository,\s*cursor,\s*controller\.signal,?\s*\)/,
  );
  assert.match(source, /controller\.abort\(\)/);
  assert.match(source, /role="alert"[\s\S]*重试/);
  assert.match(source, /管理员权限[\s\S]*手动输入/);
});
