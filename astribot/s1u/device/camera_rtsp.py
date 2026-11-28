"""Bounded H.264 publisher for the S1U camera Addon."""

import os
import re
import select
import subprocess
import threading
import time
from urllib.parse import quote, urlsplit, urlunsplit


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


def publisher_path(destination_prefix, node):
    """Validate the provisioned MediaMTX path against this Addon's node."""
    target = urlsplit(destination_prefix)
    prefix = target.path.rstrip("/")
    agent, slash, named_node = prefix.lstrip("/").partition("/")
    if (target.scheme not in ("rtsp", "rtsps") or not target.hostname or
            target.query or target.fragment or not slash or named_node != node or
            not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", agent)):
        raise ValueError("invalid MediaMTX publisher prefix")
    return prefix


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
        try:
            process.stdin.close()
        finally:
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

    def __init__(self, frame, destination_prefix, node, fps=8, username="", password=""):
        self.frame = frame
        target = urlsplit(destination_prefix.rstrip("/"))
        if bool(username) != bool(password) or (username and (target.username or target.password)):
            raise ValueError("RTSP credentials must be a username/password pair outside the URL")
        if username:
            netloc = "%s:%s@%s" % (quote(username, safe=""), quote(password, safe=""), target.netloc)
            target = target._replace(netloc=netloc)
        self.destination_prefix = urlunsplit(target)
        self.path = publisher_path(self.destination_prefix, node)
        self.fps = fps
        self.lock = threading.Lock()
        self.view = None
        self.readers = {}

    def prune(self, now):
        expired = [reader for reader, deadline in self.readers.items() if deadline <= now]
        for reader in expired:
            del self.readers[reader]

    def renew(self, view, reader):
        with self.lock:
            now = time.monotonic()
            self.prune(now)
            if self.view not in (None, view) or (reader not in self.readers and len(self.readers) >= 20):
                return False
            self.readers[reader] = now + 20
            if self.view is None:
                self.view = view
                threading.Thread(target=self.run, args=(view,), daemon=True).start()
            return True

    def release(self, view, reader):
        with self.lock:
            if self.view == view:
                self.readers.pop(reader, None)

    def run(self, view):
        def active():
            with self.lock:
                self.prune(time.monotonic())
                return bool(self.readers)

        while True:
            with self.lock:
                self.prune(time.monotonic())
                if not self.readers:
                    self.view = None
                    return
            try:
                result = publish(lambda: self.frame(view), self.destination_prefix + "/" + view, self.fps, active)
            except (OSError, ValueError):
                result = "encoder_unavailable"
            if result != "lease_expired":
                # Fixed result labels only. FFmpeg diagnostics may expose
                # the authenticated RTSP URL.
                print("Camera H.264 publisher %s: %s" % (view, result), flush=True)
                time.sleep(1)
