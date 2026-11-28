import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { cameraMediaScheduler, cameraWhepScheduler, createCameraMediaScheduler } from "../dist/test/astribot/lib/cameraMediaScheduler.js";
import { cameraTransportPlan } from "../dist/test/astribot/lib/cameraWhep.js";

test("higher priorities preempt, and release wakes the highest priority FIFO waiter", () => {
  const scheduler = createCameraMediaScheduler({ maxLiveStreams: 1 });
  const changes = new Map();
  const add = (id, priority) => scheduler.acquire(id, priority, (active) => changes.set(id, active));
  const releaseCard = add("card", "card");
  add("pinned", "pinned");
  add("dialog", "dialog");
  assert.deepEqual(scheduler.getStats().activeIds, ["dialog"]);
  assert.equal(changes.get("card"), false);
  const releaseDialog = add("dialog", "dialog");
  releaseDialog();
  assert.deepEqual(scheduler.getStats().activeIds, ["pinned"]);
  releaseCard();
});

test("budget changes notify demotions and promote queued entries", () => {
  const scheduler = createCameraMediaScheduler({ maxLiveStreams: 2 });
  const changes = {};
  const releases = ["a", "b", "c"].map((id) => scheduler.acquire(id, "card", (active) => { changes[id] = active; }));
  assert.equal(scheduler.getStats().activeCount, 2);
  scheduler.setMaxLiveStreams(1);
  assert.equal(scheduler.getStats().activeCount, 1);
  scheduler.setMaxLiveStreams(3);
  assert.equal(scheduler.getStats().activeCount, 3);
  assert.equal(changes.c, true);
  releases.forEach((release) => release());
  assert.equal(scheduler.nextSnapshotDelayMs(5000, 1500, () => 0), 3500);
  assert.equal(scheduler.nextSnapshotDelayMs(1000, 0, () => 0.5), 1500);
});

test("reacquiring an ID grants the new lease and fences the old release", () => {
  const scheduler = createCameraMediaScheduler({ maxLiveStreams: 1 });
  const old = [];
  const replacement = [];
  const waiter = [];
  const releaseOld = scheduler.acquire("same", "card", (active) => old.push(active));
  scheduler.acquire("waiter", "card", (active) => waiter.push(active));
  const releaseReplacement = scheduler.acquire("same", "card", (active) => replacement.push(active));

  assert.deepEqual(old, [true, false]);
  assert.deepEqual(replacement, [true]);
  releaseOld();
  assert.deepEqual(scheduler.getStats().activeIds, ["same"]);
  assert.deepEqual(waiter, []);
  releaseReplacement();
  assert.deepEqual(replacement, [true, false]);
  assert.deepEqual(scheduler.getStats().activeIds, ["waiter"]);
  assert.deepEqual(waiter, [true]);
});

test("WHEP has an independent three-preview limit", () => {
  const whep = createCameraMediaScheduler({ maxLiveStreams: 3 });
  const media = createCameraMediaScheduler({ maxLiveStreams: 3 });
  const whepReleases = ["a", "b", "c", "d"].map((id) => whep.acquire(id, "card", () => {}));
  const mediaReleases = ["a", "b", "c", "d"].map((id) => media.acquire(id, "card", () => {}));
  assert.equal(whep.getStats().activeCount, 3);
  assert.equal(whep.getStats().queuedCount, 1);
  assert.equal(media.getStats().activeCount, 3);
  whepReleases.forEach((release) => release());
  mediaReleases.forEach((release) => release());
  assert.equal(cameraWhepScheduler.getStats().maxLiveStreams, 3);
  assert.equal(cameraMediaScheduler.getStats().maxLiveStreams, 3);
});

test("a dialog takes priority over cards and cancellation removes a queued WHEP preview", () => {
  const scheduler = createCameraMediaScheduler({ maxLiveStreams: 3 });
  const changes = new Map();
  const releases = ["card-a", "card-b", "card-c"].map((id) => scheduler.acquire(id, "card", (active) => changes.set(id, active)));
  scheduler.acquire("dialog", "dialog", (active) => changes.set("dialog", active));
  assert.equal(changes.get("dialog"), true);
  assert.equal(scheduler.getStats().activeIds.includes("dialog"), true);
  assert.equal(scheduler.getStats().activeIds.includes("card-c"), false);
  const cancel = scheduler.acquire("queued", "card", (active) => changes.set("queued", active));
  cancel();
  assert.equal(changes.has("queued"), false);
  releases.forEach((release) => release());
});

test("a queued WHEP preview stays snapshot-only until its grant", async () => {
  const queued = cameraTransportPlan({ whepEnabled: true, phase: "rate-limited", mjpegGranted: false });
  assert.deepEqual(queued, { postWhep: false, requestSnapshot: true, requestMjpeg: false });
  const granted = cameraTransportPlan({ whepEnabled: true, phase: "connecting", mjpegGranted: false });
  assert.equal(granted.postWhep, true);

  const card = await readFile(new URL("../src/astribot/pages/StationCard.tsx", import.meta.url), "utf8");
  const dialog = await readFile(new URL("../src/astribot/pages/CameraPreviewDialog.tsx", import.meta.url), "utf8");
  assert.match(card, /!whepGranted/);
  assert.match(card, /!whepOn \|\| !wantsLive \|\| !whepGranted \|\| !streamView/);
  assert.match(dialog, /!whepOn \|\| !foreground \|\| !whepGranted/);
});
