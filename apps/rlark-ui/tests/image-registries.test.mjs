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
  assert.match(page, /registry-info-strip/);
  assert.match(browser, /"镜像"[\s\S]*"最新版本"[\s\S]*"更新时间"[\s\S]*"大小"[\s\S]*"平台"[\s\S]*"操作"/);
  assert.match(browser, /loadRegistryTagPreviews<RegistryTagDetailPage>/);
  assert.match(browser, /\(name, signal\) => fetchDetails\(name, "", signal\)/);
  assert.match(browser, /error instanceof ApiError && error\.status === 404/);
  assert.match(browser, /legacyTagDetails\(await imageRegistriesApi\.tags\(registryID/);
  assert.match(browser, /当前 Gateway 版本不支持读取镜像构建时间/);
  assert.match(browser, /groupRegistryVersions/);
  assert.match(browser, /registry-tag-chip/);
  assert.match(browser, /className="addon-drawer registry-drawer"[\s\S]*role="dialog"/);
  assert.match(browser, /event\.key === "Escape"\) onClose\(\)/);
  assert.doesNotMatch(browser, /registry-expanded-row/);
  assert.match(browser, /className=\{iconOnly \? "icon-button" : compact \? "table-action-button"/);
  assert.match(browser, /复制成功[\s\S]*复制失败/);
  assert.match(browser, /按 digest Pull/);
  assert.match(browserStyles, /\.registry-version-actions[\s\S]*?flex-wrap: wrap/);
  assert.match(browser, /标签加载失败/);
  assert.match(browser, /无法读取镜像信息/);
  assert.match(browser, /role="alert"/);
  assert.match(browser, /当前页没有匹配的镜像/);
  assert.match(browser, /Push \/ Pull 操作指南/);
  assert.match(browser, /registryCommands\(item\.registry, item\.username\)/);
});

test("registry tag details API targets the Gateway tag-details route", () => {
  assert.match(backend, /tagDetails\(id: string, repository: string, last = "", signal\?: AbortSignal\)/);
  assert.match(backend, /\/tag-details\?\$\{query\}/);
});
