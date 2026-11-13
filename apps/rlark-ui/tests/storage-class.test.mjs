import assert from "node:assert/strict";
import test from "node:test";
import {
  normalizeStorageClass,
  storageClassToForm,
} from "../dist/test/utils/storageClass.js";

test("mount endpoint survives API normalization and edit submission", () => {
  const item = normalizeStorageClass({
    name: "cloud",
    endpoint: "https://public.example.com",
    mount_endpoint: "https://internal.example.com",
  });
  const form = storageClassToForm(item);
  assert.equal(form.endpoint, "https://public.example.com");
  assert.equal(form.mount_endpoint, "https://internal.example.com");
  assert.equal(
    storageClassToForm(normalizeStorageClass({ ...form, mount_endpoint: "" }))
      .mount_endpoint,
    "",
  );
});

test("bucket prefix and VFS cache mode survive form normalization", () => {
  const item = normalizeStorageClass({
    name: "fleet-ks3",
    prefix: "ks3-regular",
    vfs_cache_mode: "off",
  });
  const form = storageClassToForm(item);
  assert.equal(form.prefix, "ks3-regular");
  assert.equal(form.vfs_cache_mode, "off");
  assert.equal(storageClassToForm().vfs_cache_mode, "writes");
  assert.equal(normalizeStorageClass({}).prefix, "");
});

test("legacy and new forms default to an empty mount override", () => {
  assert.equal(storageClassToForm().mount_endpoint, "");
  const legacy = normalizeStorageClass({
    endpoint: "https://public.example.com",
  });
  assert.equal(legacy.mount_endpoint, "");
  assert.equal(
    storageClassToForm(legacy).endpoint,
    "https://public.example.com",
  );
});

test("mount profiles, overrides and effective options survive editing", () => {
  const raw = { name: "archive", mount_profile: "archive", mount_options: ["tpslimit=2"], effective_mount_options: ["vfs-cache-mode=off"], effective_backend_options: {chunk_size: "32Mi"}, vfs_cache_mode: "off" };
  const item = normalizeStorageClass(raw);
  const form = storageClassToForm(item);
  assert.equal(form.mount_profile, "archive");
  assert.deepEqual(form.mount_options, ["tpslimit=2"]);
  form.mount_options.push("low-level-retries=10");
  assert.deepEqual(item.mount_options, ["tpslimit=2"]);
  assert.deepEqual(item.effective_backend_options, {chunk_size: "32Mi"});
  assert.equal(storageClassToForm().mount_profile,"default");
});
