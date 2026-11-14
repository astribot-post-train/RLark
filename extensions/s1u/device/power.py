"""The S1U's power, driver start and restart without the vendor's page (user 11-12): the three buttons of Astribot's
control centre (电源 / 启动 / 重启) as a service of the Addon s1u-robot on every S1U robot x86, for RLark's API.

It does what the vendor's web server does behind those buttons, nothing else: one std_srvs/SetBool call to the
vendor's system monitor, which runs the vendor's own scripts.

    电源  /astribot_system_monitor/power_command   True: joint motors on, False: off
    启动  /astribot_system_monitor/driver_command  True: astribot_driver and its activation, False: stop it
    重启  /astribot_system_monitor/reboot_command  True: motors off, the Orin restarted, then this x86

The vendor's services answer at once, before anything has happened, and check nothing; the checks are in the vendor's
page, and so here (refusal): the driver starts only on powered motors, the motors go off under a running driver only
when forced, and come on again no sooner than 10 s after they went off. Power off and restart also wait while the robot
is in use (any mark under /run/rlark/in-use), unless forced with a reason. The state is the vendor's: its system
monitor publishes every component's status codes (/astribot_error_code/multi_error_code, 30 Hz); a code's digits are
module (12 power and hardware, 13 driver), level (0 normal, 1 warning, 2 error) and part.

It speaks ROS itself (rospy, the vendor's noetic) to the robot's master at S1U_ROS_IP_ADDRESS, as a node at that address
(the container is on the host's network). It listens on a Unix socket only (S1U_SERVICE_SOCKET); RLark reaches it on
the node and decides who may press, and records who did.

    GET  /power   the state: monitor, powered, driver, emergency_stop, ready, errors, power_level, driver_level, power_wait_s
    POST /power   {"action": power_on | power_off | driver_on | driver_off | restart | bring_up, "force": false,
                   "reason": "..."}: 200 {sent, value, success, message, state}; 409 {reason, state} when refused;
                   400 when force comes without a reason; 503 when the vendor's service failed; 504 when bring_up did
                   not end ready
S1U_AUTO_DRIVER=1 starts the driver whenever the motors come on; S1U_AUTO_POWER=1 also powers the motors when the
service starts (energising a robot nobody stands at is the site's decision: off by default).
"""
import json
import os
import socketserver
import threading
import time
from http.server import BaseHTTPRequestHandler
from pathlib import Path

SERVICES = {"power_on": ("power_command", True), "power_off": ("power_command", False),
            "driver_on": ("driver_command", True), "driver_off": ("driver_command", False),
            "restart": ("reboot_command", True)}
EMERGENCY_STOP, DRIVER_NORMAL = "12200100", "13000000"
POWER_OFF_SECONDS = 10.0  # the vendor's page: "关节电机下电后需等待10s后方可上电"
CODES_SECONDS = 1.5  # a status code is its component's this long after its last publication (vendor: 0.8 s)
TOPIC = "/astribot_error_code/multi_error_code"
IN_USE = Path("/in-use")  # the host's /run/rlark/in-use
BRING_UP_SECONDS = 60.0


def robot_state(codes, powered_off_s=None):
    """The robot as the vendor's page reads it from the status ``codes`` (None: its monitor is not publishing).
    ``powered_off_s``: seconds since this service powered the motors off."""
    if codes is None:
        return dict(monitor=False, powered=False, driver=False, emergency_stop=False, ready=False, errors=[],
                    power_level=None, driver_level=None, power_wait_s=0.0)

    def level(module):
        return max((int(code[2]) for code in codes if code.startswith(module)), default=None)

    wait = 0.0 if powered_off_s is None else max(0.0, POWER_OFF_SECONDS - powered_off_s)
    errors = sorted(code for code in codes if code[2] == "2")
    return dict(monitor=True, powered=level("12") is not None, driver=level("13") is not None,
                emergency_stop=EMERGENCY_STOP in codes, ready=DRIVER_NORMAL in codes and not errors, errors=errors,
                power_level=level("12"), driver_level=level("13"), power_wait_s=round(wait, 1))


def in_use():
    """Why the robot is in use now (each component's mark under /run/rlark/in-use), or ""."""
    marks = sorted(IN_USE.glob("*")) if IN_USE.is_dir() else []
    return "; ".join("%s: %s" % (path.name, path.read_text().strip()[:200]) for path in marks)


def refusal(action, state, force=False, busy=""):
    """Why ``action`` is not sent now, or None: the vendor page's conditions, and the robot in use."""
    if not state["monitor"]:
        return "vendor_monitor_down"
    if action == "power_on" and state["power_wait_s"] > 0:
        return "power_off_wait"
    if action in ("power_off", "restart") and busy and not force:
        return "in_use"
    if action == "power_off" and state["driver"] and not force:
        return "driver_running"  # the page asks to stop the driver first, and offers 强制下电
    if action == "driver_on" and state["power_level"] not in (0, 1):
        return "not_powered"
    return None


