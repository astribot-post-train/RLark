"""The node tuner of the Addon s1u-robot, on every S1U robot x86. It makes the host follow its node profile and tells
the cluster the result, with no shell on the box:

- copies node_profile.sh and node_profile.env, which this image holds beside this file, to the host's /run/rlark/s1u
  and runs the script in the host's namespaces: install once (a new image, so a new profile, rolls this Pod), then
  apply and status every minute;
- publishes the result on its own Node: label rlark.io/node-profile-state = ok | drift | kubelet-restart-required |
  reboot-required, and annotation rlark.io/node-profile-report with the status lines that are not ok;
- does a restart the profile needs by itself while the robot is idle (S1U_NODE_REBOOT "idle", the Addon's default):
  the whole robot (kernel command line, systemd's default affinity) through the vendor's restart while the vendor
  driver does not run, its motors are off and nothing marks the host in use (/run/rlark/in-use: any component's
  mark, a Station attached to the robot's device layer or this Addon's own); kubelet alone (a new kubelet
  configuration of the profile; the Pods keep running) while the driver does not run and nothing marks the host in use. While the robot is
  in use it waits, as long as that takes, with the reason in the report. Once idle, each attempt is recorded on the
  Node (annotation rlark.io/node-profile-restart) with what the restart changes, the boot's identifier or kubelet's
  start: changed and the rules still do not hold, it is not repeated for this profile; unchanged five minutes later,
  the restart did not happen and is tried again, three times. With "never" the label stays reboot-required or
  kubelet-restart-required until an operator restarts it;
- marks the host in use while the vendor driver runs or the motors are on: /run/rlark/in-use/s1u-robot, with the
  reason, checked every minute without the API server. RLark replaces its programs on the host (the site tunnel's)
  only while that directory is empty. The file is the host's and not this Pod's: a Pod that ends or is rolled leaves
  it as it is, and the next Pod's first pass corrects it within seconds of its start; "removed" takes it away;
- restarts the whole robot once on request: the Node's label rlark.io/restart-request with a value of the asker's
  choosing (through the robot's node object in RLark). Done while the robot is idle by the same judgement as above,
  whatever S1U_NODE_REBOOT says (that parameter is about restarts nobody asked for); refused, with the reason in the
  report, while the node profile does not hold, since a restart brings back what is on the disk; the same value is
  not done twice. Annotation rlark.io/restart-request-state says what became of it, "done" with the time of the boot
  that followed;
- does what can take a robot off line in waves (the Addon's parameter rollout "staged", its default): the restarts
  above and, through the file /run/rlark/s1u/rollout, the Orin container's switch of a payload's version. A robot's
  wave is its Node's label rlark.io/rollout-wave, a whole number; without it the robot is of the last wave. A wave
  acts once every Ready robot of the earlier waves has been done with the same image for rolloutSeconds, at least one
  has, and none failed; with the API server away nothing of this is done. Where a robot is, and whom it waits for, is
  on its Node: label rlark.io/rollout-state, annotation rlark.io/rollout. "paused" holds every robot, "all" is no
  waves. The Pods themselves are replaced on every robot at once: what is held back is the acting, not the version;
- publishes whether the whole node conforms to the S1U node's specification and its fingerprints (node_spec.py),
  and which body the box drives, as the vendor's file on the box says: annotations rlark.io/body-serial and
  s1u.rlark.io/body-parameters (the file's digest).

With S1U_NODE_WANTED "removed" (the Addon's parameter profile, before the Addon itself is uninstalled) it takes the
profile back instead (node_profile.sh uninstall), and the label says removed.

Standard library only (any image with python3, bash and nsenter). Usage: node_tuner.py [--once]"""
import calendar
import glob
import hashlib
import json
import os
import shutil
import ssl
import subprocess
import sys
import time
import urllib.request

import node_spec

