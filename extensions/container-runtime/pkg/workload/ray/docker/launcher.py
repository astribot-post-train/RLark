"""Internal Docker PID-1 launcher, compiled only from a trusted Ray assignment.

Generated commands use argv. Only explicit user scripts cross a shell boundary.
Ray's private GCS protobuf API is deliberately pinned to the tested version.
"""

import json
import os
import signal
import subprocess
import sys
import time


class LaunchFailure(Exception):
    pass


class Cancelled(Exception):
    pass


def main(config):
    stopped = False
    children = []

    def stop(_signum, _frame):
        nonlocal stopped
        stopped = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    def check_cancelled():
        if stopped:
            raise Cancelled()

    def spawn(argv):
        check_cancelled()
        child = subprocess.Popen(argv, start_new_session=True)
        children.append(child)
        return child

    def wait(child, ray=None):
        while True:
            check_cancelled()
            if ray is not None and ray.poll() is not None:
                raise LaunchFailure("Ray member exited while training")
            result = child.poll()
            if result is not None:
                return result if result >= 0 else 128 - result
            time.sleep(0.1)

    def cleanup():
        # Even an exited script may have left descendants in its process group.
        # Production runs inside one Docker PID namespace; PID-1 exit also
        # terminates remaining descendants that changed their process groups.
        for sig, timeout in ((signal.SIGTERM, 5), (signal.SIGKILL, 2)):
            for child in reversed(children):
                try:
                    os.killpg(child.pid, sig)
                except ProcessLookupError:
                    pass
            deadline = time.monotonic() + timeout
            for child in children:
                try:
                    child.wait(timeout=max(0, deadline - time.monotonic()))
                except subprocess.TimeoutExpired:
                    pass

    channel = None
    try:
        # Check before a user prepare script. Preparation cannot silently turn
        # an unsupported image into a different launcher protocol.
        import grpc
        import ray
        import ray.scripts.scripts
        from ray.core.generated import gcs_pb2, gcs_service_pb2, gcs_service_pb2_grpc

        if ray.__version__ != "2.56.1":
            raise LaunchFailure("image requires Ray CLI/SDK 2.56.1")
        if config.get("prepare"):
            result = wait(spawn(["/bin/sh", "-eu", "-c", config["prepare"]]))
            if result:
                return result
        # Use this interpreter's CLI module, never a different virtualenv's
        # `ray` executable that happens to appear first on PATH.
        ray_process = spawn([sys.executable, "-m", "ray.scripts.scripts", *config["start"][1:]])
        channel = grpc.insecure_channel(
            config["address"], options=[("grpc.max_receive_message_length", 1048576)]
        )
        nodes = gcs_service_pb2_grpc.NodeInfoGcsServiceStub(channel)
        expected = sorted(config["members"])
        deadline = time.monotonic() + config["startupSeconds"]
        while True:
            check_cancelled()
            if ray_process.poll() is not None:
                raise LaunchFailure("Ray member exited before readiness")
            if time.monotonic() >= deadline:
                raise LaunchFailure("Ray membership readiness timed out")
            try:
                reply = nodes.GetAllNodeInfo(
                    gcs_service_pb2.GetAllNodeInfoRequest(),
                    timeout=min(2, max(0.01, deadline - time.monotonic())),
                )
                if reply.status.code != 0:
                    raise LaunchFailure("Ray membership query failed")
                alive = sorted(
                    node.node_manager_address
                    for node in reply.node_info_list
                    if node.state == gcs_pb2.GcsNodeInfo.ALIVE
                )
                if any(address not in expected for address in alive) or len(set(alive)) != len(alive):
                    raise LaunchFailure("Ray membership conflicts with assignment")
                if alive == expected:
                    break
            except grpc.RpcError:
                pass  # Startup is retryable only within the bounded deadline.
            time.sleep(0.1)
        print("RLark Ray membership ready", flush=True)
        if config["head"]:
            argv = config.get("command")
            if config.get("run"):
                argv = ["/bin/sh", "-eu", "-c", config["run"]]
            if not argv:
                raise LaunchFailure("Ray head has no training command")
            return wait(spawn(argv), ray_process)
        # A worker is a required resident service. Any spontaneous exit,
        # including zero, fails it; group stop is separate durable admission.
        wait(ray_process)
        raise LaunchFailure("Ray worker exited unexpectedly")
    except Cancelled:
        return 143
    except (LaunchFailure, ImportError, OSError, ValueError, KeyError):
        # Generated argv and user scripts may contain secrets. Do not include
        # exception strings, configuration or child environment in diagnostics.
        print("RLark Ray launcher failed", file=sys.stderr, flush=True)
        return 1
    finally:
        if channel is not None:
            channel.close()
        cleanup()


if __name__ == "__main__":
    sys.exit(main(json.loads(sys.argv[1])))