class Power:
    """The vendor's status codes and its three services; ``call(service, value)`` answers (success, message)."""

    def __init__(self, call):
        self.call = call
        self.codes, self.codes_at, self.powered_off_at = [], -float("inf"), None
        self.lock = threading.Lock()  # one button at a time

    def receive(self, text):
        """One publication of the status codes (comma-separated)."""
        self.codes, self.codes_at = [code for code in text.split(",") if code], time.monotonic()

    def state(self):
        fresh = time.monotonic() - self.codes_at < CODES_SECONDS
        off = None if self.powered_off_at is None else time.monotonic() - self.powered_off_at
        return robot_state(self.codes if fresh else None, off)

    def send(self, action):
        service, value = SERVICES[action]
        name = "/astribot_system_monitor/" + service
        success, message = self.call(name, value)
        if action == "power_off":
            self.powered_off_at = time.monotonic()
        return dict(sent=name, value=value, success=success, message=message)

    def act(self, action, force=False, reason="", sleep=time.sleep):
        """(HTTP status, body) of one button."""
        if force and not reason.strip():
            return 400, dict(detail="force needs a reason")
        with self.lock:
            state = self.state()
            if action == "bring_up":
                try:
                    return self.bring_up(sleep)
                except Exception as error:
                    return 503, dict(detail="vendor service failed: %s" % error, state=self.state())
            refused = refusal(action, state, force, in_use())
            if refused:
                return 409, dict(reason=refused, state=state, in_use=in_use())
            try:
                return 200, dict(self.send(action), state=self.state())
            except Exception as error:  # the service is gone or failed: the operator reads why
                return 503, dict(detail="vendor service failed: %s" % error, state=state)

    def bring_up(self, sleep):
        """Powered and the driver ready: power on unless powered, the driver once the motors are on (by itself
        within 5 s with S1U_AUTO_DRIVER, else asked for), each step waited for up to BRING_UP_SECONDS."""
        def wait(check, seconds):
            until = time.monotonic() + seconds
            while time.monotonic() < until:
                if check(self.state()):
                    return True
                sleep(0.5)
            return check(self.state())

        state = self.state()
        refused = None if state["powered"] else refusal("power_on", state)
        if refused:
            return 409, dict(reason=refused, state=state)
        if not state["powered"]:
            self.send("power_on")
            if not wait(lambda s: s["powered"], BRING_UP_SECONDS):
                return 504, dict(detail="not powered within %d s" % BRING_UP_SECONDS, state=self.state())
        if not wait(lambda s: s["driver"], 5.0):
            self.send("driver_on")
        if not wait(lambda s: s["ready"] or s["errors"], BRING_UP_SECONDS):
            return 504, dict(detail="not ready within %d s" % BRING_UP_SECONDS, state=self.state())
        state = self.state()
        if state["errors"]:
            return 504, dict(detail="errors: %s" % ",".join(state["errors"]), state=state)
        return 200, dict(sent="bring_up", value=True, success=True, message="ready", state=state)

    def automatic(self, power_at_start):
        """S1U_AUTO_DRIVER (and S1U_AUTO_POWER at start): the driver once whenever the motors come on."""
        if power_at_start:
            while self.act("power_on")[0] != 200:
                time.sleep(2.0)
        powered = False
        while True:
            state = self.state()
            if state["powered"] and not powered and not state["driver"]:
                # Once per power-on, so a driver the operator stopped stays stopped.
                status, answer = self.act("driver_on")
                print("%s driver_on after power-on (S1U_AUTO_DRIVER): %s %s" % (
                    time.strftime("%F %T"), status, answer.get("reason") or answer.get("detail") or "sent"), flush=True)
                if status != 200:
                    state = dict(state, powered=False)
            powered = state["powered"]
            time.sleep(1.0)


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def handler(power):
    class Handler(BaseHTTPRequestHandler):
        def reply(self, status, body):
            data = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            self.reply(*((200, power.state()) if self.path == "/power" else (404, dict(detail="no such path"))))

        def do_POST(self):
            if self.path != "/power":
                return self.reply(404, dict(detail="no such path"))
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
            if body.get("action") not in (*SERVICES, "bring_up"):
                return self.reply(400, dict(detail="unsupported action"))
            status, answer = power.act(body["action"], bool(body.get("force")), str(body.get("reason") or ""))
            # Every press and what became of it; who pressed is RLark's record (the header it sets).
            print("%s %s force=%s reason=%r by %s: %s %s" % (
                time.strftime("%F %T"), body["action"], bool(body.get("force")), body.get("reason") or "",
                self.headers.get("X-Remote-User", "?"), status, answer.get("reason") or answer.get("detail") or "sent"),
                flush=True)
            self.reply(status, answer)

        def address_string(self):
            return "unix"

        def log_message(self, *arguments):
            pass

    return Handler


def main():
    lines = (line.split("=", 1) for line in open(os.environ["S1U_NODE_PROFILE"]).read().splitlines()
             if "=" in line and line[0] != "#")
    profile = {key: value.strip('"') for key, value in lines}
    os.environ.update(ROS_MASTER_URI="http://%s:11311" % profile["S1U_ROS_IP_ADDRESS"], ROS_IP=profile["S1U_ROS_IP_ADDRESS"])
    import rospy
    from std_msgs.msg import String
    from std_srvs.srv import SetBool

    def call(name, value):
        rospy.wait_for_service(name, timeout=2.0)
        reply = rospy.ServiceProxy(name, SetBool)(value)
        return reply.success, reply.message

    rospy.init_node("s1u_power", disable_signals=True)
    power = Power(call)
    rospy.Subscriber(TOPIC, String, lambda message: power.receive(message.data), queue_size=1)
    if os.environ.get("S1U_AUTO_DRIVER") == "1":
        threading.Thread(target=power.automatic, args=(os.environ.get("S1U_AUTO_POWER") == "1",), daemon=True).start()
    path = Path(os.environ["S1U_SERVICE_SOCKET"])
    path.parent.mkdir(parents=True, exist_ok=True)
    path.unlink(missing_ok=True)
    server = Server(str(path), handler(power))
    os.chmod(path, 0o600)
    print("power of this robot on %s" % path, flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
