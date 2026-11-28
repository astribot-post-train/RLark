import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test, { mock } from "node:test";
import { ApiError } from "../dist/test/api.js";
import { createCameraMediaScheduler } from "../dist/test/astribot/lib/cameraMediaScheduler.js";
import { cameraStreamPath } from "../dist/test/astribot/lib/cameras.js";
import {
  WHEP_DELETE_TIMEOUT_MS,
  WHEP_FIRST_FRAME_BUDGET_MS,
  WHEP_ICE_GATHERING_TIMEOUT_MS,
  WHEP_RENEW_INTERVAL_MS,
  cameraTransportPlan,
  cameraWhepPath,
  phaseAfterWhepFailure,
  resolveWhepSession,
  startScheduledStationWhepPreview,
  startStationWhepPreview,
  stationWhepEnabled,
} from "../dist/test/astribot/lib/cameraWhep.js";

globalThis.requestAnimationFrame ??= (callback) => setImmediate(callback);
globalThis.cancelAnimationFrame ??= (handle) => clearImmediate(handle);

const ORIGIN = "https://console.example";
const NAMESPACE = "rlark-robot";
const SESSION_ID = "123e4567-e89b-42d3-a456-426614174000";
const POST_PATH = "/api/v1/rlinf.io/v1alpha1/nodes/stream-01/cameras/head/whep";

function sessionLocation(id = SESSION_ID) {
  return `${POST_PATH}/${id}?namespace=${NAMESPACE}`;
}

function postTarget() {
  return cameraWhepPath(cameraStreamPath("stream-01", NAMESPACE, "head", 30));
}

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

function video(width = 0, readyState = 2) {
  const listeners = new Map();
  return {
    srcObject: null,
    videoWidth: width,
    readyState,
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
    const entry = {
      url,
      method,
      headers: init.headers,
      body: init.body,
      credentials: init.credentials,
      signal: init.signal,
      keepalive: init.keepalive,
    };
    calls.push(entry);
    const response = await route(entry);
    if (!options.rawStatus && !response.ok) throw new ApiError(response.status, response.statusText || `HTTP ${response.status}`);
    return response;
  };
  const startPreview = options.scheduled ? startScheduledStationWhepPreview : startStationWhepPreview;
  const preview = startPreview({
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
  assert.equal(stationWhepEnabled("station-1", {}), false);
  assert.equal(stationWhepEnabled("station-1", { VITE_STATION_WHEP: "0" }), false);
  assert.equal(stationWhepEnabled("station-1", { VITE_STATION_WHEP: "true" }), false);
  assert.equal(stationWhepEnabled("station-1", { VITE_STATION_WHEP: "1" }), true);
  assert.equal(stationWhepEnabled("station-1", { VITE_STATION_WHEP: "1", VITE_STATION_WHEP_NODES: "station-2" }), false);
  assert.equal(stationWhepEnabled("station-1", { VITE_STATION_WHEP: "1", VITE_STATION_WHEP_NODES: "station-2, station-1" }), true);
});

test("a flag other than 1 performs no WHEP POST and leaves the JPEG plan in place", async () => {
  let posts = 0;
  let peers = 0;
  const preview = startStationWhepPreview({
    enabled: stationWhepEnabled("station-1", { VITE_STATION_WHEP: undefined }),
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
    headers: { Location: sessionLocation() },
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
  const location = sessionLocation();
  const session = harness((call) => {
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: location } });
    if (call.method === "PUT") return new Response(null, { status: 204 });
    if (call.method === "DELETE") return new Response(null, { status: 202 });
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
  assert.equal(callsOf(session.calls, "DELETE")[0].keepalive, true);
  assert.equal(session.failure, undefined);
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
      return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
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
  assert.equal(resolveWhepSession(`https://evil.example/whep/session?namespace=${NAMESPACE}`, NAMESPACE, ORIGIN, postTarget()).ok, false);
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

test("scheduled WHEP retries a rate limit after 1, 2, and 4 seconds, then recovers", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());
  let attempts = 0;
  const session = harness(() => {
    attempts++;
    return attempts < 4
      ? new Response("busy", { status: 429 })
      : new Response("v=0\\r\\nanswer", { status: 201, headers: { Location: sessionLocation() } });
  }, { scheduled: true });
  await settle();
  session.pc.completeIce();
  await session.failurePromise;
  assert.equal(session.calls.length, 1);
  mock.timers.tick(999);
  await settle();
  assert.equal(session.calls.length, 1);
  mock.timers.tick(1);
  await settle();
  assert.equal(session.calls.length, 2);
  mock.timers.tick(1999);
  await settle();
  assert.equal(session.calls.length, 2);
  mock.timers.tick(1);
  await settle();
  assert.equal(session.calls.length, 3);
  mock.timers.tick(3999);
  await settle();
  assert.equal(session.calls.length, 3);
  mock.timers.tick(1);
  await settle();
  assert.equal(session.calls.length, 4);
  await session.preview.stop();
});

test("scheduled WHEP cancellation stops retries when a grant is revoked", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());
  const session = harness(() => new Response("busy", { status: 429 }), { scheduled: true });
  await settle();
  session.pc.completeIce();
  await session.failurePromise;
  await session.preview.stop();
  mock.timers.tick(10000);
  await settle();
  assert.equal(session.calls.length, 1);
});

