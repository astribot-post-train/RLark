#!/usr/bin/env python3
"""Private Notebook service supervisor. No systemd, Docker daemon or root network privileges."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time

from notebook_identity import validate_instance


def read(path):
    with open(path, encoding="utf-8") as source:
        return json.load(source)


def atomic(path, value):
    temporary = str(path) + ".tmp"
    with open(temporary, "w", encoding="utf-8") as output:
        os.chmod(temporary, 0o600)
        json.dump(value, output)
    os.replace(temporary, path)


def digest(config):
    return hashlib.sha256(json.dumps(config, sort_keys=True).encode()).hexdigest()


def prepare_runtime(config_path, config):
    """Fail before starting services when the existing workload layout is invalid."""
    supervisor = Path(config_path).parent / 'supervisor.json'
    if supervisor.exists():
        workload = read(supervisor)
        for key in ('executables', 'workDirectories'):
            for raw in workload[key]:
                path = Path(raw)
                if not path.is_absolute() or path.resolve(strict=True) != path:
                    raise RuntimeError('workload allowlist path must exist and be canonical')
                if key == 'executables' and (not path.is_file() or not os.access(path, os.X_OK)):
                    raise RuntimeError('workload executable is unavailable')
                if key == 'workDirectories' and not path.is_dir():
                    raise RuntimeError('workload directory is unavailable')
    state = Path(config['stateDirectory'])
    for raw in config.get('runtimeDirectories', []):
        path = Path(raw)
        if not path.is_absolute() or path.parent != state.parent or path.name not in ('tasks', 'tasks-agent-upgrades'):
            raise RuntimeError('runtime directory is outside this node state')
        if path.resolve() != path:
            raise RuntimeError('runtime directory must be canonical and private')
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
        if path.resolve() != path or path.stat().st_mode & 0o077:
            raise RuntimeError('runtime directory must be canonical and private')


def run_service(config_path, name):
    config = read(config_path)
    validate_instance(config)
    root = Path(config["stateDirectory"])
    lock = open(root / (name + ".lock"), "a", encoding="utf-8")
    os.chmod(root / (name + ".lock"), 0o600)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return
    os.set_inheritable(lock.fileno(), True)
    argv = config["services"][name]
    fields = Path(f"/proc/{os.getpid()}/stat").read_text().rsplit(") ", 1)[1].split()
    atomic(root / (name + ".pid.json"), {"identity": config["identity"], "digest": digest(config), "pid": os.getpid(), "started": fields[19]})
    os.execv(argv[0], argv)


class RetainedProcess:
    """Adopt only a PID with the exact Linux process start-time receipt."""
    def __init__(self, receipt):
        self.pid = receipt["pid"]
        self.started = receipt["started"]

    def poll(self):
        try:
            fields = Path(f"/proc/{self.pid}/stat").read_text().rsplit(") ", 1)[1].split()
            return None if fields[19] == self.started and fields[0] != "Z" else 1
        except (FileNotFoundError, ProcessLookupError):
            return 1

    def terminate(self):
        if self.poll() is None:
            os.kill(self.pid, signal.SIGTERM)

    def kill(self):
        if self.poll() is None:
            os.kill(self.pid, signal.SIGKILL)

    def wait(self, timeout=10):
        deadline = time.monotonic() + timeout
        while self.poll() is None:
            if time.monotonic() >= deadline:
                raise subprocess.TimeoutExpired("retained service", timeout)
            time.sleep(0.1)


def serve(config_path):
    config = read(config_path)
    validate_instance(config)
    prepare_runtime(config_path, config)
    root = Path(config["stateDirectory"])
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    lock = open(root / "services.lock", "a", encoding="utf-8")
    os.chmod(root / "services.lock", 0o600)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return
    stopping = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stopping.set())
    signal.signal(signal.SIGINT, lambda *_: stopping.set())
    processes, deadlines = {}, {}
    receipts_path = root / "processes.json"
    try:
        retained = read(receipts_path)
        if retained["identity"] != config["identity"] or retained["digest"] != digest(config):
            raise RuntimeError("foreign retained services")
        for name, receipt in retained["processes"].items():
            processes[name] = RetainedProcess(receipt)
    except FileNotFoundError:
        pass
    services = dict(config["services"])
    try:
        while not stopping.is_set():
            # Configuration is immutable for a running service set.
            current = read(config_path)
            if digest(current) != digest(config):
                raise RuntimeError("service configuration changed; explicit idle upgrade required")
            validate_instance(config)
            for name, argv in services.items():
                process = processes.get(name)
                if (process is None or process.poll() is not None) and time.monotonic() >= deadlines.get(name, 0):
                    try:
                        receipt = read(root / (name + ".pid.json"))
                        if receipt["identity"] != config["identity"] or receipt["digest"] != digest(config):
                            raise RuntimeError("foreign service receipt")
                        retained_process = RetainedProcess(receipt)
                        if retained_process.poll() is None:
                            processes[name] = retained_process
                            continue
                    except FileNotFoundError:
                        pass
                    log_path = root / (name + ".log")
                    with open(log_path, "ab", buffering=0) as output:
                        os.chmod(log_path, 0o600)
                        process = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "run", config_path, name], stdin=subprocess.DEVNULL, stdout=output,
                                                   stderr=subprocess.STDOUT, start_new_session=True)
                    processes[name] = process
                    receipts = {}
                    for key, item in processes.items():
                        if item.poll() is None:
                            fields = Path(f"/proc/{item.pid}/stat").read_text().rsplit(") ", 1)[1].split()
                            receipts[key] = {"pid": item.pid, "started": fields[19]}
                    atomic(receipts_path, {"identity": config["identity"], "digest": digest(config), "processes": receipts})
                    deadlines[name] = time.monotonic() + 5
                log_path = root / (name + ".log")
                if log_path.exists() and log_path.stat().st_size > 1024 * 1024:
                    with open(log_path, "wb"):
                        pass
            atomic(root / "services-status.json", {
                "identity": config["identity"], "digest": digest(config), "sampledAt": time.time(),
                "services": sorted(name for name, process in processes.items() if name in services and process.poll() is None),
            })
            stopping.wait(1)
    finally:
        # Essential services and their Raw tasks survive manager restarts.
        # A later manager adopts them only through matching PID/start receipts.
        atomic(root / "services-status.json", {"identity": config["identity"], "digest": digest(config), "sampledAt": 0, "services": []})
        lock.close()


def ensure(config_path):
    config = read(config_path)
    validate_instance(config)
    prepare_runtime(config_path, config)
    root = Path(config["stateDirectory"])
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    lock = open(root / "services.lock", "a", encoding="utf-8")
    os.chmod(root / "services.lock", 0o600)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        # Child acquires the lock; concurrent starts race safely there.
        lock.close()
        subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "serve", config_path],
                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         start_new_session=True, close_fds=True)
    except BlockingIOError:
        lock.close()
    required = set(config["services"])
    for _ in range(60):
        try:
            status = read(root / "services-status.json")
            if status["identity"] != config["identity"] or status["digest"] != digest(config):
                raise RuntimeError("foreign or incompatible manager; stop/upgrade explicitly while idle")
            if time.time() - status["sampledAt"] < 4 and required.issubset(status["services"]):
                print(json.dumps(status))
                return
        except FileNotFoundError:
            pass
        time.sleep(0.25)
    raise RuntimeError("services did not become running")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["ensure", "serve", "run"])
    parser.add_argument("config")
    parser.add_argument("service", nargs="?")
    args = parser.parse_args()
    if args.mode == "run":
        run_service(args.config, args.service)
    elif args.mode == "serve":
        serve(args.config)
    else:
        ensure(args.config)
