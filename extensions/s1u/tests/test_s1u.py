"""The S1U robot x86's model configuration (extensions/s1u): the node tuner, the node profile, the Orin and the
clock. Run with pytest from any directory: python3 -m pytest extensions/s1u/tests."""
import hashlib
import importlib.util
import io
import json
import re
import subprocess
import sys
import tarfile
from pathlib import Path

HERE = Path(__file__).resolve().parents[1]
TUNER, ORIN, CLOCK = HERE / "tuner", HERE / "orin", HERE / "clock"
CATALOG = HERE.parents[1] / "apps/rlark/pkg/addons/catalog/s1u-robot"
for directory in (TUNER, ORIN, CLOCK):
    sys.path.insert(0, str(directory))  # node_tuner.py imports node_spec.py from beside itself
specification = importlib.util.spec_from_file_location("node_tuner", TUNER / "node_tuner.py")
node_tuner = importlib.util.module_from_spec(specification)
specification.loader.exec_module(node_tuner)
import clock  # noqa: E402
import node_spec  # noqa: E402
import orin  # noqa: E402

PROFILE = dict(line.split("=", 1) for line in (TUNER / "node_profile.env").read_text().splitlines()
               if line[:1].isalpha() and "=" in line)


def shell(function):
    """A function of node_profile.sh with the profile's values, on a 20-CPU host."""
    script = (TUNER / "node_profile.sh").read_text()
    functions = script[script.index("cpus() {"):script.index('case "${1:-}" in')]
    return subprocess.run(["bash", "-c", f"nproc() {{ echo 20; }}\n{functions}\nsource {TUNER}/node_profile.env\n{function}"],
                          check=True, capture_output=True, text=True).stdout.strip()


def done(stdout=b"", returncode=0, stderr=b""):
    return type("R", (), {"returncode": returncode, "stdout": stdout, "stderr": stderr})()


def test_every_cpu_no_pod_may_use_is_reserved_for_the_vendor_and_the_host():
    # Pods: the device layer's 8-9 and 10-15, the others' 16-19.
    assert shell("reserved") == "0-7"
    assert shell('ranges "3,4,5,8,9"') == "3-5,8-9"


def test_the_node_label_says_what_an_operator_or_the_tuner_must_do():
    ok = "ok     governor of every CPU: powersave\nok     isolated CPUs (kernel command line): 3-5,8-9\n"
    assert node_tuner.state(ok) == ("ok", "")
    drift = ok + "DRIFT  governor of every CPU: performance powersave (profile: powersave)\n"
    assert node_tuner.state(drift) == ("drift", "governor of every CPU: performance powersave (profile: powersave)")
    # A rule that only a restart of the host makes hold decides the label, whatever else drifts.
    value, report = node_tuner.state(drift + "REBOOT isolated CPUs (kernel command line): 3-5 (profile: 3-5,8-9)\n")
    assert value == "reboot-required" and "isolated CPUs" in report and "governor" in report
    kubelet = "KUBELET  kubelet's CPU manager: static, started before its configuration (profile: static, started with its configuration)\n"
    assert node_tuner.state(ok + kubelet)[0] == "kubelet-restart-required"
    assert node_tuner.state(ok + kubelet + "REBOOT  containerd-robot's affinity: 0-19 (profile: 16-19)\n")[0] == "reboot-required"


def test_a_restart_that_did_not_happen_is_tried_again_and_one_that_did_not_help_is_not():
    # s1u-02-x86, 11-13: the restart step failed before it called the vendor's service, and was never tried again.
    record, why = node_tuner.restart("host", {}, "p1", "boot-a", 1000.0)
    assert record == dict(kind="host", profile="p1", attempts=1, was="boot-a", at=1000.0) and why is None
    assert node_tuner.restart("host", record, "p1", "boot-a", 1060.0) == (None, "its restart was asked for 60 s ago")
    second, _ = node_tuner.restart("host", dict(record, said="ROS_MASTER_URI: unbound variable"), "p1", "boot-a", 1400.0)
    assert second["attempts"] == 2 and second["at"] == 1400.0
    third, _ = node_tuner.restart("host", second, "p1", "boot-a", 1800.0)
    assert node_tuner.restart("host", third, "p1", "boot-a", 2200.0) == (
        None, "3 attempts did not restart it; the last said: ROS_MASTER_URI: unbound variable")
    assert node_tuner.restart("host", record, "p1", "boot-b", 1400.0)[1].startswith("it was restarted for this profile")
    assert node_tuner.restart("host", third, "p2", "boot-a", 2200.0)[0]["attempts"] == 1
    assert node_tuner.restart("kubelet", third, "p1", "4242", 2200.0)[0] == dict(
        kind="kubelet", profile="p1", attempts=1, was="4242", at=2200.0)
    assert node_tuner.running("host") == open("/proc/sys/kernel/random/boot_id").read().strip()


