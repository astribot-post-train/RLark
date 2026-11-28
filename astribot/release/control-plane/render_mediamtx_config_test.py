import importlib.util
import json
import stat
import tempfile
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("render_mediamtx_config.py")
SPEC = importlib.util.spec_from_file_location("render_mediamtx_config", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class RenderMediaMTXConfigTest(unittest.TestCase):
    def setUp(self):
        self.settings = {
            "MEDIAMTX_STATION_SOURCE_CIDR": "192.0.2.16/28",
            "MEDIAMTX_PILOT_AGENT": "pilot-agent",
            "MEDIAMTX_PILOT_NODE": "pilot-node",
            "MEDIAMTX_ICE_ADDITIONAL_HOST": "media.example.invalid",
        }
        self.credentials = {
            "publishUser": "publisher",
            "publishPassword": "publish-test-only",
            "readerUser": "reader",
            "readerPassword": "reader-test-only",
            "controlUser": "controller",
            "controlPassword": "control-test-only",
        }

    def test_render_materializes_cidr_paths_and_role_permissions(self):
        result = MODULE.render(self.settings, self.credentials)
        self.assertIn('ips: ["192.0.2.16/28"]', result)
        self.assertIn('path: "pilot-agent/pilot-node/head"', result)
        self.assertIn('path: "pilot-agent/pilot-node/wrist_right"', result)
        self.assertIn('webrtcAdditionalHosts: ["media.example.invalid"]', result)
        self.assertIn('pass: "publish-test-only"', result)
        self.assertEqual(result.count("action: publish"), 4)
        self.assertEqual(result.count("action: read"), 4)
        self.assertEqual(result.count("action: api"), 1)

    def test_rejects_invalid_cidr_and_shared_roles(self):
        invalid_settings = {**self.settings, "MEDIAMTX_STATION_SOURCE_CIDR": "not-a-cidr"}
        with self.assertRaises(ValueError):
            MODULE.render(invalid_settings, self.credentials)
        shared_user = {**self.credentials, "controlUser": "reader"}
        with self.assertRaises(ValueError):
            MODULE.render(self.settings, shared_user)

    def test_protected_json_rejects_permissive_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "credentials.json"
            path.write_text(json.dumps(self.credentials), encoding="utf-8")
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                MODULE.read_protected_json(path)

    def test_output_is_atomic_and_mode_0600(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "nested" / "mediamtx.yml"
            MODULE.write_private_file(path, "test\n")
            self.assertEqual(path.read_text(encoding="utf-8"), "test\n")
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)


if __name__ == "__main__":
    unittest.main()
