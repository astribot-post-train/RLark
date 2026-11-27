// Station-camera WHEP. JPEG stays the default. A session is opened only when
// VITE_STATION_WHEP is exactly "1", and it uses the same `request` helper as the
// JPEG paths so the IAM bearer token is attached once, on the console origin.
import { ApiError } from "../../api.js";

/** The card's JPEG stream watchdog. ICE gathering and the first frame share it. */
export const WHEP_ICE_GATHERING_TIMEOUT_MS = 8000;
export const WHEP_FIRST_FRAME_BUDGET_MS = WHEP_ICE_GATHERING_TIMEOUT_MS;
export const WHEP_RENEW_INTERVAL_MS = 6000;

export type WhepFailureKind = "unauthorized" | "rate-limited" | "technical";

export interface WhepFailure {
  kind: WhepFailureKind;
  status?: number;
  detail: string;
}

export type WhepUiPhase =
  | "off"
  | "connecting"
  | "playing"
  | "rate-limited"
  | "unauthorized"
  | "mjpeg";

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
  | { ok: false; reason: "missing" | "namespace" | "cross-origin" };

export interface WhepSdpRequest {
  (
    input: RequestInfo | URL,
    init?: {
      method?: string;
      headers?: HeadersInit;
      body?: BodyInit | null;
      signal?: AbortSignal;
    },
  ): Promise<Response>;
}

export interface WhepPreviewVideo {
  srcObject: MediaProvider | null;
  videoWidth: number;
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
  addTransceiver(kind: "video", init: { direction: RTCRtpTransceiverDirection }): void;
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

function readStationWhepFlag(): string | undefined {
  const env = import.meta.env as { VITE_STATION_WHEP?: string } | undefined;
  return env?.VITE_STATION_WHEP;
}

export function stationWhepEnabled(env?: { VITE_STATION_WHEP?: string }): boolean {
  const value = env ? env.VITE_STATION_WHEP : readStationWhepFlag();
  return value === "1";
}

// The resource is the stream path with only its final `/stream` segment rewritten.
// An earlier "stream" (a node name, for example) stays put, and the query keeps
// the namespace alone so fps and any other stream parameter are not forwarded.
export function cameraWhepPath(streamPath: string): string {
  const base = new URL(streamPath, "https://whep.invalid");
  const segments = base.pathname.split("/");
  if (segments[segments.length - 1] === "stream") segments[segments.length - 1] = "whep";
  const namespace = base.searchParams.get("namespace");
  const query = new URLSearchParams();
  if (namespace) query.set("namespace", namespace);
  const search = query.toString();
  return `${segments.join("/")}${search ? `?${search}` : ""}`;
}

export function resolveWhepSession(
  location: string | null,
  namespace: string,
  origin: string,
): ResolvedWhepSession {
  if (location == null || location.trim() === "") return { ok: false, reason: "missing" };
  if (!namespace || !location.includes(namespace)) return { ok: false, reason: "namespace" };
  let resolved: URL;
  let page: URL;
  try {
    resolved = new URL(location, origin);
    page = new URL(origin);
  } catch {
    return { ok: false, reason: "missing" };
  }
  // A foreign origin must not receive the IAM Authorization header, cookies, or
  // a session PUT/DELETE. The caller treats this as a technical failure.
  if (resolved.origin !== page.origin) return { ok: false, reason: "cross-origin" };
  return { ok: true, url: resolved.href };
}

export function failureFromStatus(status: number, detail: string): WhepFailure {
  if (status === 401 || status === 403) return { kind: "unauthorized", status, detail };
  if (status === 429) return { kind: "rate-limited", status, detail };
  return { kind: "technical", status, detail };
}

function failureFromUnknown(error: unknown): WhepFailure {
  if (error instanceof WhepControlError) return error.failure;
  if (error instanceof ApiError) return failureFromStatus(error.status, error.message || `HTTP ${error.status}`);
  if (error instanceof Error) return { kind: "technical", detail: error.message || error.name };
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
      return { postWhep: input.phase === "connecting", requestSnapshot: false, requestMjpeg: false };
    case "unauthorized":
      return { postWhep: false, requestSnapshot: false, requestMjpeg: false };
    case "rate-limited":
      return { postWhep: false, requestSnapshot: true, requestMjpeg: false };
    case "mjpeg":
      return { postWhep: false, requestSnapshot: !input.mjpegGranted, requestMjpeg: true };
    default: {
      const neverPhase: never = input.phase;
      return neverPhase;
    }
  }
}

function createRecvOnlyPeer(): WhepPeer {
  return new RTCPeerConnection() as WhepPeer;
}

