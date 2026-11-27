const MAX_HEADER_BYTES = 8192;
const MAX_JPEG_BYTES = 16 * 1024 * 1024;
const LF = 10;

export interface CameraFrame {
  jpeg: Uint8Array<ArrayBuffer>;
  capturedAtMs?: number;
  ageSeconds?: number;
}

// The camera service sends a Content-Length for every JPEG. Reading exactly
// that many bytes also allows boundary-like bytes inside the JPEG.
export async function* readCameraFrames(
  response: Response,
): AsyncGenerator<CameraFrame> {
  const contentType = response.headers.get("Content-Type") ?? "";
  const boundary = /;\s*boundary\s*=\s*(?:"([^"]+)"|([^;\s]+))/i.exec(
    contentType,
  );
  const marker = boundary?.[1] ?? boundary?.[2];
  if (
    !/^multipart\/x-mixed-replace\s*;/i.test(contentType) ||
    !marker ||
    !response.body
  ) {
    throw new Error("Invalid camera stream");
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  const header = new Uint8Array(MAX_HEADER_BYTES);
  let headerSize = 0;
  let lineStart = 0;
  let frame: Uint8Array<ArrayBuffer> | undefined;
  let frameSize = 0;
  let capturedAtMs: number | undefined;
  let ageSeconds: number | undefined;
  try {
    while (true) {
      const { value: chunk, done } = await reader.read();
      if (done) {
        if (frame || headerSize) throw new Error("Incomplete camera frame");
        return;
      }
      for (let offset = 0; offset < chunk.length;) {
        if (frame) {
          const size = Math.min(
            chunk.length - offset,
            frame.length - frameSize,
          );
          frame.set(chunk.subarray(offset, offset + size), frameSize);
          offset += size;
          frameSize += size;
          if (frameSize === frame.length) {
            yield { jpeg: frame, capturedAtMs, ageSeconds };
            frame = undefined;
            frameSize = 0;
          }
        } else {
          const byte = chunk[offset++];
          if (headerSize === header.length)
            throw new Error("Camera frame headers are too large");
          header[headerSize++] = byte;
          if (byte !== LF) continue;
          const line = decoder
            .decode(header.subarray(lineStart, headerSize))
            .trim();
          if (line === `--${marker}--`) return;
          if (line) {
            lineStart = headerSize;
            continue;
          }
          const headers = decoder.decode(header.subarray(0, lineStart)).trim();
          headerSize = 0;
          lineStart = 0;
          if (!headers) continue; // CRLF between frames
          const length = Number(
            /^content-length:\s*(\d+)\s*$/im.exec(headers)?.[1],
          );
          if (
            headers.split(/\r?\n/, 1)[0] !== `--${marker}` ||
            !/^content-type:\s*image\/jpeg\s*$/im.test(headers) ||
            !Number.isSafeInteger(length) ||
            length <= 0 ||
            length > MAX_JPEG_BYTES
          ) {
            throw new Error("Invalid camera frame headers");
          }
          frame = new Uint8Array(length);
          const age = Number(
            /^x-age-seconds:[ \t]*(\d+(?:\.\d+)?)[ \t]*$/im.exec(headers)?.[1],
          );
          ageSeconds = Number.isFinite(age) ? age : undefined;
          const stamp = Number(
            /^x-camera-capture-ms:\s*(\d+(?:\.\d+)?)\s*$/im.exec(headers)?.[1],
          );
          capturedAtMs =
            Number.isFinite(stamp) && stamp > 0 ? stamp : undefined;
        }
      }
    }
  } finally {
    void reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

export function sameCameraFrame(
  previous: CameraFrame | undefined,
  next: CameraFrame,
): boolean {
  if (!previous) return false;
  if (previous.capturedAtMs !== undefined || next.capturedAtMs !== undefined) {
    return previous.capturedAtMs === next.capturedAtMs;
  }
  return (
    previous.jpeg.length === next.jpeg.length &&
    next.jpeg.every((byte, index) => previous.jpeg[index] === byte)
  );
}

export function cameraFrameStale(frame: CameraFrame): boolean {
  return frame.ageSeconds !== undefined && frame.ageSeconds >= 5;
}

export function cameraStreamIssue(status?: number, waiting = false) {
  if (status === 429) return "capacity";
  if (status === 503 || waiting) return "no-frame";
  return "disconnected";
}
