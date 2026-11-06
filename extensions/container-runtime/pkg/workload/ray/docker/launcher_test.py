"""Process-boundary tests for the embedded launcher; no real daemon required."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

LAUNCHER = Path(__file__).with_name("launcher.py")


class LauncherProcessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="rlark-launch-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.members = ["10.0.0.12", "10.0.0.13"]
        self.members_file = self.root / "members.json"
        self.members_file.write_text(json.dumps(self.members))
        modules = {
            "grpc.py": '''class RpcError(Exception): pass
class Channel:
    def close(self): pass
def insecure_channel(*args, **kwargs): return Channel()
''',
            "ray/__init__.py": '__version__ = "2.56.1"\n',
            "ray/core/__init__.py": "",
            "ray/core/generated/__init__.py": "",
            "ray/core/generated/gcs_pb2.py": "class GcsNodeInfo: ALIVE = 0\n",
            "ray/core/generated/gcs_service_pb2.py": "class GetAllNodeInfoRequest: pass\n",
            "ray/core/generated/gcs_service_pb2_grpc.py": '''import json, os
from types import SimpleNamespace as NS
class NodeInfoGcsServiceStub:
    def __init__(self, channel): pass
    def GetAllNodeInfo(self, request, timeout):
        addresses = json.load(open(os.environ["TEST_MEMBERS"]))
        return NS(status=NS(code=0), node_info_list=[NS(node_manager_address=a, state=0) for a in addresses])
''',
        }
        for name, contents in modules.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(contents)
        # A real child process represents ray start --block at the argv/process
        # boundary. It writes identity before either waiting or exiting.
        self.ray = self.root / "bin" / "ray"
        self.ray.parent.mkdir(parents=True, exist_ok=True)
        self.ray.write_text(
            "#!" + sys.executable + "\n"
            "import json,os,sys,time\n"
            "from pathlib import Path\n"
            "Path(os.environ['TEST_RAY_PID']).write_text(str(os.getpid()))\n"
            "if os.environ.get('TEST_RAY_EXIT') is not None: sys.exit(int(os.environ['TEST_RAY_EXIT']))\n"
            "while True: time.sleep(0.05)\n"
        )
        self.ray.chmod(0o700)
        cli = self.root / "ray" / "scripts" / "scripts.py"
        cli.parent.mkdir(parents=True)
        cli.with_name("__init__.py").touch()
        cli.write_text("if __name__ == '__main__':\n" + "\n".join("    " + line for line in self.ray.read_text().splitlines()[1:]) + "\n")
        self.env = dict(os.environ, PYTHONPATH=str(self.root),
                        PATH=str(self.ray.parent) + os.pathsep + os.environ.get("PATH", ""),
                        TEST_MEMBERS=str(self.members_file), TEST_RAY_PID=str(self.root / "ray.pid"))
        self.config = dict(head=True, start=[str(self.ray), "start", "--block"],
                           address="10.0.0.12:24000", nodeAddress="10.0.0.12",
                           members=self.members, command=[sys.executable, "-c", "print('trained')"],
                           startupSeconds=2)

    def launch(self):
        proc = subprocess.Popen([sys.executable, "-u", str(LAUNCHER), json.dumps(self.config)],
                                env=self.env, cwd=self.root, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True)
        def ensure_stopped():
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.communicate(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.communicate(timeout=2)
        self.addCleanup(ensure_stopped)
        return proc

    def wait_for_ray(self):
        deadline = time.monotonic() + 5
        while not (self.root / "ray.pid").exists():
            self.assertLess(time.monotonic(), deadline, "ray child did not start")
            time.sleep(0.01)

    def assert_ray_stopped(self):
        pid_file = self.root / "ray.pid"
        if pid_file.exists():
            with self.assertRaises(ProcessLookupError):
                os.kill(int(pid_file.read_text()), 0)

    def test_head_preserves_literal_argv_and_cleans_up(self):
        expected = ["space ' quote", "$(touch injected)", "`touch injected`", "$HOME", ""]
        self.config["command"] = [sys.executable, "-c", "import json,sys; print(json.dumps(sys.argv[1:]))", *expected]
        proc = self.launch()
        out, err = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 0, err)
        self.assertEqual(json.loads(out.splitlines()[-1]), expected)
        self.assertFalse((self.root / "injected").exists())
        self.assert_ray_stopped()

    def test_training_waits_for_exact_membership(self):
        self.members_file.write_text(json.dumps(self.members[:1]))
        marker = self.root / "trained"
        self.config["command"] = [sys.executable, "-c", "from pathlib import Path; Path('trained').touch()"]
        proc = self.launch()
        self.wait_for_ray()
        time.sleep(0.2)
        self.assertIsNone(proc.poll())
        self.assertFalse(marker.exists())
        self.members_file.write_text(json.dumps(self.members))
        _, err = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 0, err)
        self.assertTrue(marker.exists())
        self.assert_ray_stopped()

    def test_foreign_and_duplicate_members_do_not_train(self):
        for members in ([*self.members, "10.0.0.99"], [self.members[0], self.members[0]]):
            self.members_file.write_text(json.dumps(members))
            proc = self.launch()
            out, _ = proc.communicate(timeout=10)
            self.assertEqual(proc.returncode, 1)
            self.assertNotIn("trained", out)
            self.assert_ray_stopped()

    def test_missing_members_timeout_and_cleanup(self):
        self.members_file.write_text("[]")
        self.config["startupSeconds"] = 0.2
        proc = self.launch()
        out, _ = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 1)
        self.assertNotIn("trained", out)
        self.assert_ray_stopped()

    def test_prepare_and_training_exit_codes(self):
        self.config["prepare"] = "exit 23"
        proc = self.launch()
        out, _ = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 23)
        self.assertNotIn("trained", out)
        self.assertFalse((self.root / "ray.pid").exists())
        self.config["prepare"] = "printf prepared"
        self.config["run"] = "exit 19"
        proc = self.launch()
        _, _ = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 19)
        self.assert_ray_stopped()

    def test_worker_zero_exit_is_failure(self):
        self.config["head"] = False
        self.config["command"] = []
        self.env["TEST_RAY_EXIT"] = "0"
        proc = self.launch()
        _, _ = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 1)
        self.assert_ray_stopped()

    def test_sigterm_during_readiness_stops_ray(self):
        self.members_file.write_text("[]")
        proc = self.launch()
        self.wait_for_ray()
        proc.send_signal(signal.SIGTERM)
        out, _ = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 143)
        self.assertNotIn("trained", out)
        self.assert_ray_stopped()

    def test_worker_stays_alive_after_readiness(self):
        self.config["head"] = False
        self.config["command"] = []
        proc = self.launch()
        self.assertIn("membership ready", proc.stdout.readline())
        self.assertIsNone(proc.poll())
        proc.terminate()
        proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 143)
        self.assert_ray_stopped()

    def test_ray_exit_while_training_fails_and_stops_training(self):
        self.config["command"] = [sys.executable, "-c", "import time; time.sleep(60)"]
        proc = self.launch()
        self.assertIn("membership ready", proc.stdout.readline())
        self.wait_for_ray()
        os.kill(int((self.root / "ray.pid").read_text()), signal.SIGTERM)
        proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 1)
        self.assert_ray_stopped()

    def test_wrong_ray_version_does_not_start_or_log_configuration(self):
        (self.root / "ray/__init__.py").write_text('__version__ = "wrong"\n')
        self.config["prepare"] = "echo secret-value"
        proc = self.launch()
        out, err = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 1)
        self.assertNotIn("secret-value", out + err)
        self.assertFalse((self.root / "ray.pid").exists())


if __name__ == "__main__":
    unittest.main()
