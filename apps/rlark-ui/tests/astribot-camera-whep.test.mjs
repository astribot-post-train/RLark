import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test, { mock } from "node:test";
import { ApiError } from "../dist/test/api.js";
import { createCameraMediaScheduler } from "../dist/test/astribot/lib/cameraMediaScheduler.js";
import { cameraStreamPath } from "../dist/test/astribot/lib/cameras.js";
import {
  WHEP_FIRST_FRAME_BUDGET_MS,
  WHEP_ICE_GATHERING_TIMEOUT_MS,
  WHEP_RENEW_INTERVAL_MS,
  cameraTransportPlan,
  cameraWhepPath,
  phaseAfterWhepFailure,
  resolveWhepSession,
  startStationWhepPreview,
  stationWhepEnabled,
} from "../dist/test/astribot/lib/cameraWhep.js";

const ORIGIN = "https://console.example";
const NAMESPACE = "rlark-robot";

class FakeMediaStream {
  constructor(tracks) {
    this.tracks = tracks;
  }
}

function track() {
  const listeners = new Map();
  return {
    kind: "video",
    muted: true,
    readyState: "live",
    stopped: false,
    stop() { this.stopped = true; },
    addEventListener(type, fn) {
      const list = listeners.get(type) ?? [];
      list.push(fn);
      listeners.set(type, list);
    },
    removeEventListener(type, fn) {
      listeners.set(type, (listeners.get(type) ?? []).filter((item) => item !== fn));
    },
    unmute() {
      this.muted = false;
      for (const fn of listeners.get("unmute") ?? []) fn();
    },
    end() {
      this.readyState = "ended";
      for (const fn of listeners.get("ended") ?? []) fn();
    },
  };
}

function peer(options = {}) {
  const listeners = new Map();
  const media = options.track ?? track();
  const created = {
    iceGatheringState: "gathering",
    iceConnectionState: "new",
    localDescription: null,
    remoteDescription: null,
    closed: false,
    track: media,
    transceivers: [],
    offerSdp: options.offerSdp ?? "v=0\r\noffer-line",
    addTransceiver(kind, init) { this.transceivers.push({ kind, init }); },
    async createOffer() { return { type: "offer", sdp: this.offerSdp }; },
    async setLocalDescription(description) {
      this.localDescription = { type: description.type, sdp: description.sdp };
    },
    async setRemoteDescription(description) { this.remoteDescription = description; },
    addEventListener(type, fn) {
      const list = listeners.get(type) ?? [];
      list.push(fn);
      listeners.set(type, list);
    },
    removeEventListener(type, fn) {
      listeners.set(type, (listeners.get(type) ?? []).filter((item) => item !== fn));
    },
    emit(type) {
      for (const fn of [...(listeners.get(type) ?? [])]) fn({ type, track: this.track });
    },
    getReceivers() { return [{ track: this.track }]; },
    getSenders() { return [{ track: null }]; },
    close() { this.closed = true; },
    completeIce(extra = "") {
      if (this.localDescription) {
        this.localDescription = { ...this.localDescription, sdp: `${this.localDescription.sdp}${extra}` };
      }
      this.iceGatheringState = "complete";
      this.emit("icegatheringstatechange");
    },
  };
  return created;
}

function video(width = 0) {
  const listeners = new Map();
  return {
    srcObject: null,
    videoWidth: width,
    play() { return Promise.resolve(); },
    addEventListener(type, fn) {
      const list = listeners.get(type) ?? [];
      list.push(fn);
      listeners.set(type, list);
    },
    removeEventListener(type, fn) {
      listeners.set(type, (listeners.get(type) ?? []).filter((item) => item !== fn));
    },
    emit(type) {
      for (const fn of [...(listeners.get(type) ?? [])]) fn();
    },
  };
}

function header(headers, name) {
  if (!headers) return undefined;
  if (typeof headers.get === "function") return headers.get(name);
  const key = Object.keys(headers).find((item) => item.toLowerCase() === name.toLowerCase());
  return key ? headers[key] : undefined;
}

