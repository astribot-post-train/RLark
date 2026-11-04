"""Exercise process receipt recovery with isolated CPU-only test services."""
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

from notebook_identity import instance_id, validate_instance

spec = importlib.util.spec_from_file_location("node_services", Path(__file__).with_name("node_services.py"))
manager = importlib.util.module_from_spec(spec)
spec.loader.exec_module(manager)


class ServiceRecovery(unittest.TestCase):
    def test_runtime_directories_and_workload_preflight(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = {'stateDirectory': str(root / 'state/services'),
                      'runtimeDirectories': [str(root / 'state/tasks'), str(root / 'state/tasks-agent-upgrades')]}
            path = root / 'services.json'
            executable = str(Path(sys.executable).resolve())
            supervisor = root / 'supervisor.json'
            supervisor.write_text(json.dumps({'executables': [executable], 'workDirectories': [str(root)]}))
            manager.prepare_runtime(path, config)
            manager.prepare_runtime(path, config)
            for value in config['runtimeDirectories']:
                self.assertEqual(Path(value).stat().st_mode & 0o777, 0o700)
            link = root / 'python'
            link.symlink_to(executable)
            supervisor.write_text(json.dumps({'executables': [str(link)], 'workDirectories': [str(root)]}))
            with self.assertRaisesRegex(RuntimeError, 'canonical'):
                manager.prepare_runtime(path, config)
            supervisor.unlink()
            config['runtimeDirectories'] = [str(root / 'outside')]
            with self.assertRaisesRegex(RuntimeError, 'outside'):
                manager.prepare_runtime(path, config)
            self.assertFalse((root / 'outside').exists())
            private = root / 'state/tasks'
            private.chmod(0o755)
            config['runtimeDirectories'] = [str(private)]
            with self.assertRaisesRegex(RuntimeError, 'private'):
                manager.prepare_runtime(path, config)
            outside = root / 'foreign'
            outside.mkdir()
            linked = root / 'linked'
            linked.symlink_to(outside, target_is_directory=True)
            config = {'stateDirectory': str(linked / 'services'),
                      'runtimeDirectories': [str(linked / 'tasks')]}
            with self.assertRaisesRegex(RuntimeError, 'canonical'):
                manager.prepare_runtime(path, config)
            self.assertFalse((outside / 'tasks').exists(), 'preflight mutated a symlink target')

    def test_manager_restart_retains_exact_service_and_rejects_foreign_state(self):
        with tempfile.TemporaryDirectory(prefix="rlark-services-test-") as directory:
            root = Path(directory)
            config = root / "services.json"
            state = root / "state"
            config.write_text(json.dumps({"identity": "test-instance", "stateDirectory": str(state),
                "services": {"agent": [sys.executable, "-c", "import time; time.sleep(120)"]}}))
            child = None
            process = None
            try:
                process = self.start(config)
                first = self.wait_ready(state)
                receipt = json.loads((state / "agent.pid.json").read_text())
                child = manager.RetainedProcess(receipt)
                self.assertIsNone(child.poll())
                process.terminate()
                process.wait(timeout=10)
                self.assertIsNone(child.poll(), "manager stop killed retained service")
                process = self.start(config)
                self.wait_ready(state)
                self.assertEqual(receipt["pid"], json.loads((state / "agent.pid.json").read_text())["pid"])
                self.assertEqual(first["identity"], "test-instance")
                process.terminate()
                process.wait(timeout=10)
                retained = json.loads((state / "processes.json").read_text())
                retained["identity"] = "foreign"
                (state / "processes.json").write_text(json.dumps(retained))
                process = self.start(config)
                self.assertNotEqual(process.wait(timeout=10), 0)
                self.assertIsNone(child.poll(), "foreign manager touched existing service")
            finally:
                if process and process.poll() is None:
                    process.terminate()
                    process.wait(timeout=10)
                if child and child.poll() is None:
                    child.terminate()
                    try:
                        child.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        child.kill()

    def test_start_time_mismatch_is_not_a_live_service(self):
        retained = manager.RetainedProcess({"pid": os.getpid(), "started": "impossible-start-time"})
        self.assertIsNotNone(retained.poll())

    def test_replaced_notebook_cannot_start_services_or_adopt_receipts(self):
        for field in ('instanceID', 'privateAddress'):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                current_instance, current_address = 'current-instance', '10.1.2.3'
                config = root / 'services.json'
                data = {'identity': 'test-instance', 'instanceID': current_instance,
                        'privateAddress': current_address, 'stateDirectory': str(root / 'state'),
                        'services': {'agent': [sys.executable, '-c',
                                              f"open({str(root / 'started')!r}, 'w').close()"]}}
                data[field] = 'stale'
                config.write_text(json.dumps(data))
                with patch('notebook_identity.instance_id', return_value=current_instance), \
                     patch('notebook_identity.private_address', return_value=current_address):
                    with self.assertRaisesRegex(RuntimeError, 'explicit replacement'):
                        manager.ensure(str(config))
                    with self.assertRaisesRegex(RuntimeError, 'explicit replacement'):
                        manager.run_service(str(config), 'agent')
                process = self.start(config)
                self.assertNotEqual(process.wait(timeout=10), 0)
                self.assertFalse((root / 'started').exists())
                self.assertFalse((root / 'state').exists())

    def test_fingerprint_changes_with_pid_namespace_start_and_boot(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / '1/ns').mkdir(parents=True)
            (root / 'sys/kernel/random').mkdir(parents=True)
            namespace, stat, boot = root / '1/ns/pid', root / '1/stat', root / 'sys/kernel/random/boot_id'
            namespace.symlink_to('pid:[12345]')
            stat.write_text('1 (init with spaces) ' + ' '.join(['0'] * 19 + ['101']))
            boot.write_text('boot-1')
            original = instance_id(root)
            self.assertEqual(instance_id(root), original)
            stat.write_text('1 (init with spaces) ' + ' '.join(['0'] * 19 + ['102']))
            self.assertNotEqual(instance_id(root), original)
            stat.write_text('1 (init with spaces) ' + ' '.join(['0'] * 19 + ['101']))
            boot.write_text('boot-2')
            self.assertNotEqual(instance_id(root), original)
            boot.write_text('boot-1')
            namespace.unlink()
            namespace.symlink_to('pid:[12346]')
            self.assertNotEqual(instance_id(root), original)

    def test_matching_instance_guard_and_probe_failure(self):
        config = {'instanceID': 'current', 'privateAddress': '10.1.2.3'}
        with patch('notebook_identity.instance_id', return_value='current'), \
             patch('notebook_identity.private_address', return_value='10.1.2.3'):
            validate_instance(config)
        with patch('notebook_identity.instance_id', side_effect=PermissionError('denied')):
            with self.assertRaises(PermissionError):
                validate_instance(config)

    def start(self, config):
        return subprocess.Popen([sys.executable, str(Path(__file__).with_name("node_services.py")), "serve", str(config)],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    def wait_ready(self, state):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                status = json.loads((state / "services-status.json").read_text())
                if status["sampledAt"] > 0 and "agent" in status["services"]:
                    return status
            except (FileNotFoundError, json.JSONDecodeError):
                pass
            time.sleep(0.05)
        self.fail("service did not start")


if __name__ == "__main__":
    unittest.main()