def test_an_idle_robot_gets_the_restart_its_profile_needs_and_one_in_use_does_not(monkeypatch):
    calls, status, record = [], ["REBOOT isolated CPUs (kernel command line): none\n"], [{}]
    for name, value in (("NODE_NAME", "s1u-03-x86"), ("S1U_NODE_WANTED", "installed"), ("S1U_NODE_REBOOT", "idle"),
                        ("S1U_NODE_ROLLOUT", "all"), ("S1U_SITE_TIME_SOURCE", ""),
                        ("S1U_IMAGE", "registry/rlark/s1u-robot@sha256:1")):
        monkeypatch.setenv(name, value)
    monkeypatch.setattr(node_tuner.shutil, "copy", lambda *paths: None)
    opened = open
    monkeypatch.setattr("builtins.open", lambda path, *a, **k: opened(path, *a, **k) if str(path).startswith(
        (str(TUNER), "/proc")) else io.StringIO())
    published = []
    # What node_profile.sh says: "busy host" counts other components' marks, "busy host own" the robot's own reasons.
    said = {("facts",): "record body serial=XCSU21\nrecord body parameters=0f0f\nmodel os=ubuntu 20.04\n",
            ("reboot",): "ROS_MASTER_URI: unbound variable\n", ("restart-kubelet",): "",
            ("busy", "host"): "", ("busy", "kubelet"): "", ("busy", "host", "own"): ""}
    monkeypatch.setattr(node_tuner, "host", lambda *arguments, quiet=False: calls.append(arguments) or (
        0, said.get(arguments, status[0])))
    monkeypatch.setattr(node_tuner, "publish", lambda *arguments, **annotations: (
        published.append((arguments, annotations)),
        record.__setitem__(0, json.loads(annotations.get(node_tuner.RESTART, json.dumps(record[0]))))))
    monkeypatch.setattr(node_tuner, "node", lambda: ({"rlark.io/node-category": "robot"},
                                                     {node_tuner.RESTART: json.dumps(record[0])}))
    monkeypatch.setattr(node_tuner.sys, "argv", ["node_tuner.py", "--once"])
    kept = []
    monkeypatch.setattr(node_tuner.os, "sched_setaffinity", lambda pid, cpus: kept.append(cpus))
    node_tuner.main()
    assert kept == [{16, 17, 18, 19}]  # the tuner and what it runs on the host: the profile's CPUs for Pods
    # The attempt is on the Node before the restart is asked for, and what the script said after it.
    assert calls[-1] == ("reboot",) and published[-2][0] == (
        "reboot-required", "isolated CPUs (kernel command line): none",
        {"rlark.io/node-conformance": "not-ok", "rlark.io/rollout-state": "acting"})
    assert published[-2][1][node_tuner.SERIAL] == "XCSU21" and "node profile (node-profile): reboot-required" in (
        published[-2][1]["rlark.io/node-conformance-report"])
    assert record[0]["attempts"] == 1 and record[0]["said"] == "ROS_MASTER_URI: unbound variable\n"
    calls.clear()
    node_tuner.main()  # the same boot, at once: not asked for again yet, and the label's report says why
    assert ("reboot",) not in calls and published[-1][0][1].startswith("not restarting host: its restart was asked for")
    status[0] = "KUBELET kubelet's CPU manager: static, started before its configuration\n"
    node_tuner.main()
    assert calls[-1] == ("restart-kubelet",) and record[0]["kind"] == "kubelet"
    # Another component marks the host in use (a Station attached to the device layer): not now.
    said[("busy", "kubelet")], record[0] = "fleet-station: attached s1u_03\n", {}
    calls.clear()
    node_tuner.main()
    assert ("restart-kubelet",) not in calls and ("reboot",) not in calls
    assert published[-1][0][1].startswith("restarting kubelet once the robot is idle (fleet-station: attached s1u_03)")
    # In use for an hour (the driver runs): it waits and no attempt is counted, then restarts at the first idle pass.
    said[("busy", "kubelet")], status[0] = "", "REBOOT isolated CPUs (kernel command line): none\n"
    said[("busy", "host")] = said[("busy", "host", "own")] = "astribot_driver runs\n"
    for _ in range(60):
        node_tuner.main()
    assert ("reboot",) not in calls and record[0] == {}
    assert published[-1][0][1].startswith("restarting host once the robot is idle (astribot_driver runs)")
    # The robot's own reason is this Addon's mark on the host; another component's is that component's.
    assert ("in-use", "astribot_driver runs") in calls
    said[("busy", "host")] = said[("busy", "host", "own")] = ""
    node_tuner.main()
    assert calls[-1] == ("reboot",) and record[0]["attempts"] == 1
    monkeypatch.setenv("S1U_NODE_REBOOT", "never")
    calls.clear()
    record[0] = {}
    node_tuner.main()
    assert ("reboot",) not in calls
    # A restart on request (label), with restarts of its own switched off: refused while the profile does not hold,
    # waits while the robot is in use, done once when idle, and after the boot it says done and is not done again.
    requested = [{}]
    monkeypatch.setattr(node_tuner, "node", lambda: (
        {"rlark.io/node-category": "robot", node_tuner.REQUEST: "7"}, {node_tuner.REQUESTED: json.dumps(requested[0])}))
    monkeypatch.setattr(node_tuner, "publish", lambda *arguments, **annotations: (
        published.append((arguments, annotations)),
        requested.__setitem__(0, json.loads(annotations.get(node_tuner.REQUESTED, json.dumps(requested[0]))))))
    node_tuner.main()
    assert ("reboot",) not in calls and published[-1][0][1].startswith(
        "restart request 7 refused, the node profile does not hold; isolated CPUs")
    status[0] = "ok     everything\n"
    said[("busy", "host")] = "fleet-station: attached s1u_03\n"
    node_tuner.main()
    assert ("reboot",) not in calls and published[-1][0][1].startswith(
        "restart request 7 waits for the robot to be idle (fleet-station: attached s1u_03)")
    assert ("in-use", "") in calls  # another component's mark is not this Addon's to write
    said[("busy", "host")] = ""
    node_tuner.main()
    assert calls[-1] == ("reboot",) and requested[0]["profile"] == "7" and requested[0]["attempts"] == 1
    calls.clear()
    monkeypatch.setattr(node_tuner, "running", lambda kind: "another boot")
    node_tuner.main()
    node_tuner.main()
    assert ("reboot",) not in calls and requested[0]["done"].endswith("Z") and requested[0]["attempts"] == 1
    # Every pass: the profile applied, the mark written, then the API server.
    assert calls[1:4] == [("apply",), ("busy", "host", "own"), ("in-use", "")]


