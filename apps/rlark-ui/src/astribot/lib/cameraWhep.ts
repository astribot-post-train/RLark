// Station-camera WHEP. JPEG stays the default. A session is opened only when
// VITE_STATION_WHEP is exactly "1", and it uses the same `request` helper as the
// JPEG paths so the IAM bearer token is attached once, on the console origin.
import { ApiError } from "../../api.js";

/** The card's JPEG stream watchdog. ICE gathering and the first frame share it. */
export const WHEP_ICE_GATHERING_TIMEOUT_MS = 8000;
export const WHEP_FIRST_FRAME_BUDGET_MS = WHEP_ICE_GATHERING_TIMEOUT_MS;
export const WHEP_RENEW_INTERVAL_MS = 6000;
/** Best-effort session DELETE. A hung request must not hold the preview open. */
export const WHEP_DELETE_TIMEOUT_MS = 2000;

export type WhepFailureKind = "unauthorized" | "rate-limited" | "technical";

export interface WhepFailure {
  kind: WhepFailureKind;
  status?: number;
  detail: string;
}

export type WhepUiPhase =
  "off" | "connecting" | "playing" | "rate-limited" | "unauthorized" | "mjpeg";

export interface WhepSdpOffer {
  type: "offer";
  sdp: string;
}

export interface WhepSdpAnswer {
  type: "answer";
  sdp: string;
}

export interface CameraTransportPlan {
  postWhep: boolean;
  requestSnapshot: boolean;
  requestMjpeg: boolean;
}

export type ResolvedWhepSession =
  | { ok: true; url: string }
  | {
      ok: false;
      reason: "missing" | "namespace" | "cross-origin" | "resource";
    };

export interface WhepSdpRequest {
  (
    input: RequestInfo | URL,
    init?: {
      method?: string;
      headers?: HeadersInit;
      body?: BodyInit | null;
      signal?: AbortSignal;
      keepalive?: boolean;
    },
  ): Promise<Response>;
}

export interface WhepPreviewVideo {
  srcObject: MediaProvider | null;
  videoWidth: number;
  readyState: number;
  play?: () => Promise<void>;
  addEventListener(type: "error", listener: EventListener): void;
  removeEventListener(type: "error", listener: EventListener): void;
}

interface WhepTrack {
  kind: string;
  muted: boolean;
  readyState: "live" | "ended";
  stop(): void;
  addEventListener(type: "unmute" | "ended", listener: () => void): void;
  removeEventListener(type: "unmute" | "ended", listener: () => void): void;
}

export interface WhepPeer {
  iceGatheringState: RTCIceGatheringState;
  iceConnectionState: RTCIceConnectionState;
  localDescription: { sdp?: string } | null;
  addTransceiver(
    kind: "video",
    init: { direction: RTCRtpTransceiverDirection },
  ): void;
  createOffer(): Promise<RTCSessionDescriptionInit>;
  setLocalDescription(description: RTCSessionDescriptionInit): Promise<void>;
  setRemoteDescription(description: RTCSessionDescriptionInit): Promise<void>;
  addEventListener(type: string, listener: (event: Event) => void): void;
  removeEventListener(type: string, listener: (event: Event) => void): void;
  getReceivers(): Array<{ track: WhepTrack | null }>;
  getSenders(): Array<{ track: WhepTrack | null }>;
  close(): void;
}

export interface StationWhepPreviewOptions {
  enabled: boolean;
  streamPath: string;
  namespace: string;
  origin: string;
  request: WhepSdpRequest;
  video: WhepPreviewVideo | null;
  iceGatheringTimeoutMs?: number;
  firstFrameBudgetMs?: number;
  renewIntervalMs?: number;
  createPeerConnection?: () => WhepPeer;
  onPlaying?: () => void;
  onFailure?: (failure: WhepFailure) => void;
}

export interface WhepPreviewHandle {
  stop(): Promise<void>;
}

class WhepControlError extends Error {
  readonly failure: WhepFailure;

  constructor(failure: WhepFailure) {
    super(failure.detail);
    this.name = "WhepControlError";
    this.failure = failure;
  }
}

interface StationWhepEnvironment {
  VITE_STATION_WHEP?: string;
  VITE_STATION_WHEP_NODES?: string;
}

function readStationWhepEnvironment(): StationWhepEnvironment {
  return (import.meta.env as StationWhepEnvironment | undefined) ?? {};
}