function callsOf(calls, method) {
  return calls.filter((call) => call.method === method);
}

async function settle() {
  for (let i = 0; i < 8; i += 1) await new Promise((resolve) => setImmediate(resolve));
}

function harness(route, options = {}) {
  const calls = [];
  const pc = options.peer ?? peer();
  const element = options.video ?? video(options.videoWidth ?? 0);
  let failure;
  let playing = false;
  let resolveFailure = () => {};
  let resolvePlaying = () => {};
  const failurePromise = new Promise((resolve) => { resolveFailure = resolve; });
  const playingPromise = new Promise((resolve) => { resolvePlaying = resolve; });
  const request = async (input, init = {}) => {
    const url = String(input);
    const method = init.method ?? "GET";
    const entry = { url, method, headers: init.headers, body: init.body, credentials: init.credentials, signal: init.signal };
    calls.push(entry);
    const response = await route(entry);
    if (!response.ok) throw new ApiError(response.status, response.statusText || `HTTP ${response.status}`);
    return response;
  };
  const preview = startStationWhepPreview({
    enabled: options.enabled ?? true,
    streamPath: options.streamPath ?? cameraStreamPath("stream-01", NAMESPACE, "head", 30),
    namespace: options.namespace ?? NAMESPACE,
    origin: ORIGIN,
    request,
    video: options.video === null ? null : element,
    createPeerConnection: options.createPeerConnection ?? (() => pc),
    onPlaying: () => {
      playing = true;
      resolvePlaying();
    },
    onFailure: (value) => {
      failure = value;
      resolveFailure(value);
    },
  });
  return { calls, pc, element, preview, failurePromise, playingPromise, get failure() { return failure; }, get playing() { return playing; } };
}

test("the whep path replaces only the final /stream and keeps only namespace", () => {
  const stream = cameraStreamPath("stream-01", NAMESPACE, "head", 30);
  assert.equal(stream, "/api/v1/rlinf.io/v1alpha1/nodes/stream-01/cameras/head/stream?namespace=rlark-robot&fps=30");
  assert.equal(
    cameraWhepPath(stream),
    "/api/v1/rlinf.io/v1alpha1/nodes/stream-01/cameras/head/whep?namespace=rlark-robot",
  );
  assert.equal(
    cameraWhepPath("/proxy/stream/nodes/stream-cam/cameras/head/stream?namespace=rlark-robot&fps=15&stream=1"),
    "/proxy/stream/nodes/stream-cam/cameras/head/whep?namespace=rlark-robot",
  );
  assert.equal(stationWhepEnabled({}), false);
  assert.equal(stationWhepEnabled({ VITE_STATION_WHEP: "0" }), false);
  assert.equal(stationWhepEnabled({ VITE_STATION_WHEP: "true" }), false);
  assert.equal(stationWhepEnabled({ VITE_STATION_WHEP: "1" }), true);
});

test("a flag other than 1 performs no WHEP POST and leaves the JPEG plan in place", async () => {
  let posts = 0;
  let peers = 0;
  const preview = startStationWhepPreview({
    enabled: stationWhepEnabled({ VITE_STATION_WHEP: undefined }),
    streamPath: cameraStreamPath("stream-01", NAMESPACE, "head", 5),
    namespace: NAMESPACE,
    origin: ORIGIN,
    request: async () => { posts += 1; return new Response(null, { status: 201 }); },
    video: video(),
    createPeerConnection: () => { peers += 1; return peer(); },
  });
  await settle();
  await preview.stop();
  assert.equal(posts, 0);
  assert.equal(peers, 0);
  const jpeg = cameraTransportPlan({ whepEnabled: false, phase: "off", mjpegGranted: false });
  assert.equal(jpeg.postWhep, false);
  assert.equal(jpeg.requestSnapshot, true);
  assert.equal(jpeg.requestMjpeg, false);
  const live = cameraTransportPlan({ whepEnabled: false, phase: "off", mjpegGranted: true });
  assert.equal(live.postWhep, false);
  assert.equal(live.requestSnapshot, false);
  assert.equal(live.requestMjpeg, true);
});