def test_the_busy_check_reads_other_components_marks_and_not_its_own(tmp_path):
    script = (TUNER / "node_profile.sh").read_text()
    library = script.rsplit('case "${1:-}" in', 1)[0].replace(
        "IN_USE=/run/rlark/in-use/s1u-robot", "IN_USE=%s/in-use/s1u-robot" % tmp_path).replace(
        "PROFILE=/etc/rlark/s1u/node_profile.env", "PROFILE=%s" % (TUNER / "node_profile.env"))
    (tmp_path / "library.sh").write_text(library)
    (tmp_path / "in-use").mkdir()

    def busy(*arguments):
        return subprocess.run(["bash", "-c", "source %s; driver_runs() { false; }; busy %s" % (
            tmp_path / "library.sh", " ".join(arguments))], capture_output=True, text=True).stdout.strip()

    assert busy("kubelet") == ""
    # One signal: any mark, its own among them, whoever asks; "own" (what its own mark says) reads none.
    (tmp_path / "in-use/s1u-robot").write_text("astribot_driver runs\n")
    assert busy("kubelet") == "s1u-robot: astribot_driver runs" and busy("kubelet", "own") == ""
    (tmp_path / "in-use/s1u-robot").unlink()
    (tmp_path / "in-use/fleet-station").write_text("attached s1u_03\n")
    assert busy("kubelet") == "fleet-station: attached s1u_03" and busy("kubelet", "own") == ""


def test_a_node_conforms_when_every_item_of_the_specification_holds_and_like_nodes_have_one_fingerprint():
    text = ("model os=ubuntu 20.04\nmodel kernel=5.15.0-139-generic\nmodel container runtime version=1.7.24\n"
            "model registry credentials on the node=none\nmodel kubelet arguments=--config=/var/lib/kubelet/config.yaml\n"
            "site name servers=192.168.31.1\nbody grippers=left v1 right v1\nrecord body serial=XC%s\nrecord firmware=B%s\n")
    labels = {"rlark.io/robot-model": "s1u", "rlark.io/robot-id": "s1u-02", "rlark.io/node-category": "robot",
              "rlark.io/robot-body": "s1u", node_spec.ORIN: "ok", node_spec.CLOCK: "no_source"}
    failing, marks = node_spec.conformance("s1u-02-x86", node_spec.parse(text % (2, 2)), labels, {}, "ok", "")
    assert failing == ["%s (%s): not declared on the Node" % reference[:2] for reference in node_spec.REFERENCES]
    declared = {"rlark.io/node-install-spec": "v1.33.13-abc.0123456789ab", "rlark.io/node-install-state": "ok",
                "station.rlark.io/network-spec": "c0ffee", "station.rlark.io/network-state": "no-handshake",
                "rlark.io/node-labels-spec": "aa", "rlark.io/node-labels-state": "ok",
                "rlark.io/registry-trust.spec": "ab12", "rlark.io/registry-trust.state": "deferred ab12: config_path"}
    check = lambda: node_spec.conformance("s1u-02-x86", node_spec.parse(text % (2, 2)), labels, declared, "ok", "")
    assert check()[0] == ["site tunnel (station): no-handshake", "registry trust (registry-trust): deferred ab12: config_path"]
    declared.update({"station.rlark.io/network-state": "ok", "rlark.io/registry-trust.state": "applied ab12"})
    conforming, trusted = check()
    assert conforming == [] and trusted[node_spec.FINGERPRINT + "model"] != marks[node_spec.FINGERPRINT + "model"]
    other = dict(labels, **{"rlark.io/robot-id": "s1u-01", "rlark.io/robot-body": "s1u_observer"})
    _, others = node_spec.conformance("s1u-01-x86", node_spec.parse(text % (1, 1)), other, {}, "ok", "")
    model, site, body = (node_spec.FINGERPRINT + scope for scope in ("model", "site", "body"))
    assert marks[model] == others[model] and marks[site] == others[site] and marks[body] != others[body]
    drift = text.replace("5.15.0-139", "5.15.0-140").replace("node=none", "node=present") % (2, 2)
    labels.update({"rlark.io/robot-id": "s1u-07", node_spec.ORIN: "unreachable"})
    del labels["rlark.io/robot-body"]
    failing, changed = node_spec.conformance("s1u-02-x86", node_spec.parse(drift), labels, {}, "reboot-required", "10.254.240.1")
    assert changed[model] != marks[model] and changed[site] == marks[site]
    assert failing[:5] == [
        "node profile (node-profile): reboot-required",
        "kernel (vendor): 5.15.0-140-generic, specified 5.15.0-139-generic",
        "registry credentials on the node (registry-trust): present, specified none",
        "label rlark.io/robot-body (admission): missing",
        "label rlark.io/robot-id (admission): s1u-07 on node s1u-02-x86"]
    assert failing[-2:] == ["Orin (orin): unreachable", "clock (clock): no_source"]


def test_nothing_is_installed_on_a_node_that_is_not_a_robot(monkeypatch):
    calls = []
    monkeypatch.setenv("NODE_NAME", "alexfu")
    monkeypatch.setattr(node_tuner.shutil, "copy", lambda *paths: None)
    monkeypatch.setattr(node_tuner, "host", lambda *arguments, quiet=False: calls.append(arguments) or (0, ""))
    monkeypatch.setattr(node_tuner, "node", lambda: ({"rlark.io/cluster-id": "rlark-robot-s1u-01"}, {}))

    class Stop(Exception):
        pass

    def stop(seconds):
        raise Stop

    monkeypatch.setattr(node_tuner.time, "sleep", stop)
    try:
        node_tuner.main()
    except Stop:
        pass
    assert calls == []  # the control plane of the robot cluster is a workstation: its host is not touched