export function stationWhepEnabled(
  node: string,
  env?: StationWhepEnvironment,
): boolean {
  const configured = env ?? readStationWhepEnvironment();
  if (configured.VITE_STATION_WHEP !== "1" || !node) return false;
  const nodes =
    configured.VITE_STATION_WHEP_NODES?.split(",")
      .map((value) => value.trim())
      .filter(Boolean) ?? [];
  return nodes.length === 0 || nodes.includes(node);
}

// The resource is the stream path with only its final `/stream` segment rewritten.
// An earlier "stream" (a node name, for example) stays put, and the query keeps
// the namespace alone so fps and any other stream parameter are not forwarded.
export function cameraWhepPath(streamPath: string): string {
  const base = new URL(streamPath, "https://whep.invalid");
  const segments = base.pathname.split("/");
  if (segments[segments.length - 1] === "stream")
    segments[segments.length - 1] = "whep";
  const namespace = base.searchParams.get("namespace");
  const query = new URLSearchParams();
  if (namespace) query.set("namespace", namespace);
  const search = query.toString();
  return `${segments.join("/")}${search ? `?${search}` : ""}`;
}

// Gateway sessions are uuid.NewString(): 8-4-4-4-12 hex. Anything else is not a
// session id, so it must not become a PUT or DELETE target.
const WHEP_SESSION_UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// Location is accepted only when it is this POST pathname plus /<uuid>, on this
// origin, with the namespace query present once and equal to the camera namespace.
// Userinfo and fragments are rejected so a same-origin URL cannot retarget auth.
export function resolveWhepSession(
  location: string | null,
  namespace: string,
  origin: string,
  postPath: string,
): ResolvedWhepSession {
  if (
    location == null ||
    location.trim() === "" ||
    location !== location.trim()
  )
    return { ok: false, reason: "missing" };
  if (!namespace || !postPath) return { ok: false, reason: "namespace" };
  if (location.includes("#")) return { ok: false, reason: "resource" };
  if (location.includes("\\") || location.includes("@"))
    return { ok: false, reason: "cross-origin" };
  let resolved: URL;
  let page: URL;
  let posted: URL;
  try {
    page = new URL(origin);
    resolved = new URL(location, page);
    posted = new URL(postPath, page);
  } catch {
    return { ok: false, reason: "missing" };
  }
  if (
    page.origin === "null" ||
    page.username ||
    page.password ||
    posted.username ||
    posted.password
  ) {
    return { ok: false, reason: "cross-origin" };
  }
  if (resolved.username || resolved.password || resolved.hash || posted.hash) {
    return {
      ok: false,
      reason:
        resolved.username || resolved.password ? "cross-origin" : "resource",
    };
  }
  // A foreign origin must not receive the IAM Authorization header, cookies, or
  // a session PUT/DELETE. The caller treats this as a technical failure.
  if (resolved.origin !== page.origin || posted.origin !== page.origin)
    return { ok: false, reason: "cross-origin" };
  const names = resolved.searchParams.getAll("namespace");
  const extraQuery = [...resolved.searchParams.keys()].some(
    (key) => key !== "namespace",
  );
  if (extraQuery || names.length !== 1 || names[0] !== namespace)
    return { ok: false, reason: "namespace" };
  const base = posted.pathname.replace(/\/+$/, "") || "/";
  const prefix = `${base}/`;
  if (!resolved.pathname.startsWith(prefix))
    return { ok: false, reason: "resource" };
  const segment = resolved.pathname.slice(prefix.length);
  if (!WHEP_SESSION_UUID.test(segment))
    return { ok: false, reason: "resource" };
  return {
    ok: true,
    url: `${resolved.origin}${resolved.pathname}${resolved.search}`,
  };
}

export function failureFromStatus(status: number, detail: string): WhepFailure {
  if (status === 401 || status === 403)
    return { kind: "unauthorized", status, detail };
  if (status === 429) return { kind: "rate-limited", status, detail };
  return { kind: "technical", status, detail };
}

function failureFromUnknown(error: unknown): WhepFailure {
  if (error instanceof WhepControlError) return error.failure;
  if (error instanceof ApiError)
    return failureFromStatus(
      error.status,
      error.message || `HTTP ${error.status}`,
    );
  if (error instanceof Error)
    return { kind: "technical", detail: error.message || error.name };
  return { kind: "technical", detail: "whep failed" };
}

export function phaseAfterWhepFailure(failure: WhepFailure): WhepUiPhase {
  if (failure.kind === "unauthorized") return "unauthorized";
  if (failure.kind === "rate-limited") return "rate-limited";
  return "mjpeg";
}

