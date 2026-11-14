"""The S1U's power service (extensions/s1u/device/power.py): the vendor page's rules, the robot in use, the vendor's
ROS on the host, the Unix socket."""
import http.client
import json
import socket
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "device"))
import power  # noqa: E402


def done(stdout="", returncode=0, stderr=""):
    return type("R", (), {"returncode": returncode, "stdout": stdout, "stderr": stderr})()


def test_the_state_and_the_vendor_pages_refusals():
    assert power.robot_state(None)["monitor"] is False
    on = power.robot_state(["12000000", "13000000"])
    assert on["powered"] and on["driver"] and on["ready"] and on["power_level"] == 0
    stop = power.robot_state(["12200100", "12000001"])
    assert stop["emergency_stop"] and stop["errors"] == ["12200100"] and not stop["driver"]
    assert power.robot_state(["12000000"], powered_off_s=3.0)["power_wait_s"] == 7.0
    assert power.refusal("power_on", power.robot_state(None)) == "vendor_monitor_down"
    assert power.refusal("power_on", power.robot_state(["12000000"], 3.0)) == "power_off_wait"
    assert power.refusal("power_off", on) == "driver_running" and power.refusal("power_off", on, force=True) is None
    assert power.refusal("driver_on", power.robot_state([])) == "not_powered"
    # In use (any mark): power off and restart wait, unless forced; the others do not.
    assert power.refusal("restart", on, busy="fleet-station: run") == "in_use"
    assert power.refusal("power_off", on, force=True, busy="fleet-station: run") is None
    assert power.refusal("driver_on", on, busy="fleet-station: run") is None


def test_the_codes_are_the_monitors_and_a_button_is_one_service_call():
    called = []
    service = power.Power(lambda name, value: called.append((name, value)) or (True, "ok"))
    service.receive("12000000,13000000")
    assert service.state()["ready"]
    answer = service.send("driver_off")
    assert answer == dict(sent="/astribot_system_monitor/driver_command", value=False, success=True, message="ok")
    assert called == [("/astribot_system_monitor/driver_command", False)]
    service.codes_at = time.monotonic() - 2  # stale: the monitor is down
    assert service.state()["monitor"] is False

    def failing(name, value):
        raise RuntimeError("service [/astribot_system_monitor/power_command] unavailable")

    service.call = failing
    service.receive("12000000")
    status, body = service.act("power_off")
    assert status == 503 and "unavailable" in body["detail"]


def test_force_needs_a_reason_and_in_use_refuses_power_off(tmp_path, monkeypatch):
    monkeypatch.setattr(power, "IN_USE", tmp_path)
    service = power.Power(lambda name, value: (True, ""))
    service.receive("12000000")
    (tmp_path / "fleet-station").write_text("run 20261114\n")
    assert service.act("power_off") == (409, dict(reason="in_use", state=service.state(), in_use="fleet-station: run 20261114"))
    assert service.act("power_off", force=True)[0] == 400  # force without a reason
    status, body = service.act("power_off", force=True, reason="robot must be moved")
    assert status == 200 and body["sent"].endswith("power_command") and service.powered_off_at is not None
    assert service.act("power_on")[1]["reason"] == "power_off_wait"


def test_bring_up_powers_waits_and_starts_the_driver(monkeypatch):
    codes = [["12000000"]]
    sent = []

    def call(name, value):
        sent.append((name, value))
        codes[0] = ["12000000", "13000000"] if "driver_command" in name else ["12000000"]
        return True, ""

    service = power.Power(call)
    service.receive("")  # the monitor publishes nothing powered

    def sleep(seconds):
        service.receive(",".join(codes[0]))

    monkeypatch.setattr(power.time, "monotonic", __import__("itertools").count(0.0, 0.1).__next__)  # 0.1 s a look
    status, body = service.act("bring_up", sleep=sleep)
    assert status == 200 and body["state"]["ready"]
    assert sent == [("/astribot_system_monitor/power_command", True), ("/astribot_system_monitor/driver_command", True)]


def test_the_service_answers_on_its_unix_socket_only(tmp_path):
    service = power.Power(lambda name, value: (True, ""))
    service.receive("12000000,13000000")
    path = tmp_path / "s1u-power.sock"
    server = power.Server(str(path), power.handler(service))
    threading.Thread(target=server.serve_forever, daemon=True).start()

    class Connection(http.client.HTTPConnection):
        def connect(self):
            self.sock = socket.socket(socket.AF_UNIX)
            self.sock.connect(str(path))

    def call(method, url, body=None):
        connection = Connection("localhost")
        connection.request(method, url, body=json.dumps(body) if body else None,
                           headers={"X-Remote-User": "operator-1", "Content-Type": "application/json"})
        response = connection.getresponse()
        return response.status, json.loads(response.read())

    assert call("GET", "/power")[1]["ready"]
    assert call("POST", "/power", {"action": "fly"})[0] == 400
    status, body = call("POST", "/power", {"action": "driver_off"})
    assert status == 200 and body["success"]
    server.shutdown()