def test_a_wave_acts_after_the_earlier_ones_are_done_with_the_image_and_says_whom_it_waits_for(monkeypatch):
    """What can take a robot off line reaches a declared few first: a wrong version would otherwise restart every
    idle robot in the same minute (the Pods themselves are replaced everywhere at once)."""
    image, now = "registry/rlark/s1u-robot@sha256:2", 10_000

    def robot(name, wave, state=None, since=0, ready=True, ready_since=0, change=image, why=""):
        return dict(name=name, wave=wave, ready=ready, since=ready_since,
                    rollout=dict(change=change, state=state, since=since, why=why) if state else {})

    def gate(wave, *others):
        return node_tuner.gate(wave, image, list(others), 600, now)

    done_, fresh = robot("a", "0", "done", since=9_000), robot("a", "0", "done", since=9_900)
    assert gate("0", robot("b", "0"), robot("c", None)) == ""
    assert gate(None) == "" and gate(None, robot("c", None)) == ""
    assert gate(None, done_) == "" and gate("1", done_, robot("c", None)) == ""
    assert gate(None, fresh) == "wave 0: a is not done with this image for 600 s yet (done)"
    assert gate(None, done_, robot("b", "0", ready=False)) == ""
    assert gate(None, robot("b", "0", ready=False)) == "no robot of an earlier wave is Ready and done with this image"
    assert gate(None, done_, robot("b", "0", "acting", ready=False, ready_since=9_000)) == (
        "wave 0: b failed with this image (not Ready since it began)")
    assert gate(None, done_, robot("b", "0", "failed", why="3 attempts did not restart it")) == (
        "wave 0: b failed with this image (3 attempts did not restart it)")

    calls, published, written, others = [], [], {}, [robot("s1u-01-x86", "0", "acting")]
    for name, value in (("NODE_NAME", "s1u-03-x86"), ("S1U_NODE_REBOOT", "idle"), ("S1U_NODE_ROLLOUT", "staged"),
                        ("S1U_NODE_ROLLOUT_SECONDS", "600"), ("S1U_IMAGE", image)):
        monkeypatch.setenv(name, value)
    said = {("status",): "REBOOT isolated CPUs (kernel command line): none\n", ("busy", "host"): "",
            ("facts",): "record body serial=X\nrecord body parameters=0\n", ("reboot",): "restarting\n"}
    monkeypatch.setattr(node_tuner, "host", lambda *arguments, quiet=False: calls.append(arguments) or (
        0, said.get(arguments, "")))
    monkeypatch.setattr(node_tuner, "node", lambda: ({"rlark.io/node-category": "robot"}, {}))
    monkeypatch.setattr(node_tuner, "robots", lambda: others + [robot("s1u-03-x86", None)])
    monkeypatch.setattr(node_tuner, "publish", lambda *arguments, **annotations: published.append((arguments, annotations)))
    monkeypatch.setattr(node_tuner.time, "time", lambda: now)

    class Told:
        def __init__(self, path):
            self.path = path

        def __enter__(self):
            return self

        def __exit__(self, *error):
            return False

        def write(self, text):
            written[self.path] = text

    opened = open
    monkeypatch.setattr("builtins.open", lambda path, *a, **k: Told(path) if str(path).endswith("/rollout") else opened(path, *a, **k))
    node_tuner.follow("profile", "", "")
    assert ("reboot",) not in calls
    assert written["/host/rollout"].startswith("waiting: wave 0: s1u-01-x86 is not done with this image")
    assert published[-1][0][1].startswith("restarting host waits for its wave (wave 0: s1u-01-x86 is not done")
    assert published[-1][0][2]["rlark.io/rollout-state"] == "waiting"
    assert json.loads(published[-1][1]["rlark.io/rollout"])["why"].startswith("wave 0: s1u-01-x86")
    others[0] = robot("s1u-01-x86", "0", "done", since=9_000)
    node_tuner.follow("profile", "", "")
    assert calls[-1] == ("reboot",) and written["/host/rollout"] == "allowed\n"
    calls.clear()
    monkeypatch.setenv("S1U_NODE_ROLLOUT", "paused")
    node_tuner.follow("profile", "", "")
    assert ("reboot",) not in calls and published[-1][0][2]["rlark.io/rollout-state"] == "paused"
    # Done: the profile holds and the Orin is as the Addon declares.
    said[("status",)] = "ok     everything\n"
    monkeypatch.setattr(node_tuner, "node", lambda: ({"rlark.io/node-category": "robot", node_spec.ORIN: "ok"}, {}))
    node_tuner.follow("profile", "", "")
    assert published[-1][0][2]["rlark.io/rollout-state"] == "done"
    assert json.loads(published[-1][1]["rlark.io/rollout"]) == dict(change=image, state="done", why="", since=now)