HOST = "/host"  # the host's /run/rlark/s1u
SOURCE = os.path.dirname(os.path.abspath(__file__))
ON_HOST = "/run/rlark/s1u"
ACCOUNT = "/var/run/secrets/kubernetes.io/serviceaccount"
STATE, REPORT = "rlark.io/node-profile-state", "rlark.io/node-profile-report"
BOOT = "rlark.io/boot-report"
CATEGORY = "rlark.io/node-category"
RESTART = "rlark.io/node-profile-restart"
# A restart on request: the label is the request (set on the robot's node object in RLark, whose label sync brings it
# here), the annotation what became of it: {"profile": the request, "was": the boot it was asked in, "at", "attempts",
# "said", "done": when the host booted after it}.
REQUEST, REQUESTED = "rlark.io/restart-request", "rlark.io/restart-request-state"
# What can take a robot off line is done in waves (gate): the label is the robot's wave, a whole number, and a robot
# without it is of the last one; the annotation is where the robot is with the image it runs, {"change": the image,
# "state": done | acting | waiting | paused | failed, "since", "why"}, and the label repeats the state.
WAVE, ROLLOUT, ROLLOUT_STATE = "rlark.io/rollout-wave", "rlark.io/rollout", "rlark.io/rollout-state"
ATTEMPTS, RESTART_SECONDS = 3, 300  # tries of a restart that does not happen, and how long one may take
SERIAL, PARAMETERS = "rlark.io/body-serial", "s1u.rlark.io/body-parameters"