test("an empty local offer is a technical failure and does not POST", async () => {
  globalThis.MediaStream = FakeMediaStream;
  const session = harness(() => new Response("v=0\r\nanswer", { status: 201 }), {
    peer: peer({ offerSdp: "   " }),
  });
  await settle();
  session.pc.completeIce();
  const failure = await session.failurePromise;
  assert.equal(failure.kind, "technical");
  assert.match(failure.detail, /empty offer/);
  assert.equal(session.calls.length, 0);
  assert.equal(session.playing, false);
  assert.equal(session.pc.closed, true);
});

test("POST waits until ICE gathering is complete and sends application/sdp", async () => {
  globalThis.MediaStream = FakeMediaStream;
  const session = harness(() => new Response("v=0\r\nanswer", {
    status: 201,
    headers: { Location: `/cameras/head/whep/session?namespace=${NAMESPACE}` },
  }));
  await settle();
  assert.equal(session.calls.length, 0);
  session.pc.completeIce("\r\na=candidate:1 1 UDP 2122252543 192.0.2.1 9 typ host");
  await settle();
  assert.equal(session.calls.length, 1);
  const post = session.calls[0];
  assert.equal(post.method, "POST");
  assert.equal(post.url, "/api/v1/rlinf.io/v1alpha1/nodes/stream-01/cameras/head/whep?namespace=rlark-robot");
  assert.equal(header(post.headers, "Content-Type"), "application/sdp");
  assert.equal(header(post.headers, "Accept"), "application/sdp");
  assert.equal(post.body, "v=0\r\noffer-line\r\na=candidate:1 1 UDP 2122252543 192.0.2.1 9 typ host");
  assert.equal(post.credentials, undefined);
  assert.deepEqual(session.pc.transceivers, [{ kind: "video", init: { direction: "recvonly" } }]);
  assert.equal(session.pc.remoteDescription.type, "answer");
  await session.preview.stop();
});

test("201 with a relative namespaced Location renews every 6s and DELETEs on close", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());
  const location = `/api/v1/rlinf.io/v1alpha1/nodes/stream-01/cameras/head/whep/session?namespace=${NAMESPACE}`;
  const session = harness((call) => {
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: location } });
    if (call.method === "PUT") return new Response(null, { status: 204 });
    if (call.method === "DELETE") return new Response(null, { status: 200 });
    return new Response("no", { status: 500 });
  }, { videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
  await settle();
  session.pc.completeIce();
  await session.playingPromise;
  assert.equal(callsOf(session.calls, "PUT").length, 0);
  mock.timers.tick(WHEP_RENEW_INTERVAL_MS);
  await settle();
  mock.timers.tick(WHEP_RENEW_INTERVAL_MS);
  await settle();
  const puts = callsOf(session.calls, "PUT");
  assert.equal(puts.length, 2);
  assert.equal(puts[0].url, `${ORIGIN}${location}`);
  assert.equal(puts[1].url, puts[0].url);
  await session.preview.stop();
  await settle();
  assert.equal(callsOf(session.calls, "DELETE").length, 1);
  assert.equal(callsOf(session.calls, "DELETE")[0].url, puts[0].url);
  assert.equal(session.pc.closed, true);
  assert.equal(session.pc.track.stopped, true);
  const putsAfterClose = callsOf(session.calls, "PUT").length;
  mock.timers.tick(WHEP_RENEW_INTERVAL_MS * 3);
  await settle();
  assert.equal(callsOf(session.calls, "PUT").length, putsAfterClose);
});

test("a failed DELETE still closes the peer connection and clears the renew timer", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());
  const session = harness((call) => {
    if (call.method === "POST") {
      return new Response("v=0\r\nanswer", { status: 201, headers: { Location: `/whep/session?namespace=${NAMESPACE}` } });
    }
    if (call.method === "DELETE") return new Response("no", { status: 500 });
    return new Response(null, { status: 204 });
  }, { videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
  await settle();
  session.pc.completeIce();
  await session.playingPromise;
  await session.preview.stop();
  await settle();
  assert.equal(session.pc.closed, true);
  assert.equal(callsOf(session.calls, "DELETE").length, 1);
  mock.timers.tick(WHEP_RENEW_INTERVAL_MS * 2);
  await settle();
  assert.equal(callsOf(session.calls, "PUT").length, 0);
});