def test_the_x86_holds_its_own_address_only_and_everything_else_leaves_through_the_orin(tmp_path):
    """s1u_02 11-13: its Orin was leased the address of the site's network the x86 held by hand, and the x86 was gone.
    The x86 holds its own address, the Orin forwards under whatever address it has."""
    script = (TUNER / "node_profile.sh").read_text().replace(
        "/etc/rlark/s1u/node_profile.env", str(TUNER / "node_profile.env")).replace(
        "STATE=/run/rlark/s1u", "STATE=%s" % tmp_path)
    (tmp_path / "library.sh").write_text(script.rsplit('case "${1:-}" in', 1)[0])
    (tmp_path / "ip").write_text("""#!/bin/bash
case "$*" in
  "-4 -o addr show to 192.168.0.10") echo "2: enp2s0    inet 192.168.0.10/24 brd 192.168.0.255 scope global enp2s0" ;;
  "-4 -o addr show dev enp2s0") echo "2: enp2s0    inet 192.168.0.10/24 brd 192.168.0.255 scope global enp2s0"
     [[ ! -f $STUB/second ]] || echo "2: enp2s0    inet 192.168.31.250/32 scope global enp2s0" ;;
  "-4 route show default"*) [[ ! -f $STUB/routed ]] || echo "default via 192.168.0.11 dev enp2s0 proto static metric 100" ;;
  "route replace default via 192.168.0.11 dev enp2s0 proto static metric 100") touch $STUB/routed ;;
esac
""")
    (tmp_path / "nmcli").write_text("""#!/bin/bash
case "$*" in
  "-g GENERAL.CONNECTION device show enp2s0") echo "Wired connection 3" ;;
  *) echo "nmcli $*" >> $STUB/calls ;;
esac
""")
    (tmp_path / "ping").write_text("#!/bin/bash\n[[ -f $STUB/masquerade && $* == *'-I 192.168.0.10 10.11.14.128' ]]\n")
    (tmp_path / "hostname").write_text("#!/bin/sh\necho 192.168.0.10 192.168.31.251 10.254.240.7\n")
    for name in ("ip", "nmcli", "ping", "hostname"):
        (tmp_path / name).chmod(0o755)
    environment = {"PATH": "%s:/usr/bin:/bin" % tmp_path, "STUB": str(tmp_path)}
    rules = "#" + script.split("  # The uplink: this host holds its address", 1)[1].split('  rule "CPU shares', 1)[0]

    def drift(addresses):
        (tmp_path / "orin_addresses").write_text(addresses + "\n")
        return subprocess.run(["bash", "-c", 'source %s; source $PROFILE; rule() { [[ $2 == "$3" ]] || echo "$1: $2"; }\ncheck() {\n%s\n}\ncheck'
                               % (tmp_path / "library.sh", rules)], capture_output=True, text=True, env=environment).stdout.splitlines()

    assert drift("192.168.31.5") == ["default route: ", "the site answers this host's own address through the Orin (10.11.14.128): no"]
    default = "#" + script.split("  # Everything but the robot's own network leaves through the Orin", 1)[1].split("  local unit shares", 1)[0]
    for _ in range(2):
        subprocess.run(["bash", "-c", "source %s; source $PROFILE\ndefault() {\n%s\n}\ndefault" % (tmp_path / "library.sh", default)],
                       check=True, env=environment)
    assert (tmp_path / "calls").read_text() == "nmcli connection modify Wired connection 3 ipv4.gateway 192.168.0.11\n"
    (tmp_path / "masquerade").touch()
    assert drift("192.168.31.5") == []
    (tmp_path / "second").touch()
    assert drift("192.168.31.5") == ["addresses on the robot's interface enp2s0: 192.168.0.10 192.168.31.250"]
    (tmp_path / "second").unlink()
    # Room on the disk: each filesystem of kubelet's state, said before kubelet acts on it.
    (tmp_path / "df").write_text("""#!/bin/bash
[[ "$*" == "--output=target,pcent /var/lib /var/lib/kubelet" ]] || exit 2
printf 'Mounted on Use%%\\n/ %s%%\\n/ %s%%\\n' "$(cat $STUB/used)" "$(cat $STUB/used)"
""")
    (tmp_path / "df").chmod(0o755)
    space = "#" + script.split("  # Room on the disk, before kubelet removes images", 1)[1].split('  echo "note   CPUs', 1)[0]

    def room(used):
        (tmp_path / "used").write_text(str(used))
        return subprocess.run(["bash", "-c", "source %s; source $PROFILE\ncheck() {\n%s\n}\ncheck"
                               % (tmp_path / "library.sh", space)], capture_output=True, text=True, env=environment).stdout.splitlines()

    assert room(60) == ["ok     free space on /: 40 %"]
    assert room(81) == ["DRIFT  free space on /: 19 % (profile: at least 20 %)"]
    assert drift("192.168.31.251") == ["addresses of the Orin's uplink that this host holds too: 192.168.31.251"]
    assert drift("192.168.0.57 10.9.9.9") == [
        "addresses of the Orin's uplink inside the robot's own network 192.168.0.0/24: 192.168.0.57"]


def test_a_registry_credential_is_a_value_not_the_empty_section_containerd_keeps_for_it(tmp_path):
    """11-14: both robots' containerd configuration has an empty [plugins."io.containerd.grpc.v1.cri".registry.auths]
    header; the node specification read it as a credential and both nodes did not conform."""
    script = (TUNER / "node_profile.sh").read_text()
    pattern = re.search(r"grep -qiE '([^']+)' \"\$config\"", script.split("model registry credentials on the node=", 1)[1])[1]
    config = tmp_path / "config.toml"

    def read(text):
        config.write_text(text)
        return "present" if subprocess.run(["grep", "-qiE", pattern, str(config)]).returncode == 0 else "none"

    assert read('[plugins."io.containerd.grpc.v1.cri".registry]\n  [plugins."io.containerd.grpc.v1.cri".registry.auths]\n') == "none"
    assert read('[plugins."io.containerd.grpc.v1.cri".registry.configs."r:5443".auth]\n  username = "u"\n') == "present"


def test_the_clock_servo_closes_a_second_within_its_rate_limit_and_only_while_the_site_answers():
    import struct

    assert clock.ctypes.sizeof(clock.Timex) == 208  # struct timex on x86-64

    def stamp(seconds):
        return int(seconds) + clock.NTP_EPOCH, int(seconds % 1 * 2 ** 32)
    sent = 1000.0
    reply = struct.pack("!12I", *[0] * 8, *stamp(sent + 0.010 + 0.25), *stamp(sent + 0.012 + 0.25))
    offset, round_trip = clock.offset_of(reply, sent, sent + 0.022)
    assert abs(offset + 0.25) < 1e-6 and abs(round_trip - 0.020) < 1e-6
    servo, offset, history = clock.Servo(), -1.0, []
    for _ in range(600):
        ppm = servo.frequency(offset)
        assert abs(ppm) <= clock.LIMIT_PPM
        offset += ppm * 1e-6 * clock.PERIOD_SECONDS
        history.append(offset)
    locked = next(index for index, value in enumerate(history) if abs(value) <= clock.LOCKED_SECONDS)
    assert 25 * 60 < locked * clock.PERIOD_SECONDS < 40 * 60 and max(history) < clock.LOCKED_SECONDS
    assert abs(history[-1]) < 1e-4
    assert clock.hardware_clock([["bash"], ["sudo", "-S", "phc2sys", "-s", "nonexistent0", "-c", "CLOCK_REALTIME"]]) is None