test("scheduled WHEP does not retry unauthorized responses", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());
  for (const status of [401, 403]) {
    const session = harness(() => new Response("denied", { status }), { scheduled: true });
    await settle();
    session.pc.completeIce();
    const failure = await session.failurePromise;
    assert.equal(failure.kind, "unauthorized");
    mock.timers.tick(10000);
    await settle();
    assert.equal(session.calls.length, 1);
    await session.preview.stop();
  }
});

test("late cleanup from an old WHEP session preserves a newer video stream", async () => {
  globalThis.MediaStream = FakeMediaStream;
  const element = video(1, 2);
  const oldTrack = Object.assign(track(), { muted: false });
  const newTrack = Object.assign(track(), { muted: false });
  const oldPeer = peer({ track: oldTrack });
  const newPeer = peer({ track: newTrack });
  let postCount = 0;
  let releaseDelete;
  const deleteDone = new Promise((resolve) => { releaseDelete = resolve; });
  const request = async (input, init = {}) => {
    if (init.method === "DELETE") {
      await deleteDone;
      return new Response(null, { status: 204 });
    }
    postCount++;
    return new Response("v=0\\r\\nanswer", { status: 201, headers: { Location: sessionLocation(postCount === 1 ? SESSION_ID : "123e4567-e89b-42d3-a456-426614174001") } });
  };
  const oldPreview = startStationWhepPreview({ enabled: true, streamPath: cameraStreamPath("stream-01", NAMESPACE, "head", 30), namespace: NAMESPACE, origin: ORIGIN, request, video: element, createPeerConnection: () => oldPeer });
  await settle();
  oldPeer.completeIce();
  await settle();
  const oldStop = oldPreview.stop();
  const newPreview = startStationWhepPreview({ enabled: true, streamPath: cameraStreamPath("stream-01", NAMESPACE, "head", 30), namespace: NAMESPACE, origin: ORIGIN, request, video: element, createPeerConnection: () => newPeer });
  await settle();
  newPeer.completeIce();
  await settle();
  const newerStream = element.srcObject;
  assert.ok(newerStream);
  releaseDelete();
  await oldStop;
  assert.equal(element.srcObject, newerStream);
  await newPreview.stop();
});

test("timeout, ICE failure, and other technical errors snapshot first and use MJPEG only inside the budget", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());

  const timedOut = harness(() => new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } }));
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
      if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
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
      if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
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
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
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
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
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
    headers: { Location: sessionLocation() },
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
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
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

test("Location must be this POST path plus one UUID, with one exact namespace query", () => {
  const post = postTarget();
  const accepted = resolveWhepSession(sessionLocation(), NAMESPACE, ORIGIN, post);
  assert.equal(accepted.ok, true);
  assert.equal(accepted.url, `${ORIGIN}${sessionLocation()}`);
  const upper = SESSION_ID.toUpperCase();
  assert.equal(resolveWhepSession(sessionLocation(upper), NAMESPACE, ORIGIN, post).ok, true);

  const rejected = [
    [sessionLocation(), "other-namespace", "namespace"],
    [`${POST_PATH}/${SESSION_ID}?namespace=${NAMESPACE}&namespace=${NAMESPACE}`, NAMESPACE, "namespace"],
    [`${POST_PATH}/${SESSION_ID}?namespace=${NAMESPACE}&fps=30`, NAMESPACE, "namespace"],
    [`${POST_PATH}/${SESSION_ID}?namespace=${NAMESPACE}-extra`, NAMESPACE, "namespace"],
    [`${POST_PATH}/session?namespace=${NAMESPACE}`, NAMESPACE, "resource"],
    [`${POST_PATH}/${SESSION_ID}/extra?namespace=${NAMESPACE}`, NAMESPACE, "resource"],
    [`${POST_PATH}/../admin?namespace=${NAMESPACE}`, NAMESPACE, "resource"],
    [`/api/v1/secrets/${NAMESPACE}?namespace=${NAMESPACE}`, NAMESPACE, "resource"],
    [`${sessionLocation()}#fragment`, NAMESPACE, "resource"],
    [`https://user:pass@console.example${sessionLocation()}`, NAMESPACE, "cross-origin"],
    [`https://evil.example${sessionLocation()}`, NAMESPACE, "cross-origin"],
    [`//evil.example${sessionLocation()}`, NAMESPACE, "cross-origin"],
    [`\\\\evil.example\\${sessionLocation()}`, NAMESPACE, "cross-origin"],
    ["", NAMESPACE, "missing"],
    [null, NAMESPACE, "missing"],
  ];
  for (const [location, namespace, reason] of rejected) {
    const result = resolveWhepSession(location, namespace, ORIGIN, post);
    assert.equal(result.ok, false, String(location));
    assert.equal(result.reason, reason, String(location));
  }
});