export function whepRetryBlocked(phase: WhepUiPhase): boolean {
  return phase === "unauthorized" || phase === "rate-limited";
}

// Snapshot is the first fallback. MJPEG is requested only after a technical
// failure, and the existing scheduler still decides whether a slot exists.
// 401/403 request neither. 429 stays on the snapshot and does not ask for MJPEG.
export function cameraTransportPlan(input: {
  whepEnabled: boolean;
  phase: WhepUiPhase;
  mjpegGranted: boolean;
}): CameraTransportPlan {
  if (!input.whepEnabled) {
    return {
      postWhep: false,
      requestSnapshot: !input.mjpegGranted,
      requestMjpeg: input.mjpegGranted,
    };
  }
  switch (input.phase) {
    case "off":
    case "connecting":
    case "playing":
      return {
        postWhep: input.phase === "connecting",
        requestSnapshot: false,
        requestMjpeg: false,
      };
    case "unauthorized":
      return { postWhep: false, requestSnapshot: false, requestMjpeg: false };
    case "rate-limited":
      return { postWhep: false, requestSnapshot: true, requestMjpeg: false };
    case "mjpeg":
      return {
        postWhep: false,
        requestSnapshot: !input.mjpegGranted,
        requestMjpeg: true,
      };
    default: {
      const neverPhase: never = input.phase;
      return neverPhase;
    }
  }
}

function createRecvOnlyPeer(): WhepPeer {
  return new RTCPeerConnection() as WhepPeer;
}

function waitForIceGathering(
  peer: WhepPeer,
  timeoutMs: number,
  signal: AbortSignal,
): Promise<void> {
  if (peer.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve, reject) => {
    let settled = false;
    const finish = (done: () => void) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      peer.removeEventListener("icegatheringstatechange", onState);
      signal.removeEventListener("abort", onAbort);
      done();
    };
    const onState = () => {
      if (peer.iceGatheringState === "complete") finish(resolve);
    };
    const onAbort = () =>
      finish(() =>
        reject(new DOMException("The operation was aborted.", "AbortError")),
      );
    const timer = setTimeout(() => {
      finish(() =>
        reject(
          new WhepControlError({
            kind: "technical",
            detail: "ice-gathering-timeout",
          }),
        ),
      );
    }, timeoutMs);
    if (signal.aborted) {
      onAbort();
      return;
    }
    peer.addEventListener("icegatheringstatechange", onState);
    signal.addEventListener("abort", onAbort);
  });
}

function releasePeer(peer: WhepPeer | undefined) {
  if (!peer) return;
  for (const receiver of peer.getReceivers()) receiver.track?.stop();
  for (const sender of peer.getSenders()) sender.track?.stop();
  peer.close();
}

// DELETE is 202 from the Gateway and only best-effort here. The peer connection
// is released by the caller before this runs. Aborting bounds a hung proxy.
function deleteSession(url: string, request: WhepSdpRequest): Promise<void> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), WHEP_DELETE_TIMEOUT_MS);
  return request(url, {
    method: "DELETE",
    signal: controller.signal,
    keepalive: true,
  })
    .then(() => undefined)
    .catch(() => undefined)
    .finally(() => clearTimeout(timer));
}