def make_orin(tmp_path, monkeypatch, payloads=(), retired=()):
    """An Orin keeper with the payload directories given ({name: {file: text}}), its login in tmp_path."""
    monkeypatch.setattr(orin.tempfile, "mkdtemp", lambda dir: str(tmp_path))
    monkeypatch.setattr(orin, "ACCESS", str(tmp_path))
    monkeypatch.setattr(orin, "HOST", str(tmp_path))
    monkeypatch.setattr(orin, "IN_USE", tmp_path / "in-use")
    monkeypatch.setattr(orin, "PAYLOADS", tmp_path / "payloads")
    for name, files in dict(payloads).items():
        for file, text in files.items():
            (tmp_path / "payloads" / name / file).parent.mkdir(parents=True, exist_ok=True)
            (tmp_path / "payloads" / name / file).write_text(text)
    return orin.Orin("192.168.0.11", "astribot", "192.168.0.10", "s1u-02-x86", "led_driver/led1[.]py",
                     "Documents/system_setup.sh", list(retired))


def test_a_payload_has_one_version_of_its_files_and_a_changed_host_key_stops_everything_until_confirmed(tmp_path, monkeypatch):
    robot = make_orin(tmp_path, monkeypatch, {"quest-bridge": {"install": "#!/bin/sh\n", "bridge/bridge.py": "x"}})
    version, archive = robot.payloads["quest-bridge"]
    names = {member.name: member for member in tarfile.open(fileobj=io.BytesIO(archive))}
    assert set(names) == {"install", "bridge/bridge.py", "rlark.env", "VERSION"} and names["install"].mode == 0o755
    env = tarfile.open(fileobj=io.BytesIO(archive)).extractfile("rlark.env").read().decode()
    assert "S1U_ROS_IP_ADDRESS=192.168.0.10\n" in env and "S1U_ORIN_USER=astribot\n" in env
    # The version is the payload's files', not the robot's addresses.
    assert orin.payload(tmp_path / "payloads/quest-bridge", {})[0] == version
    (tmp_path / "payloads/quest-bridge/bridge/bridge.py").write_text("y")
    assert orin.payload(tmp_path / "payloads/quest-bridge", {})[0] != version
    assert [orin.delay(failures) for failures in (0, 1, 2, 4, 9)] == [60, 120, 240, 900, 900]
    assert [orin.verdict(*keys) for keys in ((None, "a"), ("a", "a"), ("a", "b"))] == ["first", "trusted", "changed"]
    # An Orin presenting another host key than the trusted one: nothing is sent to it until a person confirms the
    # presented fingerprint on the Node.
    monkeypatch.setenv("S1U_NODE_WANTED", "installed")
    sent, trusted = [], []
    monkeypatch.setattr(robot, "scan", lambda: ("ssh-ed25519 NEW", "SHA256:new"))
    record = {"fingerprint": "SHA256:old", "at": "2026-11-13T00:00:00Z", "how": "first use"}
    monkeypatch.setattr(robot, "known", lambda: ("ssh-ed25519 OLD", record))
    monkeypatch.setattr(robot, "ssh", lambda *arguments, **options: sent.append(arguments) or done())
    monkeypatch.setattr(robot, "trust", lambda *arguments: trusted.append(arguments) or {"how": arguments[2]})
    monkeypatch.setattr(orin.subprocess, "run", lambda *a, **k: type("R", (), {"stdout": "256 SHA256:old host (ED25519)"})())
    assert robot.reconcile()["label"] == "no_access" and not sent  # the secret is not mounted
    (tmp_path / "password").write_text("p\n")
    result = robot.reconcile()
    assert result["label"] == "identity_changed" and not sent and not trusted and result["trust"] == record
    assert (result["trusted_fingerprint"], result["presented_fingerprint"]) == ("SHA256:old", "SHA256:new")
    robot.confirm = "SHA256:other"
    assert robot.reconcile()["label"] == "identity_changed" and not trusted
    robot.confirm = "SHA256:new"
    monkeypatch.setattr(robot, "root", lambda key, result: [])
    # Waiting for a person is no failure: the next look is a minute later, not backed off.
    robot.state["failures"] = 0
    monkeypatch.setattr(robot, "place", lambda key, steps, result: "")
    assert robot.reconcile()["label"] == "ok" and trusted == [("ssh-ed25519 NEW", "SHA256:new", "confirmed")]
    stopped = PROFILE["S1U_ORIN_STOPPED"].strip('"')
    script = "/usr/bin/python3 /home/astribot/astribot_workspace/astribot_device_driver/led_driver/led1.py /dev/ttyLED0"
    assert re.search(stopped, script) and not re.search(stopped, "bash -c pkill -e -f -- '%s' || true" % stopped)


def test_payloads_are_placed_switched_when_the_host_is_free_and_removed_when_no_longer_declared(tmp_path, monkeypatch):
    robot = make_orin(tmp_path, monkeypatch, {"quest-bridge": {"install": "#!/bin/sh\n"}})
    version = robot.payloads["quest-bridge"][0]
    (tmp_path / "rollout").write_text("allowed\n")
    on, sent = {"declared": "old-thing", "quest-bridge": ""}, []

    def ssh(key, remote, data=b"", password=False):
        sent.append(remote)
        if remote.startswith("echo declared="):
            return done("".join("%s=%s\n" % item for item in on.items()).encode())
        return done()

    monkeypatch.setattr(robot, "ssh", ssh)
    monkeypatch.setattr(robot, "sudo", lambda key, remote: sent.append("sudo " + remote) or done())
    # Marked in use by another component: the new version goes on the Orin and is installed, not switched to.
    (tmp_path / "in-use").mkdir()
    (tmp_path / "in-use/fleet-station").write_text("attached s1u_02\n")
    result, steps = {}, []
    waiting = robot.place("KEY", steps, result)
    assert waiting.startswith("quest-bridge %s is on the Orin; it starts when nothing marks the host in use (fleet-station" % version)
    assert "sudo ~/rl/quest-bridge.%s/install" % version in sent and not [line for line in sent if "install restart" in line]
    # The payload no longer declared goes, by its own removal first; the list on the Orin is the Addon's.
    assert "sudo sh -c '[ ! -x ~astribot/rl/old-thing/install ] || ~astribot/rl/old-thing/install remove'" in sent
    assert sent[-1] == "mkdir -p ~/rl && echo quest-bridge > ~/rl/.rlark-payloads"
    # Any mark holds a payload, the robot's own (the vendor driver runs) too; with none it is switched to.
    (tmp_path / "in-use/fleet-station").unlink()
    (tmp_path / "in-use/s1u-robot").write_text("astribot_driver runs\n")
    assert robot.place("KEY", [], result).endswith("(s1u-robot: astribot_driver runs)")
    (tmp_path / "in-use/s1u-robot").unlink()
    sent.clear()
    on["declared"] = "quest-bridge"
    assert robot.place("KEY", [], result) == ""
    assert "sudo ~/rl/quest-bridge.%s/install restart" % version in sent and result["payloads"] == {
        "quest-bridge": dict(wanted=version, running=version)}
    # Its wave not yet: placed, not switched.
    (tmp_path / "rollout").write_text("waiting: wave 0\n")
    sent.clear()
    assert robot.place("KEY", [], result).endswith("it starts with its wave of the rollout")
    on["quest-bridge"] = version
    sent.clear()
    assert robot.place("KEY", [], result) == "" and not [line for line in sent if line.startswith("sudo")]


