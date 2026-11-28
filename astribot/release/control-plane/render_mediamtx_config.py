#!/usr/bin/env python3
"""Render a protected MediaMTX config from site settings and credentials."""

import argparse
import ipaddress
import json
import os
import re
import stat
import tempfile
from pathlib import Path


def read_env_file(path):
    values = {}
    for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or not re.fullmatch(r"[A-Z][A-Z0-9_]*", key):
            raise ValueError(f"invalid environment entry on line {line_number}")
        if key in values:
            raise ValueError(f"duplicate environment key on line {line_number}")
        values[key] = value
    return values


def read_protected_json(path):
    mode = stat.S_IMODE(path.stat().st_mode)
    if mode & 0o077:
        raise ValueError(f"protected file permissions must be 0600 or stricter: {path}")
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"protected file must contain a JSON object: {path}")
    return data


def required(values, key):
    value = values.get(key, "")
    if not value:
        raise ValueError(f"missing required setting {key}")
    return value


def yaml_string(value):
    return json.dumps(value, ensure_ascii=True)


def render(settings, credentials):
    cidr = ipaddress.ip_network(required(settings, "MEDIAMTX_STATION_SOURCE_CIDR"), strict=False)
    agent = required(settings, "MEDIAMTX_PILOT_AGENT")
    node = required(settings, "MEDIAMTX_PILOT_NODE")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+", agent) or not re.fullmatch(r"[A-Za-z0-9_.-]+", node):
        raise ValueError("pilot agent and node must contain only letters, digits, dot, underscore, or hyphen")

    publish_user = required(credentials, "publishUser")
    publish_password = required(credentials, "publishPassword")
    reader_user = required(credentials, "readerUser")
    reader_password = required(credentials, "readerPassword")
    control_user = required(credentials, "controlUser")
    control_password = required(credentials, "controlPassword")
    if len({publish_user, reader_user, control_user}) != 3:
        raise ValueError("MediaMTX roles must use distinct usernames")
    ice_host = required(settings, "MEDIAMTX_ICE_ADDITIONAL_HOST")
    paths = [f"{agent}/{node}/{view}" for view in ("head", "torso", "wrist_left", "wrist_right")]

    def permissions(action):
        return "\n".join(f"        - action: {action}\n          path: {yaml_string(path)}" for path in paths)

    return "\n".join(
        [
            "logLevel: info",
            "rtsp: true",
            "rtspAddress: :8554",
            "rtspTransports: [tcp]",
            "hls: false",
            "rtmp: false",
            "playback: false",
            "srt: false",
            "moq: false",
            "api: true",
            "apiAddress: :9997",
            "metrics: false",
            "pprof: false",
            "authMethod: internal",
            "authInternalUsers:",
            f"  - user: {yaml_string(publish_user)}",
            f"    pass: {yaml_string(publish_password)}",
            f"    ips: [{yaml_string(str(cidr))}]",
            "    permissions:",
            permissions("publish"),
            f"  - user: {yaml_string(reader_user)}",
            f"    pass: {yaml_string(reader_password)}",
            "    ips: []",
            "    permissions:",
            permissions("read"),
            f"  - user: {yaml_string(control_user)}",
            f"    pass: {yaml_string(control_password)}",
            "    ips: []",
            "    permissions:",
            "      - action: api",
            "webrtc: true",
            "webrtcAddress: :8889",
            "webrtcEncryption: false",
            "webrtcAllowOrigins: []",
            "webrtcLocalUDPAddress: :8189",
            "webrtcLocalTCPAddress: :8189",
            "webrtcIPsFromInterfaces: false",
            f"webrtcAdditionalHosts: [{yaml_string(ice_host)}]",
            "paths:",
            "  all_others:",
            "    source: publisher",
            "",
        ]
    )


def write_private_file(path, content):
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            stream.write(content)
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--environment-file", required=True, type=Path)
    parser.add_argument("--credentials-file", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    try:
        content = render(read_env_file(args.environment_file), read_protected_json(args.credentials_file))
        write_private_file(args.output, content)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        parser.error(str(error))
    print(f"Rendered protected MediaMTX configuration at {args.output}")


if __name__ == "__main__":
    main()
