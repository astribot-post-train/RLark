"""Bounded H.264 publisher for the S1U camera Addon."""

import os
import select
import subprocess
import threading
import time
from urllib.parse import urlsplit


def encoder_command(destination, fps):
    target = urlsplit(destination)
    if target.scheme not in ("rtsp", "rtsps") or not target.hostname or not target.path.strip("/"):
        raise ValueError("an RTSP publisher needs a configured host and stream path")
    if not 1 <= fps <= 10:
        raise ValueError("the H.264 substream supports 1 through 10 FPS")

    return [
        "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
        "-f", "image2pipe", "-framerate", str(fps), "-c:v", "mjpeg", "-i", "pipe:0",
        "-an", "-vf",
        "scale=854:480:force_original_aspect_ratio=decrease:force_divisible_by=2,"
        "pad=854:480:(ow-iw)/2:(oh-ih)/2,setsar=1",
        "-c:v", "libx264", "-profile:v", "baseline", "-pix_fmt", "yuv420p",
        "-preset", "ultrafast", "-tune", "zerolatency", "-threads", "2",
        "-b:v", "600k", "-maxrate", "800k", "-bufsize", "800k",
        "-g", str(fps * 2), "-bf", "0",
        "-x264-params", "repeat-headers=1:scenecut=0",
        "-f", "rtsp", "-rtsp_transport", "tcp", destination,
    ]


def publish(camera_frame, destination, fps, lease_active):
    """Forward fresh JPEGs until the lease expires or the source stops.

    camera_frame returns (JPEG, age in seconds, sequence), without waiting.
    The caller owns retry policy. This function owns the child and reaps it
    on every exit. It does not expose the publisher URL in logs.
    """
    process = subprocess.Popen(
        encoder_command(destination, fps), stdin=subprocess.PIPE,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        descriptor = process.stdin.fileno()
        os.set_blocking(descriptor, False)
        poll = select.poll()
        poll.register(descriptor, select.POLLOUT)
        previous = None
        last_frame = time.monotonic()
        while lease_active():
            if process.poll() is not None:
                return "encoder_exited"
            began = time.monotonic()
            jpeg, age, sequence = camera_frame()
            if jpeg and age is not None and age < 5 and sequence != previous:
                remaining = memoryview(jpeg)
                deadline = began + 2
                while remaining:
                    if not lease_active():
                        return "lease_expired"
                    if time.monotonic() >= deadline:
                        return "encoder_stalled"
                    if not poll.poll(100):
                        continue
                    try:
                        remaining = remaining[os.write(descriptor, remaining):]
                    except BlockingIOError:
                        continue
                    except BrokenPipeError:
                        return "encoder_exited"
                previous = sequence
                last_frame = time.monotonic()
            elif began - last_frame >= 5:
                return "source_stale"
            time.sleep(max(0, 1 / fps - (time.monotonic() - began)))
        return "lease_expired"
    finally:
        process.stdin.close()
        try:
            process.terminate()
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            try:
                process.kill()
            except ProcessLookupError:
                pass
            process.wait()


class SourceLease:
    """Keep at most one publisher active on this robot."""

    def __init__(self, frame, destination_prefix, fps=8):
        self.frame = frame
        self.destination_prefix = destination_prefix.rstrip("/")
        self.fps = fps
        self.lock = threading.Lock()
        self.view = None
        self.deadline = 0

    def renew(self, view):
        with self.lock:
            if self.view is not None:
                # Once the deadline passes, wait until the previous child
                # has actually exited before allowing a new lease.
                if self.view != view or self.deadline <= time.monotonic():
                    return False
                self.deadline = time.monotonic() + 20
                return True
            self.view = view
            self.deadline = time.monotonic() + 20
            threading.Thread(target=self.run, args=(view,), daemon=True).start()
            return True

    def run(self, view):
        def active():
            with self.lock:
                return time.monotonic() < self.deadline

        try:
            publish(lambda: self.frame(view), self.destination_prefix + "/" + view, self.fps, active)
        except (OSError, ValueError):
            # The existing JPEG service remains available if the optional
            # encoder is missing or the configured destination is invalid.
            pass
        finally:
            with self.lock:
                self.view = None
                self.deadline = 0