test("a same-origin Location that is not this session gets no PUT or DELETE", async () => {
  globalThis.MediaStream = FakeMediaStream;
  const attacks = [
    `${ORIGIN}/api/v1/admin/delete?namespace=${NAMESPACE}`,
    `${POST_PATH}/session?namespace=${NAMESPACE}`,
    `https://user:pass@console.example${sessionLocation()}`,
    `${sessionLocation()}#session`,
    `${POST_PATH}/${SESSION_ID}?namespace=${NAMESPACE}&namespace=other`,
  ];
  for (const location of attacks) {
    const session = harness(() => new Response("v=0\r\nanswer", { status: 201, headers: { Location: location } }));
    await settle();
    session.pc.completeIce();
    const failure = await session.failurePromise;
    assert.equal(failure.kind, "technical");
    assert.deepEqual(session.calls.map((call) => call.method), ["POST"]);
    assert.equal(session.pc.closed, true);
    assert.equal(session.playing, false);
  }
});

test("a returned 401, 403, or 429 response is classified without an ApiError", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());
  for (const status of [401, 403, 429]) {
    const session = harness(() => new Response("no", { status, headers: { Location: sessionLocation() } }), { rawStatus: true });
    await settle();
    session.pc.completeIce();
    const failure = await session.failurePromise;
    assert.equal(failure.kind, status === 429 ? "rate-limited" : "unauthorized");
    assert.equal(failure.status, status);
    const plan = cameraTransportPlan({ whepEnabled: true, phase: phaseAfterWhepFailure(failure), mjpegGranted: false });
    assert.equal(plan.postWhep, false);
    assert.equal(plan.requestMjpeg, false);
    assert.equal(plan.requestSnapshot, status === 429);
    assert.equal(session.playing, false);
  }

  const created = harness(() => new Response("v=0\r\nanswer", {
    status: 202,
    headers: { Location: sessionLocation() },
  }), { rawStatus: true });
  await settle();
  created.pc.completeIce();
  const createdFailure = await created.failurePromise;
  assert.equal(createdFailure.kind, "technical");
  assert.equal(createdFailure.status, 202);
  assert.equal(created.playing, false);
  assert.equal(created.pc.remoteDescription, null);

  for (const status of [401, 403, 429, 202]) {
    const session = harness((call) => {
      if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
      if (call.method === "PUT") return new Response("no", { status });
      return new Response(null, { status: 202 });
    }, { rawStatus: true, videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
    await settle();
    session.pc.completeIce();
    await session.playingPromise;
    mock.timers.tick(WHEP_RENEW_INTERVAL_MS);
    const failure = await session.failurePromise;
    await settle();
    const kind = status === 401 || status === 403 ? "unauthorized" : status === 429 ? "rate-limited" : "technical";
    assert.equal(failure.kind, kind);
    assert.equal(failure.status, status);
    const plan = cameraTransportPlan({ whepEnabled: true, phase: phaseAfterWhepFailure(failure), mjpegGranted: false });
    if (kind === "unauthorized") {
      assert.equal(plan.requestSnapshot, false);
      assert.equal(plan.requestMjpeg, false);
    } else if (kind === "rate-limited") {
      assert.equal(plan.requestSnapshot, true);
      assert.equal(plan.requestMjpeg, false);
    } else {
      assert.equal(plan.requestSnapshot, true);
      assert.equal(plan.requestMjpeg, true);
    }
    assert.equal(session.pc.closed, true);
  }
});