def test_the_uplink_is_one_file_its_setup_script_is_the_addons_and_a_retired_unit_restarts_it(tmp_path, monkeypatch):
    unit = (ORIN / "s1u-orin-uplink.service").read_text()
    assert "@" not in unit and "$" not in unit and "-s %s ! -d" % PROFILE["S1U_ROS_IP_ADDRESS"] in unit
    setup = orin.uplink_setup()
    assert unit in setup and "s1u-orin-uplink %s" % hashlib.sha256(unit.encode()).hexdigest()[:16] in setup
    # The script a new robot's Orin gets from Station API is this one, byte for byte.
    assert (CATALOG / "bootstrap/orin-uplink.sh").read_text() == setup
    (tmp_path / "systemctl").write_text("#!/bin/sh\necho \"$*\" >> %s/systemctl.calls\n[ \"$1\" != is-active ] || echo active\n" % tmp_path)
    (tmp_path / "systemctl").chmod(0o755)
    placed = setup.replace("/etc/systemd/system/s1u-orin-uplink.service", str(tmp_path / "placed.service"))
    for _ in range(2):
        subprocess.run(["sh"], input=placed, text=True, check=True, env={"PATH": "%s:/usr/bin:/bin" % tmp_path})
    assert (tmp_path / "placed.service").read_bytes() == unit.encode()
    assert (tmp_path / "systemctl.calls").read_text().count("daemon-reload") == 1
    # On the Orin: placed where it is not, and a unit retired by the Addon is stopped and the uplink started again.
    robot = make_orin(tmp_path, monkeypatch, retired=["fleet-orin-uplink"])
    there = dict(unit=hashlib.sha256(unit.encode()).hexdigest(), active="active", forwarding="1",
                 addresses="192.168.31.5", retired="fleet-orin-uplink", camera="", found="")
    sent = []

    def ssh(key, remote, data=b"", password=False):
        sent.append(remote)
        if remote.startswith("echo unit="):
            return done("".join("%s=%s\n" % item for item in there.items()).encode())
        return done(b"1000\n" if remote == "id -u" else b"")

    monkeypatch.setattr(robot, "ssh", ssh)
    monkeypatch.setattr(robot, "sudo", lambda key, remote: sent.append("sudo " + remote) or done())
    robot.root("KEY", {})
    assert "sudo sh -c 'systemctl disable --now --quiet fleet-orin-uplink.service; rm -f /etc/systemd/system/fleet-orin-uplink.service; " \
           "sh ~astribot/rl/.s1u-orin-uplink.sh && systemctl restart s1u-orin-uplink.service'" in sent
    assert (tmp_path / "orin_addresses").read_text() == "192.168.31.5\n"
    camera = next(line for line in sent if line.startswith("sudo sh -c 'install -m 644 ~astribot/rl/.s1u-camera.rules"))
    assert camera.endswith("|| { rm -f /etc/udev/rules.d/70-s1u-camera.rules; exit 1; }'")  # retried while udev did not take it
    # A unit not there yet is placed by its setup script, which starts it: no restart beyond that (its stop step would
    # drop the x86's forwarding for a moment).
    there.update(unit="", retired="")
    sent.clear()
    robot.root("KEY", {})
    assert "sudo sh -c 'sh ~astribot/rl/.s1u-orin-uplink.sh'" in sent
    # Nothing to retire, the unit as it is, the rule there: nothing as root.
    there.update(unit=hashlib.sha256(unit.encode()).hexdigest(), retired="",
                 camera='SUBSYSTEM=="usb", ATTR{idVendor}=="2bc5", OWNER="1000", MODE="0660"')
    sent.clear()
    robot.root("KEY", {})
    assert not [line for line in sent if line.startswith("sudo")]
    # What is asked, run by a real shell on a machine without any of it: every answer under its own name.
    asked = subprocess.run(["sh", "-c", next(line for line in sent if line.startswith("echo unit="))],
                           capture_output=True, text=True).stdout.splitlines()
    assert [line.split("=")[0] for line in asked] == ["unit", "active", "forwarding", "addresses", "retired", "camera", "found"]


