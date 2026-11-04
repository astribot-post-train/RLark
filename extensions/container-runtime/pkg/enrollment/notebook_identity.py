"""Notebook instance identity shared by SSH discovery and service launch guards."""
import hashlib
from pathlib import Path
import socket


def instance_id(proc=Path('/proc')):
    # PID namespace + PID 1 start time fences container replacement; boot ID
    # fences host reboot even when the namespace inode is reused.
    namespace = (proc / '1/ns/pid').readlink().name
    started = (proc / '1/stat').read_text().rsplit(')', 1)[1].split()[19]
    boot = (proc / 'sys/kernel/random/boot_id').read_text().strip()
    return hashlib.sha256('\0'.join((namespace, started, boot)).encode()).hexdigest()


def private_address():
    # The SSH endpoint can be a platform DNAT address. Observe the container
    # network namespace itself instead of treating the SSH address as its IP.
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as route:
        route.connect(('10.255.255.255', 9))
        return route.getsockname()[0]


def validate_instance(config):
    expected = config.get('instanceID')
    # Packages emitted by the current enrollment path always carry both
    # values. Omit the check for old test/compatibility bundles; they cannot
    # claim the new replacement-fencing guarantee.
    if expected is not None and (instance_id() != expected or
                                 private_address() != config.get('privateAddress')):
        raise RuntimeError('Notebook identity or private address changed; explicit replacement required')