test("close releases the peer before DELETE, and a hung DELETE does not block fallback", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());
  let releaseDelete = () => {};
  const session = harness((call) => {
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
    if (call.method === "DELETE") return new Promise((resolve) => { releaseDelete = () => resolve(new Response(null, { status: 202 })); });
    return new Response(null, { status: 204 });
  }, { videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
  await settle();
  session.pc.completeIce();
  await session.playingPromise;
  await settle();
  assert.equal(session.pc.closed, false);
  session.pc.iceConnectionState = "failed";
  session.pc.emit("iceconnectionstatechange");
  const failure = await session.failurePromise;
  assert.equal(failure.kind, "technical");
  assert.equal(session.pc.closed, true);
  assert.equal(session.pc.track.stopped, true);
  const deleted = callsOf(session.calls, "DELETE");
  assert.equal(deleted.length, 1);
  assert.equal(deleted[0].keepalive, true);
  assert.equal(deleted[0].signal.aborted, false);
  assert.equal(deleted[0].url, `${ORIGIN}${sessionLocation()}`);
  mock.timers.tick(WHEP_DELETE_TIMEOUT_MS);
  assert.equal(deleted[0].signal.aborted, true);
  releaseDelete();

  let releaseStop = () => {};
  const stopping = harness((call) => {
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
    if (call.method === "DELETE") return new Promise((resolve) => { releaseStop = () => resolve(new Response(null, { status: 202 })); });
    return new Response(null, { status: 204 });
  }, { videoWidth: 1, peer: peer({ track: Object.assign(track(), { muted: false }) }) });
  await settle();
  stopping.pc.completeIce();
  await stopping.playingPromise;
  await settle();
  const done = stopping.preview.stop();
  await settle();
  assert.equal(stopping.pc.closed, true);
  const stopDelete = callsOf(stopping.calls, "DELETE")[0];
  assert.equal(stopDelete.keepalive, true);
  assert.equal(stopDelete.signal.aborted, false);
  let settled = false;
  void done.then(() => { settled = true; });
  await settle();
  assert.equal(settled, false);
  mock.timers.tick(WHEP_DELETE_TIMEOUT_MS);
  await done;
  assert.equal(settled, true);
  assert.equal(stopDelete.signal.aborted, true);
  releaseStop();
});

test("unmute and metadata dimensions are not a frame until current video data is available", async (t) => {
  globalThis.MediaStream = FakeMediaStream;
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  t.after(() => mock.timers.reset());

  const media = track();
  const session = harness((call) => {
    if (call.method === "POST") return new Response("v=0\r\nanswer", { status: 201, headers: { Location: sessionLocation() } });
    return new Response(null, { status: 204 });
  }, {
    video: video(640, 0),
    peer: peer({ track: media }),
  });
  await settle();
  session.pc.completeIce();
  await settle();

  assert.equal(session.element.srcObject.tracks[0], media);
  assert.equal(session.playing, false);
  media.unmute();
  session.element.readyState = 1;
  await settle();
  mock.timers.tick(16);
  await settle();
  assert.equal(session.playing, false);

  session.element.readyState = 2;
  mock.timers.tick(16);
  await session.playingPromise;
  assert.equal(session.playing, true);
  assert.equal(session.element.srcObject.tracks[0], media);
  await session.preview.stop();
});

test("the card and dialog gate WHEP on the flag and keep the JPEG stream path", async () => {
  const card = await readFile(new URL("../src/astribot/pages/StationCard.tsx", import.meta.url), "utf8");
  const dialog = await readFile(new URL("../src/astribot/pages/CameraPreviewDialog.tsx", import.meta.url), "utf8");
  assert.match(card, /stationWhepEnabled\(node\)/);
  assert.match(dialog, /stationWhepEnabled\(node\)/);
  assert.match(card, /if \(!whepOn \|\| !wantsLive \|\| !whepGranted \|\| !streamView\) return/);
  assert.match(dialog, /phaseAfterWhepFailure/);
  assert.match(card, /cameraStreamPath\(node, namespace, streamView, fps\)/);
  assert.match(dialog, /cameraStreamPath\(node, namespace, view, fps\)/);
  assert.match(dialog, /cameraFramePath\(node, namespace, view\)/);
  assert.match(card, /cameraFramePath\(node, namespace, streamView\)/);
  const english = await readFile(new URL("../../../apps/rlark/docs/user-guide/console.md", import.meta.url), "utf8");
  const chinese = await readFile(new URL("../../../apps/rlark/docs/zh/user-guide/console.md", import.meta.url), "utf8");
  assert.match(english, /VITE_STATION_WHEP_NODES/);
  assert.match(english, /401 and 403/);
  assert.match(english, /429 stays on the snapshot/);
  assert.match(chinese, /VITE_STATION_WHEP_NODES/);
  assert.match(chinese, /401 和 403/);
  assert.match(chinese, /429 只保留快照/);
});
