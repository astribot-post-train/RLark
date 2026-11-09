import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const page = readFileSync("src/pages/ImageRegistries.tsx", "utf8");
const admin = readFileSync("src/admin/AdminApp.tsx", "utf8");
const mock = readFileSync("src/mockBackend.ts", "utf8");
const backend = readFileSync("src/backend.ts", "utf8");
const browser = readFileSync("src/pages/RegistryBrowser.tsx", "utf8");
const browserStyles = readFileSync("src/pages/RegistryBrowser.css", "utf8");

test("image registries use immutable IDs for routes and mutations", () => {
  assert.match(page, /key=\{item\.id\}/);
  assert.match(page, /onSelect\?\.\(item\.id\)/);
  assert.match(page, /imageRegistriesApi\.remove\(item\.id\)/);
  assert.match(backend, /image-registries\/\$\{encodeURIComponent\(id\)\}/);
  assert.match(admin, /selectedID=\{adminSub \|\| undefined\}/);
});

test("image registry forms submit explicit distribution scope", () => {
  assert.match(page, /"None" \| "Selected" \| "All"/);
  assert.match(page, /clusterSelection: \{ mode: "All", clusters: \[\] \}/);
  assert.match(page, /clusterSelection: form\.clusterSelection/);
  assert.match(page, /form\.password \? \{ password: form\.password \} : \{\}/);
});

test("mock backend implements ID-based image registry CRUD", () => {
  assert.match(mock, /path === "\/api\/v1\/image-registries"/);
  assert.match(mock, /item\.id === id/);
  assert.match(mock, /return json\(\{ ok: true \}, 202\)/);
});

test("registry detail exposes image table, states and push/pull guidance", () => {
  assert.match(page, /<RegistryBrowser/);
  assert.match(browser, /镜像名称[\s\S]*"标签"[\s\S]*操作/);
  assert.match(browser, /loadRegistryTagPreviews/);
  assert.match(browser, /registry-tag-chip/);
  assert.match(browser, /registry-expanded-row[\s\S]*<RegistryTags/);
  assert.match(browser, /registry-tag-row-main[\s\S]*registry-tag-actions/);
  assert.match(browser, /aria-expanded=\{selectedTag === tag\}/);
  assert.match(browser, /className=\{compact \? "table-action-button"/);
  assert.match(browser, /复制成功[\s\S]*复制失败/);
  assert.match(browserStyles, /\.registry-tag-actions[\s\S]*?flex-wrap: wrap/);
  assert.match(browser, /标签加载失败/);
  assert.match(browser, /查看摘要/);
  assert.match(browser, /role="alert"/);
  assert.match(browser, /当前页没有匹配的镜像/);
  assert.match(browser, /Push \/ Pull 操作指南/);
  assert.match(browser, /registryCommands\(item\.registry, item\.username\)/);
});