def host(*arguments, quiet=False):
    """node_profile.sh ``arguments`` in the host's namespaces: (exit code, output), the output in this Pod's log
    too unless ``quiet``."""
    result = subprocess.run(
        ["nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--",
         "bash", f"{ON_HOST}/node_profile.sh", *arguments],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    if not quiet:
        print(result.stdout, end="", flush=True)
    return result.returncode, result.stdout


def state(status):
    """(label value, report) from the script's status lines: reboot-required when a rule holds only after a
    restart of the host, kubelet-restart-required when a restart of kubelet is enough, drift when another fails, with those rules as the report."""
    failed = [line for line in status.splitlines() if line.startswith(("REBOOT", "KUBELET", "DRIFT"))]
    report = "; ".join(line.split(None, 1)[1] for line in failed)
    if any(line.startswith("REBOOT") for line in failed):
        return "reboot-required", report
    if any(line.startswith("KUBELET") for line in failed):
        return "kubelet-restart-required", report
    return ("drift", report) if failed else ("ok", "")


def publish(value, report, labels={}, **annotations):
    """The state on this Pod's Node, through the API server with the Pod's service account, with how long the
    host's last boot took (boot_report.py: seconds from the power button to each milestone)."""
    boot = json.load(open(f"{HOST}/boot_report.json")) if os.path.exists(f"{HOST}/boot_report.json") else {}
    request = urllib.request.Request(
        "https://%s:%s/api/v1/nodes/%s" % (os.environ["KUBERNETES_SERVICE_HOST"],
                                            os.environ["KUBERNETES_SERVICE_PORT"], os.environ["NODE_NAME"]),
        data=json.dumps({"metadata": {"labels": dict(labels, **{STATE: value}), "annotations": {
            REPORT: report[:1000], BOOT: json.dumps(boot.get("seconds_from_power_button", {})), **annotations}}}).encode(),
        method="PATCH",
        headers={"Authorization": "Bearer " + open(f"{ACCOUNT}/token").read(),
                 "Content-Type": "application/merge-patch+json"})
    urllib.request.urlopen(request, context=ssl.create_default_context(cafile=f"{ACCOUNT}/ca.crt"), timeout=10)


def node():
    """This Node's (labels, annotations)."""
    request = urllib.request.Request(
        "https://%s:%s/api/v1/nodes/%s" % (os.environ["KUBERNETES_SERVICE_HOST"],
                                            os.environ["KUBERNETES_SERVICE_PORT"], os.environ["NODE_NAME"]),
        headers={"Authorization": "Bearer " + open(f"{ACCOUNT}/token").read()})
    context = ssl.create_default_context(cafile=f"{ACCOUNT}/ca.crt")
    with urllib.request.urlopen(request, context=context, timeout=10) as reply:
        metadata = json.load(reply)["metadata"]
    return metadata.get("labels", {}), metadata.get("annotations", {})


def robots():
    """Every robot's Node of the cluster as gate reads it: name, wave, whether it is Ready and since when (seconds),
    and its rollout annotation."""
    request = urllib.request.Request(
        "https://%s:%s/api/v1/nodes?labelSelector=%s%%3Drobot" % (
            os.environ["KUBERNETES_SERVICE_HOST"], os.environ["KUBERNETES_SERVICE_PORT"], CATEGORY),
        headers={"Authorization": "Bearer " + open(f"{ACCOUNT}/token").read()})
    context = ssl.create_default_context(cafile=f"{ACCOUNT}/ca.crt")
    with urllib.request.urlopen(request, context=context, timeout=10) as reply:
        items = json.load(reply)["items"]
    found = []
    for item in items:
        ready = next(condition for condition in item["status"]["conditions"] if condition["type"] == "Ready")
        found.append(dict(
            name=item["metadata"]["name"], wave=item["metadata"].get("labels", {}).get(WAVE),
            ready=ready["status"] == "True",
            since=calendar.timegm(time.strptime(ready["lastTransitionTime"], "%Y-%m-%dT%H:%M:%SZ")),
            rollout=json.loads(item["metadata"].get("annotations", {}).get(ROLLOUT, "{}"))))
    return found


def gate(wave, change, others, seconds, now):
    """Why a robot of ``wave`` (its label, None for the last wave) may not yet do, for the image ``change``, what
    can take it off line; "" when it may. It waits for the earlier waves: every robot of them that is Ready has
    been done with this image for ``seconds``, at least one has, and none failed. A robot that began to act and has
    not been Ready since for ``seconds`` failed: it cannot say so itself."""
    earlier = [robot for robot in others if robot["wave"] is not None and (wave is None or int(robot["wave"]) < int(wave))]
    if not earlier:
        return ""
    done = 0
    for robot in earlier:
        rollout = robot["rollout"] if robot["rollout"].get("change") == change else {}
        state = rollout.get("state")
        if state == "failed" or state == "acting" and not robot["ready"] and now - robot["since"] > seconds:
            return "wave %s: %s failed with this image (%s)" % (robot["wave"], robot["name"], rollout.get("why") or (
                "not Ready since it began"))
        if state == "done" and now - rollout["since"] >= seconds:
            done += 1
        elif robot["ready"]:
            return "wave %s: %s is not done with this image for %d s yet (%s)" % (
                robot["wave"], robot["name"], seconds, state or "not begun")
    return "" if done else "no robot of an earlier wave is Ready and done with this image"


def running(kind):
    """What a restart of ``kind`` changes: the boot's identifier (host), kubelet's start in clock ticks since boot
    (kubelet; this Pod is in the host's PID namespace)."""
    if kind == "host":
        return open("/proc/sys/kernel/random/boot_id").read().strip()
    for path in glob.glob("/proc/[0-9]*/comm"):
        try:
            if open(path).read().strip() == "kubelet":
                return open(os.path.dirname(path) + "/stat").read().rsplit(")", 1)[1].split()[19]
        except OSError:  # it ended meanwhile
            pass
    return None


def restart(kind, record, profile, current, now):
    """Whether the restart of ``kind`` this ``profile`` needs is tried now: (the record to keep on the Node, None)
    when it is, else (None, why not). ``record`` is the last attempt's, ``current`` what running(kind) reads now."""
    if record.get("kind") != kind or record.get("profile") != profile:
        record = dict(kind=kind, profile=profile, attempts=0)
    elif record["was"] != current:
        return None, "it was restarted for this profile and the rules still do not hold"
    elif now - record["at"] < RESTART_SECONDS:
        return None, "its restart was asked for %d s ago" % (now - record["at"])
    elif record["attempts"] >= ATTEMPTS:
        return None, "%d attempts did not restart it; the last said: %s" % (record["attempts"], record.get("said"))
    return dict(record, was=current, at=now, attempts=record["attempts"] + 1), None


def follow(profile, source, busy):
    """One pass against the API server: the state on the Node, a restart the profile needs; ``busy`` is why the robot
    is in use now ("" when idle)."""
    value, report = state(host("status")[1])
    labels, annotations = node()
    # Whether this robot's wave may act yet (the Addon's parameter rollout: staged, paused, or all at once).
    mode, change, now = os.environ["S1U_NODE_ROLLOUT"], os.environ["S1U_IMAGE"], time.time()
    held = {"all": "", "paused": "the Addon's parameter rollout is paused"}.get(mode)
    if held is None:
        held = gate(labels.get(WAVE), change, [other for other in robots() if other["name"] != os.environ["NODE_NAME"]],
                    int(os.environ["S1U_NODE_ROLLOUT_SECONDS"]), now)
    with open(f"{HOST}/rollout", "w") as told:  # for the Orin's container: its switch of a payload's version
        told.write(f"waiting: {held}\n" if held else "allowed\n")
    needed = {"reboot-required": "host", "kubelet-restart-required": "kubelet"}.get(value)
    record = None
    failed = ""
    if needed and os.environ["S1U_NODE_REBOOT"] == "idle":
        # A robot in use waits, as long as it takes, and that is no attempt (s1u 11-13: three refusals while the
        # driver ran used up the three attempts, and the robots, idle later, were never restarted).
        waits = host("busy", needed, quiet=True)[1].strip()
        if held:
            report = f"restarting {needed} waits for its wave ({held}); {report}"
        elif waits:
            report = f"restarting {needed} once the robot is idle ({waits}); {report}"
        else:
            record, why = restart(needed, json.loads(annotations.get(RESTART, "{}")), profile, running(needed), now)
            report = report if record else f"not restarting {needed}: {why}; {report}"
            failed = "" if record or why.startswith("its restart was asked") else why
    # Where this robot is with the image, for the later waves and for people.
    before = json.loads(annotations.get(ROLLOUT, "{}"))
    where = ("done" if value == "ok" and labels.get(node_spec.ORIN) == "ok" else "failed" if failed else
             "paused" if mode == "paused" else "waiting" if held else "acting")
    rollout = dict(change=change, state=where, why="" if where == "done" else failed or held, since=(
        before["since"] if (before.get("change"), before.get("state")) == (change, where) else now))
    # A restart a person asks for (the Node's label REQUEST, any value of theirs as the request's number): once,
    # while the robot is idle, and only of a node whose profile holds, because a restart shows what the disk says
    # (11-14: s1u_02 with its uplink moved on the running system only would have come back on the earlier one).
    request, asked, requested = labels.get(REQUEST), json.loads(annotations.get(REQUESTED, "{}")), None
    if request and asked.get("profile") == request and asked["was"] != running("host"):
        if "done" not in asked:
            booted = next(int(line.split()[1]) for line in open("/proc/stat") if line.startswith("btime"))
            asked = dict(asked, done=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(booted)))
    elif request and value != "ok":
        report = f"restart request {request} refused, the node profile does not hold; {report}"
    elif request:
        if busy:
            report = f"restart request {request} waits for the robot to be idle ({busy}); {report}"
        else:
            requested, why = restart("host", asked, request, running("host"), now)
            report = report if requested else f"restart request {request}: {why}; {report}"
    asked = requested or asked
    # The whole node against its specification (node_spec.py), and which body the box drives.
    facts = node_spec.parse(host("facts", quiet=True)[1])
    failing, marks = node_spec.conformance(os.environ["NODE_NAME"], facts, labels, annotations, value, source)
    identity = dict(marks, **{
        SERIAL: facts["record"]["body serial"], PARAMETERS: facts["record"]["body parameters"],
        node_spec.SPEC: profile, node_spec.FACTS: json.dumps(facts, sort_keys=True),
        node_spec.REPORT: "; ".join(failing)[:2000]})
    conforms = {node_spec.CONFORMANCE: "not-ok" if failing else "ok", ROLLOUT_STATE: where}
    identity[ROLLOUT] = json.dumps(rollout)
    # The attempt is on the Node before it is made: a restart of the host ends this Pod.
    publish(value, report, conforms, **identity, **({RESTART: json.dumps(record)} if record else {}),
            **({REQUESTED: json.dumps(asked)} if asked else {}))
    if record:
        # What the step said stays with the attempt: the reason of one that did not restart anything.
        said = host("reboot" if needed == "host" else "restart-kubelet")[1]
        publish(value, report, conforms, **identity, **{RESTART: json.dumps(dict(record, said=said[-300:]))})
    if requested:
        said = host("reboot")[1]
        publish(value, report, conforms, **identity, **{REQUESTED: json.dumps(dict(requested, said=said[-300:]))})