test("a cross-origin Location receives no auth and no PUT or DELETE", async () => {
  globalThis.MediaStream = FakeMediaStream;
  const session = harness(() => new Response("v=0\r\nanswer", {
    status: 201,
    headers: { Location: `https://evil.example/whep/session?namespace=${NAMESPACE}` },
  }), { videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
  await settle();
  session.pc.completeIce();
  const failure = await session.failurePromise;
  assert.equal(failure.kind, "technical");
  assert.match(failure.detail, /cross-origin/);
  assert.deepEqual(session.calls.map((call) => call.method), ["POST"]);
  assert.equal(session.calls.some((call) => call.url.includes("evil.example")), false);
  assert.equal(session.playing, false);
  assert.equal(session.pc.closed, true);
  assert.equal(resolveWhepSession(`https://evil.example/whep/session?namespace=${NAMESPACE}`, NAMESPACE, ORIGIN).ok, false);
});

test("a missing Location or one without the namespace is not called", async () => {
  globalThis.MediaStream = FakeMediaStream;
  for (const headers of [{}, { Location: "/whep/session" }, { Location: "" }]) {
    const session = harness(() => new Response("v=0\r\nanswer", { status: 201, headers }));
    await settle();
    session.pc.completeIce();
    const failure = await session.failurePromise;
    assert.equal(failure.kind, "technical");
    assert.deepEqual(session.calls.map((call) => call.method), ["POST"]);
    assert.equal(session.pc.closed, true);
  }
});

test("401 and 403 do not request a snapshot and do not start MJPEG", async () => {
  globalThis.MediaStream = FakeMediaStream;
  for (const status of [401, 403]) {
    const session = harness(() => new Response("no", { status }));
    await settle();
    session.pc.completeIce();
    const failure = await session.failurePromise;
    assert.equal(failure.kind, "unauthorized");
    assert.equal(failure.status, status);
    assert.deepEqual(session.calls.map((call) => `${call.method} ${call.url}`), [
      "POST /api/v1/rlinf.io/v1alpha1/nodes/stream-01/cameras/head/whep?namespace=rlark-robot",
    ]);
    assert.equal(session.calls.some((call) => call.url.includes(".jpg") || call.url.endsWith("/stream")), false);
    const phase = phaseAfterWhepFailure(failure);
    const plan = cameraTransportPlan({ whepEnabled: true, phase, mjpegGranted: false });
    assert.equal(phase, "unauthorized");
    assert.equal(plan.requestSnapshot, false);
    assert.equal(plan.requestMjpeg, false);
    assert.equal(plan.postWhep, false);
    assert.equal(session.pc.closed, true);
    assert.equal(session.playing, false);
  }
});

test("429 requests a snapshot and does not start MJPEG or another offer", async () => {
  globalThis.MediaStream = FakeMediaStream;
  const session = harness(() => new Response("slow", { status: 429 }));
  await settle();
  session.pc.completeIce();
  const failure = await session.failurePromise;
  assert.equal(failure.kind, "rate-limited");
  assert.equal(session.calls.length, 1);
  const plan = cameraTransportPlan({ whepEnabled: true, phase: phaseAfterWhepFailure(failure), mjpegGranted: true });
  assert.equal(plan.requestSnapshot, true);
  assert.equal(plan.requestMjpeg, false);
  assert.equal(plan.postWhep, false);
  await settle();
  assert.equal(session.calls.length, 1);
});

test("timeout, ICE failure, and other technical errors snapshot first and use MJPEG only inside the budget", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());

  const timedOut = harness(() => new Response("v=0\r\nanswer", { status: 201, headers: { Location: `/s?namespace=${NAMESPACE}` } }));
  await settle();
  assert.equal(timedOut.calls.length, 0);
  mock.timers.tick(WHEP_ICE_GATHERING_TIMEOUT_MS);
  const timeoutFailure = await timedOut.failurePromise;
  assert.equal(timeoutFailure.kind, "technical");
  assert.match(timeoutFailure.detail, /ice-gathering-timeout/);
  assert.equal(timedOut.calls.length, 0);
  assert.equal(timedOut.pc.closed, true);
  assert.equal(timedOut.playing, false);

  const full = createCameraMediaScheduler({ maxLiveStreams: 1 });
  let busy = false;
  full.acquire("busy", "card", (active) => { busy = active; });
  assert.equal(busy, true);
  // A new lease that does not win a slot is not told it is active. The preview
  // therefore stays on the snapshot until the scheduler grants MJPEG.
  let queued = false;
  full.acquire("camera", "card", (active) => { queued = active; });
  assert.equal(queued, false);
  assert.equal(full.getStats().activeCount, 1);
  const waiting = cameraTransportPlan({ whepEnabled: true, phase: phaseAfterWhepFailure(timeoutFailure), mjpegGranted: queued });
  assert.equal(waiting.requestSnapshot, true);
  assert.equal(waiting.requestMjpeg, true);
  assert.equal(waiting.postWhep, false);

  const open = createCameraMediaScheduler({ maxLiveStreams: 1 });
  let granted = false;
  open.acquire("camera", "dialog", (active) => { granted = active; });
  assert.equal(granted, true);
  const live = cameraTransportPlan({ whepEnabled: true, phase: "mjpeg", mjpegGranted: granted });
  assert.equal(live.requestSnapshot, false);
  assert.equal(live.requestMjpeg, true);

  for (const state of ["failed", "disconnected"]) {
    const session = harness((call) => {
      if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: `/s?namespace=${NAMESPACE}` } });
      return new Response(null, { status: 204 });
    }, { videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
    await settle();
    session.pc.completeIce();
    await session.playingPromise;
    session.pc.iceConnectionState = state;
    session.pc.emit("iceconnectionstatechange");
    const failure = await session.failurePromise;
    assert.equal(failure.kind, "technical");
    assert.match(failure.detail, new RegExp(state));
    assert.equal(session.calls.some((call) => call.url.includes(".jpg")), false);
    await settle();
    assert.equal(callsOf(session.calls, "DELETE").length, 1);
    assert.equal(session.pc.closed, true);
    const puts = callsOf(session.calls, "PUT").length;
    mock.timers.tick(WHEP_RENEW_INTERVAL_MS);
    await settle();
    assert.equal(callsOf(session.calls, "PUT").length, puts);
  }
});

