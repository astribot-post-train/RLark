"""Exercise process receipt recovery with isolated CPU-only test services."""
import importlib.util
import hashlib
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
from gateway_reuse_probe import verify as verify_gateway

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


class GatewayReuse(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='rlark-gateway-test-')
        self.root = Path(self.temporary.name)
        self.uid = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
        self.instance = 'b' * 64
        self.package_digest = 'c' * 64
        self.identity = self.uid + '/' + self.instance + '/' + self.package_digest
        self.install_root = self.root / 'install'
        self.state_root = self.root / 'state'
        self.package = self.install_root / self.uid / self.package_digest
        self.state = self.state_root / self.uid / 'services'
        self.proc = self.root / 'proc'
        for directory in (self.package, self.state, self.proc):
            directory.mkdir(parents=True, mode=0o700)
        self.package.parent.chmod(0o700)
        self.config = {'identity': self.identity, 'instanceID': self.instance,
                       'privateAddress': '10.1.2.3', 'stateDirectory': str(self.state),
                       'services': {'agent': ['start-agent'], 'gateway': ['gateway'], 'task-manager': ['start-supervisor']}}
        self.digest = hashlib.sha256(json.dumps(self.config, sort_keys=True).encode()).hexdigest()
        self.write(self.package / 'services.json', self.config)
        binaries = {'agent': 'container-runtime-agent', 'gateway': 'container-runtime-gateway', 'task-manager': 'raw-supervisor'}
        self.processes = {'identity': self.identity, 'digest': self.digest, 'processes': {}}
        for index, (name, binary) in enumerate(binaries.items(), 1):
            executable = self.package / binary
            executable.write_bytes(name.encode())
            pid = index + 100
            started = str(index + 200)
            directory = self.proc / str(pid)
            directory.mkdir()
            (directory / 'stat').write_text(str(pid) + ' (' + name + ') ' + ' '.join(['S'] + ['0'] * 18 + [started]))
            (directory / 'exe').symlink_to(executable)
            receipt = {'identity': self.identity, 'digest': self.digest, 'pid': pid, 'started': started}
            self.write(self.state / (name + '.pid.json'), receipt)
            self.processes['processes'][name] = {'pid': pid, 'started': started}
        self.write(self.state / 'processes.json', self.processes)
        self.status = {'identity': self.identity, 'digest': self.digest, 'sampledAt': 100, 'services': list(binaries)}
        self.write(self.state / 'services-status.json', self.status)
        self.agent_digest = hashlib.sha256(b'agent').hexdigest()

    def tearDown(self):
        self.temporary.cleanup()

    def write(self, path, value):
        path.write_text(json.dumps(value))
        path.chmod(0o600)

    def check(self):
        return verify_gateway(str(self.install_root), str(self.state_root), self.uid, self.instance,
                              '10.1.2.3', self.agent_digest, proc=self.proc, now=101)

    def test_exact_receipts_and_digest(self):
        self.assertEqual(self.check()['agentSHA256'], self.agent_digest)
        self.agent_digest = 'd' * 64
        with self.assertRaisesRegex(RuntimeError, 'release mismatch'):
            self.check()

    def test_stale_status_then_recovery(self):
        self.status['sampledAt'] = 1
        self.write(self.state / 'services-status.json', self.status)
        with self.assertRaisesRegex(RuntimeError, 'not observed'):
            self.check()
        self.status['sampledAt'] = 100
        self.write(self.state / 'services-status.json', self.status)
        self.check()

    def test_replaced_process_and_foreign_receipt(self):
        self.processes['processes']['gateway']['started'] = 'wrong'
        self.write(self.state / 'processes.json', self.processes)
        with self.assertRaisesRegex(RuntimeError, 'receipt mismatch'):
            self.check()
        self.processes['identity'] = 'foreign'
        self.write(self.state / 'processes.json', self.processes)
        with self.assertRaisesRegex(RuntimeError, 'foreign gateway'):
            self.check()

    def test_rejects_untrusted_permissions_and_replaced_executable(self):
        (self.state / 'agent.pid.json').chmod(0o644)
        with self.assertRaisesRegex(RuntimeError, 'private service receipt'):
            self.check()
        (self.state / 'agent.pid.json').chmod(0o600)
        executable = self.proc / '102' / 'exe'
        executable.unlink()
        executable.symlink_to(self.package / 'raw-supervisor')
        with self.assertRaisesRegex(RuntimeError, 'executable mismatch'):
            self.check()


if __name__ == "__main__":
    unittest.main()