def main():
    for name in ("node_profile.sh", "node_profile.env", "boot_report.py"):
        shutil.copy(f"{SOURCE}/{name}", f"{HOST}/{name}")
    if node()[0].get(CATEGORY) != "robot":
        # The profile rewrites a host's kernel command line, CPU affinities and kubelet: only a robot's (11-13: the
        # robot cluster's control plane is a workstation, and a selector by cluster would have reached it).
        print("this node is not a robot (%s): nothing is installed" % CATEGORY, flush=True)
        while True:
            time.sleep(3600)
    if os.environ["S1U_NODE_WANTED"] == "removed":
        host("in-use", quiet=True)
        code, removal = host("uninstall")
        publish("drift" if code else "removed", removal[-900:] if code else
                "the host is as before the node profile from its next boot")
        if code:
            raise SystemExit(code)
        while True:
            time.sleep(3600)
    code, install = host("install", f"{ON_HOST}/node_profile.env")
    if code:
        publish("drift", "install failed: " + install[-900:])
        raise SystemExit(code)
    profile = hashlib.sha256(b"".join(open(f"{SOURCE}/{name}", "rb").read() for name in (
        "node_profile.sh", "node_profile.env", "node_spec.py"))).hexdigest()[:16]
    source = os.environ["S1U_SITE_TIME_SOURCE"]  # the Addon's timeSource: declared, the clock must be locked to it
    # Off the SDK core and the device layer's CPUs, which kubelet's pool for Pods holds too: what this container runs
    # on the host every minute stays with it.
    kubernetes = next(line.split("=", 1)[1].strip() for line in open(f"{SOURCE}/node_profile.env")
                      if line.startswith("S1U_KUBERNETES_CPUS="))
    os.sched_setaffinity(0, {cpu for part in kubernetes.split(",")
                             for cpu in range(int(part.split("-")[0]), int(part.split("-")[-1]) + 1)})
    while True:
        host("apply")  # what needs no API server: a robot cut off from the site still follows its profile
        host("in-use", host("busy", "host", "own", quiet=True)[1].strip(), quiet=True)
        busy = host("busy", "host", quiet=True)[1].strip()
        try:
            follow(profile, source, busy)
        except OSError as error:  # the API server is away (s1u-02-x86 11-13: its service address timed out for hours)
            print("the Node is not reached, again in a minute: %s" % error, flush=True)
        if "--once" in sys.argv:
            return
        time.sleep(60)

if __name__ == "__main__":
    main()
