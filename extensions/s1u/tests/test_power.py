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


def test_the_codes_come_from_rostopic_on_the_host_and_the_buttons_from_rosservice(tmp_path, monkeypatch):
    ran = []
    service = power.Power("192.168.0.10", run=lambda command, **options: ran.append(command) or done(
        "success: True\nmessage: \"ok\"\n"))
    for line in ("data: \"12000000,13000000\"\n", "---\n"):
        service.receive(line)
    assert service.state()["ready"]
    answer = service.send("driver_off")
    assert answer == dict(sent="/astribot_system_monitor/driver_command", value=False, success=True, message="ok")
    assert ran[0][:4] == ["nsenter", "--target", "1", "--mount"] and "ROS_MASTER_URI=http://192.168.0.10:11311" in ran[0][-1]
    assert ran[0][-1].endswith("rosservice call /astribot_system_monitor/driver_command 'data: false'")
    service.codes_at = time.monotonic() - 2  # stale: the monitor is down
    assert service.state()["monitor"] is False
    service.run = lambda command, **options: done("", 1, "ERROR: Unable to communicate with master!")
    service.receive("data: \"12000000\"\n")
    status, body = service.act("power_off")
    assert status == 503 and "Unable to communicate" in body["detail"]


def test_force_needs_a_reason_and_in_use_refuses_power_off(tmp_path, monkeypatch):
    monkeypatch.setattr(power, "IN_USE", tmp_path)
    service = power.Power("r", run=lambda command, **options: done("success: True\n"))
    service.receive("data: \"12000000\"\n")
    (tmp_path / "fleet-station").write_text("run 20261114\n")
    assert service.act("power_off") == (409, dict(reason="in_use", state=service.state(), in_use="fleet-station: run 20261114"))
    assert service.act("power_off", force=True)[0] == 400  # force without a reason
    status, body = service.act("power_off", force=True, reason="robot must be moved")
    assert status == 200 and body["sent"].endswith("power_command") and service.powered_off_at is not None
    assert service.act("power_on")[1]["reason"] == "power_off_wait"


def test_bring_up_powers_waits_and_starts_the_driver(monkeypatch):
    codes = [["12000000"]]
    sent = []

    def run(command, **options):
        sent.append(command[-1].split("rosservice call ")[1])
        if "power_command" in command[-1]:
            codes[0] = ["12000000"]
        if "driver_command" in command[-1]:
            codes[0] = ["12000000", "13000000"]
        return done("success: True\n")

    service = power.Power("r", run=run)
    service.receive("data: \"\"\n")  # the monitor publishes nothing powered

    def sleep(seconds):
        service.receive("data: \"%s\"\n" % ",".join(codes[0]))

    monkeypatch.setattr(power.time, "monotonic", __import__("itertools").count(0.0, 0.1).__next__)  # 0.1 s a look
    status, body = service.act("bring_up", sleep=sleep)
    assert status == 200 and body["state"]["ready"]
    assert sent == ["/astribot_system_monitor/power_command 'data: true'", "/astribot_system_monitor/driver_command 'data: true'"]


def test_the_service_answers_on_its_unix_socket_only(tmp_path):
    service = power.Power("r", run=lambda command, **options: done("success: True\n"))
    service.receive("data: \"12000000,13000000\"\n")
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
