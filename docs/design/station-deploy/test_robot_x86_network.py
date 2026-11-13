import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('robot_network', Path(__file__).with_name('robot-x86-network.py'))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
CFG = {'external': '192.168.31.251', 'lan': '192.168.31.0/24', 'x86_internal': '192.168.0.10', 'orin_internal': '192.168.0.11'}


class NetworkTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.patchers = [mock.patch.object(m, 'STATE', Path(self.tmp.name) / 'state.json'), mock.patch.object(m, 'SYSCTL', Path(self.tmp.name) / 'sysctl.conf'), mock.patch.object(m.os, 'geteuid', return_value=0), mock.patch.object(m, 'run', side_effect=self.run_command)]
        for patcher in self.patchers:
            patcher.start()
            self.addCleanup(patcher.stop)
        self.props = {'ipv4.addresses': '192.168.0.10/24', 'ipv4.routes': ''}
        self.fail_after_modify = False
        self.existing_route = False

    def run_command(self, *args, **kwargs):
        if args[:4] == ('ip', '-j', 'route', 'get'):
            return '[{"dev":"enp3s0"}]'
        if args[:4] == ('ip', '-j', 'route', 'show'):
            return '[{}]' if self.existing_route else '[]'
        if args[:4] == ('ip', '-j', 'address', 'show'):
            return '[]'
        if args[:2] == ('nmcli', '-g'):
            key = args[2]
            return {'GENERAL.CONNECTION': 'wired', 'GENERAL.CON-UUID': 'uuid', **self.props}[key]
        if args[:3] == ('nmcli', 'connection', 'modify'):
            key, value = args[-2:]
            if key.startswith('+'):
                key = key[1:]
                self.props[key] = (self.props[key] + ', ' if self.props[key] else '') + value
                if self.fail_after_modify:
                    self.fail_after_modify = False
                    raise RuntimeError('lost response')
            else:
                self.props[key] = value
            return ''
        if args[:3] == ('nmcli', 'device', 'reapply'):
            return ''
        raise AssertionError(args)

    def test_repeat_apply_and_restore(self):
        m.remote('x86', 'apply', CFG)
        first = dict(self.props)
        m.remote('x86', 'apply', CFG)
        self.assertEqual(first, self.props)
        m.remote('x86', 'restore', CFG)
        self.assertEqual(self.props, {'ipv4.addresses': '192.168.0.10/24', 'ipv4.routes': ''})
        self.assertFalse(m.STATE.exists())

    def test_interrupted_write_recovers(self):
        self.fail_after_modify = True
        with self.assertRaisesRegex(RuntimeError, 'lost response'):
            m.remote('x86', 'apply', CFG)
        m.remote('x86', 'apply', CFG)
        self.assertIn('192.168.31.251/32', self.props['ipv4.addresses'])
        self.assertEqual(self.props['ipv4.routes'].count('192.168.31.0/24'), 1)

    def test_existing_route_and_external_edits_are_not_adopted(self):
        self.existing_route = True
        with self.assertRaisesRegex(RuntimeError, 'route already exists'):
            m.remote('x86', 'apply', CFG)
        self.assertFalse(m.STATE.exists())
        self.existing_route = False
        m.remote('x86', 'apply', CFG)
        self.props['ipv4.routes'] += ', 10.0.0.0/8 192.168.0.1'
        with self.assertRaisesRegex(RuntimeError, 'profile changed'):
            m.remote('x86', 'restore', CFG)
        self.assertTrue(m.STATE.exists())


if __name__ == '__main__':
    unittest.main()