test("HTTP 404 on renew is a technical failure and 401 or 429 on renew do not upgrade transport", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());
  const expectRenew = async (status, kind) => {
    const session = harness((call) => {
      if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: `/s?namespace=${NAMESPACE}` } });
      if (call.method === "PUT") return new Response("no", { status });
      return new Response(null, { status: 204 });
    }, { videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
    await settle();
    session.pc.completeIce();
    await session.playingPromise;
    mock.timers.tick(WHEP_RENEW_INTERVAL_MS);
    const failure = await session.failurePromise;
    await settle();
    assert.equal(failure.kind, kind);
    assert.equal(failure.status, status);
    assert.equal(session.pc.closed, true);
    assert.equal(callsOf(session.calls, "DELETE").length, 1);
    const plan = cameraTransportPlan({ whepEnabled: true, phase: phaseAfterWhepFailure(failure), mjpegGranted: false });
    if (kind === "technical") {
      assert.equal(plan.requestSnapshot, true);
      assert.equal(plan.requestMjpeg, true);
    } else if (kind === "unauthorized") {
      assert.equal(plan.requestSnapshot, false);
      assert.equal(plan.requestMjpeg, false);
    } else {
      assert.equal(plan.requestSnapshot, true);
      assert.equal(plan.requestMjpeg, false);
    }
    const puts = callsOf(session.calls, "PUT").length;
    mock.timers.tick(WHEP_RENEW_INTERVAL_MS * 2);
    await settle();
    assert.equal(callsOf(session.calls, "PUT").length, puts);
  };
  await expectRenew(404, "technical");
  await expectRenew(401, "unauthorized");
  await expectRenew(403, "unauthorized");
  await expectRenew(429, "rate-limited");
});