def test_removing_takes_back_what_the_orin_got_and_sends_nothing_to_one_never_trusted(tmp_path, monkeypatch):
    monkeypatch.setenv("S1U_NODE_WANTED", "removed")
    robot = make_orin(tmp_path, monkeypatch)
    (tmp_path / "password").write_text("p\n")
    sent, patched = [], []

    def ssh(key, remote, data=b"", password=False):
        sent.append(remote)
        return done(b"declared=quest-bridge\n" if remote.startswith("echo declared=") else b"")

    monkeypatch.setattr(robot, "scan", lambda: ("ssh-ed25519 KEY", "SHA256:key"))
    monkeypatch.setattr(robot, "ssh", ssh)
    monkeypatch.setattr(robot, "sudo", lambda key, remote: sent.append("sudo " + remote) or done())
    monkeypatch.setattr(robot, "api", lambda *arguments: patched.append(arguments))
    monkeypatch.setattr(robot, "known", lambda: (None, None))
    assert robot.reconcile()["label"] == "removed" and not sent and not patched  # never trusted: never touched
    # Without the login: removed where no host key was ever trusted, else it cannot say.
    (tmp_path / "password").unlink()
    assert robot.reconcile()["label"] == "removed" and not sent
    monkeypatch.setattr(robot, "known", lambda: ("ssh-ed25519 KEY", {"fingerprint": "SHA256:key"}))
    assert robot.reconcile()["label"] == "no_access" and not sent
    (tmp_path / "password").write_text("p\n")
    monkeypatch.setattr(robot, "known", lambda: ("ssh-ed25519 KEY", {"fingerprint": "SHA256:key"}))
    (tmp_path / "in-use").mkdir()
    (tmp_path / "in-use/fleet-station").write_text("attached\n")
    assert robot.reconcile()["label"] == "removing" and sent == ["true"]
    (tmp_path / "in-use/fleet-station").unlink()
    assert robot.reconcile()["label"] == "removed"
    assert "sudo sh -c '[ ! -x ~astribot/rl/quest-bridge/install ] || ~astribot/rl/quest-bridge/install remove'" in sent
    assert "s/^#s1u-stopped# //' ~/Documents/system_setup.sh" in sent[-1] and "/ s1u-robot$/d" in sent[-1]
    assert patched == [("PATCH", "/api/v1/nodes/s1u-02-x86",
                        {"metadata": {"annotations": {orin.HOST_KEY: None, orin.TRUST: None, orin.SERIAL: None}}})]


def test_the_orin_resets_the_cameras_usb_once_per_request_of_the_device_layer(tmp_path, monkeypatch):
    """s1u_01 11-13 22:40: the head camera's own stream wedged and no restart of the vendor's camera driver brought it
    back. The device layer asks through a file on the host; the reset is the Orin account's (the cameras' rule)."""
    robot = make_orin(tmp_path, monkeypatch)
    sent, events, answer = [], [], ["bus 002 dev 008\n", 0]
    monkeypatch.setattr(robot, "known", lambda: ("ssh-ed25519 KEY", {}))
    monkeypatch.setattr(robot, "ssh", lambda key, remote, *a, **k: sent.append(remote) or done(
        answer[0].encode(), answer[1], b"PermissionError: [Errno 13]\n"))
    monkeypatch.setattr(robot, "event", lambda *arguments: events.append(arguments))
    robot.camera_pass()
    assert not sent  # no request
    (tmp_path / "camera-reset-request").write_text(json.dumps({"id": "a" * 32, "cameras": ["base_rgb"]}))
    # The device layer asks only when it may (it knows whether a Station is attached): no mark is consulted.
    (tmp_path / "in-use").mkdir()
    (tmp_path / "in-use/fleet-station").write_text("attached\n")
    robot.camera_pass()
    assert sent[0].startswith("python3 -c '") and sent[0].endswith("' 2bc5") and "sudo" not in sent[0]
    result = json.loads((tmp_path / "camera-reset-result").read_text())
    assert set(result) == {"id", "ok", "detail", "finished_at"}  # the contract with the device layer
    assert result["id"] == "a" * 32 and result["ok"] and result["detail"] == "reset ok bus 002 dev 008"
    assert events[0][0] == "OrinCameraReset"
    robot.camera_pass()
    assert len(sent) == 1  # the same request once
    (tmp_path / "camera-reset-request").write_text(json.dumps({"id": "b" * 32, "cameras": ["base_rgb"]}))
    answer[:] = ["", 1]
    robot.camera_pass()
    assert json.loads((tmp_path / "camera-reset-result").read_text())["detail"] == "PermissionError: [Errno 13]"
    script = orin.USB_RESET.replace("/sys/bus/usb/devices/*", str(tmp_path / "sys/*")).replace(
        "/dev/bus/usb/%03d/%03d", str(tmp_path / "dev/%03d-%03d")).replace("fcntl.ioctl(f, 0x5514)", "f.write(b'reset')")
    for name, vendor, bus, number in (("2-3.2.6", "2bc5", 2, 8), ("1-1", "0bda", 1, 3)):
        (tmp_path / "sys" / name).mkdir(parents=True)
        for key, value in (("idVendor", vendor), ("busnum", bus), ("devnum", number)):
            (tmp_path / "sys" / name / key).write_text("%s\n" % value)
    (tmp_path / "dev").mkdir()
    said = subprocess.run([sys.executable, "-c", script, "2bc5"], capture_output=True, text=True, check=True).stdout
    assert said == "bus 002 dev 008\n" and (tmp_path / "dev/002-008").read_bytes() == b"reset"
    assert not (tmp_path / "dev/001-003").exists()


def test_a_trusted_host_key_is_kept_on_the_robot_at_once(tmp_path, monkeypatch):
    robot = make_orin(tmp_path, monkeypatch)
    monkeypatch.setattr(orin, "KEPT", str(tmp_path / "orin_trust.json"))
    calls = []
    monkeypatch.setattr(robot, "api", lambda *arguments, **options: calls.append(arguments) or {})
    robot.serial = "17"
    record = robot.trust("ssh-ed25519 NEW", "SHA256:new", "confirmed")
    kept = json.loads((tmp_path / "orin_trust.json").read_text())
    assert kept == dict(key="ssh-ed25519 NEW", trust=record, serial="17") and record["how"] == "confirmed"
    assert calls[0][2]["metadata"]["annotations"][orin.CONFIRM] is None  # the confirmation is used up