export function startStationWhepPreview(
  options: StationWhepPreviewOptions,
): WhepPreviewHandle {
  if (!options.enabled) return { stop: () => Promise.resolve() };

  const iceTimeoutMs =
    options.iceGatheringTimeoutMs ?? WHEP_ICE_GATHERING_TIMEOUT_MS;
  const firstFrameBudgetMs =
    options.firstFrameBudgetMs ?? WHEP_FIRST_FRAME_BUDGET_MS;
  const renewEveryMs = options.renewIntervalMs ?? WHEP_RENEW_INTERVAL_MS;
  const attempt = new AbortController();
  const video = options.video;
  let attachedStream: MediaStream | undefined;
  let disposed = false;
  let playing = false;
  let armedVideo = false;
  let peer: WhepPeer | undefined;
  let sessionUrl: string | undefined;
  let renewTimer = 0;
  let firstFrameTimer = 0;
  let frameHandle = 0;
  const unlisten: Array<() => void> = [];
  let closing: Promise<void> | undefined;
  let resolveOpen = () => {};
  const openSettled = new Promise<void>((resolve) => {
    resolveOpen = resolve;
  });

  const clearRenewTimer = () => {
    if (!renewTimer) return;
    clearInterval(renewTimer);
    renewTimer = 0;
  };

  const clearFirstFrameWatch = () => {
    if (firstFrameTimer) {
      clearTimeout(firstFrameTimer);
      firstFrameTimer = 0;
    }
    if (!frameHandle) return;
    if (typeof cancelAnimationFrame === "function")
      cancelAnimationFrame(frameHandle);
    else clearTimeout(frameHandle);
    frameHandle = 0;
  };

  const dropListeners = () => {
    for (const stop of unlisten.splice(0)) stop();
  };

  const deleteAndClose = (): Promise<void> => {
    if (closing) return closing;
    clearRenewTimer();
    clearFirstFrameWatch();
    dropListeners();
    const url = sessionUrl;
    sessionUrl = undefined;
    const current = peer;
    peer = undefined;
    if (video && attachedStream && video.srcObject === attachedStream)
      video.srcObject = null;
    // Release the peer before DELETE. A hung teardown must not keep the
    // transceiver, the camera track, or the UI fallback waiting on the network.
    releasePeer(current);
    closing = url ? deleteSession(url, options.request) : Promise.resolve();
    return closing;
  };

  const fail = (failure: WhepFailure) => {
    if (disposed) return;
    disposed = true;
    attempt.abort();
    void deleteAndClose();
    options.onFailure?.(failure);
  };

  const stop = async () => {
    disposed = true;
    attempt.abort();
    await openSettled;
    const teardown = deleteAndClose();
    // Peer release already happened. Do not wait past the DELETE bound.
    await new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, WHEP_DELETE_TIMEOUT_MS);
      void teardown.finally(() => {
        clearTimeout(timer);
        resolve();
      });
    });
  };

  const listen = (
    target: {
      addEventListener(type: string, listener: () => void): void;
      removeEventListener(type: string, listener: () => void): void;
    },
    type: string,
    listener: () => void,
  ) => {
    target.addEventListener(type, listener);
    unlisten.push(() => target.removeEventListener(type, listener));
  };

  const markPlaying = () => {
    if (disposed || playing) return;
    playing = true;
    clearFirstFrameWatch();
    options.onPlaying?.();
  };

  const armTrack = (track: WhepTrack) => {
    if (armedVideo || track.kind !== "video") return;
    armedVideo = true;
    const onEnded = () => {
      // A track that ends is not a frame, before or after the first picture.
      void fail({ kind: "technical", detail: "track ended" });
    };
    track.addEventListener("ended", onEnded);
    unlisten.push(() => track.removeEventListener("ended", onEnded));
    if (track.readyState === "ended") {
      onEnded();
      return;
    }
    if (!video) return;

    const stream = new MediaStream([track as MediaStreamTrack]);
    attachedStream = stream;
    video.srcObject = stream;
    void video.play?.().catch(() => {
      // Autoplay rejection leaves the first-frame timeout in effect.
    });
    const requestFrame =
      typeof requestAnimationFrame === "function"
        ? (callback: () => void) => requestAnimationFrame(callback)
        : (callback: () => void) =>
            setTimeout(callback, 16) as unknown as number;
    const checkFrame = () => {
      if (disposed || playing || video.srcObject !== stream) return;
      if (track.readyState === "ended") {
        onEnded();
        return;
      }
      // HAVE_CURRENT_DATA requires a frame at the current playback position.
      // Track unmute and dimensions alone may arrive before decoding.
      if (!track.muted && video.readyState >= 2 && video.videoWidth > 0) {
        markPlaying();
        return;
      }
      frameHandle = requestFrame(checkFrame);
    };
    frameHandle = requestFrame(checkFrame);
  };

  const attachReceivers = (current: WhepPeer) => {
    for (const receiver of current.getReceivers()) {
      if (receiver.track) armTrack(receiver.track);
    }
    const onTrack = (event: Event) => {
      const track = (event as RTCTrackEvent).track as WhepTrack | null;
      if (track) armTrack(track);
    };
    current.addEventListener("track", onTrack);
    unlisten.push(() => current.removeEventListener("track", onTrack));
  };

  void (async () => {
    try {
      const created = (options.createPeerConnection ?? createRecvOnlyPeer)();
      peer = created;
      created.addTransceiver("video", { direction: "recvonly" });
      if (disposed) return;
      const description = await created.createOffer();
      const offer: WhepSdpOffer = { type: "offer", sdp: description.sdp ?? "" };
      if (disposed) return;
      await created.setLocalDescription(offer);
      if (disposed) return;
      // Non-trickle: the POST body is the gathered offer, not a partial one.
      await waitForIceGathering(created, iceTimeoutMs, attempt.signal);
      if (disposed) return;
      const sdp = created.localDescription?.sdp ?? offer.sdp;
      if (!sdp.trim()) {
        await fail({ kind: "technical", detail: "empty offer sdp" });
        return;
      }
      const postPath = cameraWhepPath(options.streamPath);
      const response = await options.request(postPath, {
        method: "POST",
        headers: {
          "Content-Type": "application/sdp",
          Accept: "application/sdp",
        },
        body: sdp,
        signal: attempt.signal,
      });
      // Gateway create is 201. A returned Response is classified here; a thrown
      // ApiError is classified in failureFromUnknown. 401/403/429 never fall through
      // as a generic status, whether or not the helper threw.
      const resolved = resolveWhepSession(
        response.headers.get("Location"),
        options.namespace,
        options.origin,
        postPath,
      );
      if (response.status !== 201) {
        if (resolved.ok) sessionUrl = resolved.url;
        await fail(
          failureFromStatus(response.status, "unexpected whep status"),
        );
        return;
      }
      if (!resolved.ok) {
        await fail({
          kind: "technical",
          detail: `whep location ${resolved.reason}`,
        });
        return;
      }
      sessionUrl = resolved.url;
      if (disposed) return;
      const answerText = await response.text();
      if (!answerText.trim()) {
        await fail({ kind: "technical", detail: "empty answer sdp" });
        return;
      }
      const answer: WhepSdpAnswer = { type: "answer", sdp: answerText };
      await created.setRemoteDescription(answer);
      if (disposed) return;
      attachReceivers(created);
      // An ended track fails inside attach and has already started teardown.
      if (disposed) return;
      const onIce = () => {
        const state = created.iceConnectionState;
        if (state === "failed" || state === "disconnected") {
          void fail({ kind: "technical", detail: `ice ${state}` });
        }
      };
      listen(created, "iceconnectionstatechange", onIce);
      if (video) {
        const onVideoError = () => {
          void fail({ kind: "technical", detail: "video error" });
        };
        video.addEventListener("error", onVideoError);
        unlisten.push(() => video.removeEventListener("error", onVideoError));
      }
      // A muted or ended track that never produces a frame is a failure. The
      // timer is the existing first-frame budget, not a second retry loop.
      if (!playing) {
        firstFrameTimer = setTimeout(() => {
          firstFrameTimer = 0;
          if (!playing)
            void fail({ kind: "technical", detail: "first-frame timeout" });
        }, firstFrameBudgetMs);
      }
      renewTimer = setInterval(() => {
        void renew();
      }, renewEveryMs);
    } catch (error) {
      if (!disposed && !attempt.signal.aborted)
        await fail(failureFromUnknown(error));
    } finally {
      resolveOpen();
    }
  })();

  async function renew() {
    const url = sessionUrl;
    if (!url || disposed) return;
    try {
      const response = await options.request(url, {
        method: "PUT",
        signal: attempt.signal,
      });
      if (disposed) return;
      // Gateway renew is 204. 202 is the DELETE status and is not a renew.
      if (response.status !== 204) {
        await fail(failureFromStatus(response.status, "whep renew failed"));
      }
    } catch (error) {
      if (disposed || attempt.signal.aborted) return;
      // 404 means the session is already gone: technical, so the caller snapshots
      // and may start MJPEG inside the existing budget. 401/403 do not fall back.
      await fail(failureFromUnknown(error));
    }
  }

  return { stop };
}

// Keep a granted preview alive across a temporary Gateway capacity response.
// Its owner cancels the retry when the preview is no longer scheduled.
export function startScheduledStationWhepPreview(
  options: StationWhepPreviewOptions,
): WhepPreviewHandle {
  let stopped = false;
  let retryTimer: ReturnType<typeof setTimeout> | undefined;
  let retryDelayMs = 1000;

  const start = (): WhepPreviewHandle =>
    startStationWhepPreview({
      ...options,
      onPlaying: () => {
        retryDelayMs = 1000;
        options.onPlaying?.();
      },
      onFailure: (failure) => {
        if (stopped) return;
        options.onFailure?.(failure);
        if (failure.kind !== "rate-limited") return;
        retryTimer = setTimeout(() => {
          retryTimer = undefined;
          if (!stopped) preview = start();
        }, retryDelayMs);
        retryDelayMs = Math.min(retryDelayMs * 2, 4000);
      },
    });
  let preview = start();
  return {
    stop: () => {
      stopped = true;
      if (retryTimer !== undefined) clearTimeout(retryTimer);
      return preview.stop();
    },
  };
}