test("a missed first frame, an ended track, an empty answer, and a video error are failures", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());

  const missed = harness((call) => {
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: `/s?namespace=${NAMESPACE}` } });
    return new Response(null, { status: 204 });
  });
  await settle();
  missed.pc.completeIce();
  await settle();
  assert.equal(missed.playing, false);
  mock.timers.tick(WHEP_FIRST_FRAME_BUDGET_MS);
  const missedFailure = await missed.failurePromise;
  await settle();
  assert.equal(missedFailure.kind, "technical");
  assert.match(missedFailure.detail, /first-frame/);
  assert.equal(missed.playing, false);
  assert.equal(missed.pc.closed, true);
  assert.equal(callsOf(missed.calls, "DELETE").length, 1);

  const ended = harness((call) => {
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: `/s?namespace=${NAMESPACE}` } });
    return new Response(null, { status: 204 });
  });
  await settle();
  ended.pc.completeIce();
  await settle();
  ended.pc.track.end();
  const endedFailure = await ended.failurePromise;
  await settle();
  assert.equal(endedFailure.detail, "track ended");
  assert.equal(ended.playing, false);
  assert.equal(ended.pc.closed, true);

  const empty = harness(() => new Response("   ", {
    status: 201,
    headers: { Location: `/s?namespace=${NAMESPACE}` },
  }));
  await settle();
  empty.pc.completeIce();
  const emptyFailure = await empty.failurePromise;
  await settle();
  assert.match(emptyFailure.detail, /empty answer/);
  assert.equal(empty.playing, false);
  assert.equal(callsOf(empty.calls, "DELETE").length, 1);
  assert.equal(empty.pc.closed, true);

  const broken = harness((call) => {
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: `/s?namespace=${NAMESPACE}` } });
    return new Response(null, { status: 204 });
  }, { videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
  await settle();
  broken.pc.completeIce();
  await broken.playingPromise;
  broken.element.emit("error");
  const videoFailure = await broken.failurePromise;
  await settle();
  assert.equal(videoFailure.detail, "video error");
  assert.equal(broken.pc.closed, true);
  assert.equal(callsOf(broken.calls, "DELETE").length, 1);
});

test("the card and dialog gate WHEP on the flag and keep the JPEG stream path", async () => {
  const card = await readFile(new URL("../src/astribot/pages/StationCard.tsx", import.meta.url), "utf8");
  const dialog = await readFile(new URL("../src/astribot/pages/CameraPreviewDialog.tsx", import.meta.url), "utf8");
  assert.match(card, /stationWhepEnabled\(\)/);
  assert.match(dialog, /stationWhepEnabled\(\)/);
  assert.match(card, /if \(!whepOn \|\| !previewActive \|\| !streamView\) return/);
  assert.match(dialog, /phaseAfterWhepFailure/);
  assert.match(card, /cameraStreamPath\(node, namespace, streamView, fps\)/);
  assert.match(dialog, /cameraStreamPath\(node, namespace, view, fps\)/);
  assert.match(dialog, /cameraFramePath\(node, namespace, view\)/);
  assert.match(card, /cameraFramePath\(node, namespace, streamView\)/);
  const english = await readFile(new URL("../../../apps/rlark/docs/user-guide/console.md", import.meta.url), "utf8");
  const chinese = await readFile(new URL("../../../apps/rlark/docs/zh/user-guide/console.md", import.meta.url), "utf8");
  assert.match(english, /VITE_STATION_WHEP=1/);
  assert.match(english, /401 and 403/);
  assert.match(english, /429 stays on the snapshot/);
  assert.match(chinese, /VITE_STATION_WHEP=1/);
  assert.match(chinese, /401 和 403/);
  assert.match(chinese, /429 只保留快照/);
});