function waitForIceGathering(peer: WhepPeer, timeoutMs: number, signal: AbortSignal): Promise<void> {
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
    const onAbort = () => finish(() => reject(new DOMException("The operation was aborted.", "AbortError")));
    const timer = setTimeout(() => {
      finish(() => reject(new WhepControlError({ kind: "technical", detail: "ice-gathering-timeout" })));
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

export function startStationWhepPreview(options: StationWhepPreviewOptions): WhepPreviewHandle {
  if (!options.enabled) return { stop: () => Promise.resolve() };

  const iceTimeoutMs = options.iceGatheringTimeoutMs ?? WHEP_ICE_GATHERING_TIMEOUT_MS;
  const firstFrameBudgetMs = options.firstFrameBudgetMs ?? WHEP_FIRST_FRAME_BUDGET_MS;
  const renewEveryMs = options.renewIntervalMs ?? WHEP_RENEW_INTERVAL_MS;
  const attempt = new AbortController();
  const video = options.video;
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
    if (typeof cancelAnimationFrame === "function") cancelAnimationFrame(frameHandle);
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
    if (video) video.srcObject = null;
    closing = (async () => {
      if (url) {
        try {
          await options.request(url, { method: "DELETE" });
        } catch {
          // The peer connection still has to close when DELETE fails.
        }
      }
      releasePeer(current);
    })();
    return closing;
  };

  const fail = async (failure: WhepFailure) => {
    if (disposed) return;
    disposed = true;
    attempt.abort();
    await deleteAndClose();
    options.onFailure?.(failure);
  };

  const stop = async () => {
    disposed = true;
    attempt.abort();
    await openSettled;
    await deleteAndClose();
  };

  const listen = (
    target: { addEventListener(type: string, listener: () => void): void; removeEventListener(type: string, listener: () => void): void },
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
    const onUnmute = () => {
      if (track.readyState === "ended") {
        onEnded();
        return;
      }
      if (!track.muted) markPlaying();
    };
    track.addEventListener("ended", onEnded);
    track.addEventListener("unmute", onUnmute);
    unlisten.push(() => {
      track.removeEventListener("ended", onEnded);
      track.removeEventListener("unmute", onUnmute);
    });
    if (video) {
      video.srcObject = new MediaStream([track as MediaStreamTrack]);
      void video.play?.().catch(() => {
        // Autoplay rejection is not a frame. The first-frame budget still applies.
      });
    }
    if (track.readyState === "ended") {
      onEnded();
      return;
    }
    if (!track.muted && video && video.videoWidth > 0) {
      markPlaying();
      return;
    }
    if (!track.muted) {
      const requestFrame = typeof requestAnimationFrame === "function"
        ? (callback: () => void) => requestAnimationFrame(callback)
        : (callback: () => void) => setTimeout(callback, 16) as unknown as number;
      const tick = () => {
        if (disposed || playing) return;
        if (track.readyState === "ended") {
          onEnded();
          return;
        }
        if (!track.muted && video && video.videoWidth > 0) {
          markPlaying();
          return;
        }
        frameHandle = requestFrame(tick);
      };
      frameHandle = requestFrame(tick);
    }
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
      const response = await options.request(cameraWhepPath(options.streamPath), {
        method: "POST",
        headers: {
          "Content-Type": "application/sdp",
          Accept: "application/sdp",
        },
        body: sdp,
        signal: attempt.signal,
      });
      const answerText = await response.text();
      const resolved = resolveWhepSession(response.headers.get("Location"), options.namespace, options.origin);
      if (response.status !== 201) {
        if (resolved.ok) sessionUrl = resolved.url;
        await fail({ kind: "technical", status: response.status, detail: "unexpected whep status" });
        return;
      }
      if (!resolved.ok) {
        await fail({ kind: "technical", detail: `whep location ${resolved.reason}` });
        return;
      }
      sessionUrl = resolved.url;
      if (disposed) return;
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
          if (!playing) void fail({ kind: "technical", detail: "first-frame timeout" });
        }, firstFrameBudgetMs);
      }
      renewTimer = setInterval(() => {
        void renew();
      }, renewEveryMs);
    } catch (error) {
      if (!disposed && !attempt.signal.aborted) await fail(failureFromUnknown(error));
    } finally {
      resolveOpen();
    }
  })();

  async function renew() {
    const url = sessionUrl;
    if (!url || disposed) return;
    try {
      const response = await options.request(url, { method: "PUT", signal: attempt.signal });
      if (disposed) return;
      if (response.status !== 204) {
        await fail({ kind: "technical", status: response.status, detail: "whep renew failed" });
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
