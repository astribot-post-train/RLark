"""Read-only proof of the existing gateway's managed service identities."""
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import time

REUSE_GATEWAY_PROBE_V1 = True


def load(path):
    info = path.lstat()
    if not path.is_file() or path.is_symlink() or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise RuntimeError('invalid private service receipt')
    return json.loads(path.read_text())


def verify(install_root, state_root, uid, instance, address, agent_digest, proc=Path('/proc'), now=None):
    if not re.fullmatch(r'[0-9a-f-]{36}', uid) or not re.fullmatch(r'[0-9a-f]{64}', instance) or not re.fullmatch(r'[0-9a-f]{64}', agent_digest):
        raise RuntimeError('invalid gateway identity')
    state = Path(state_root) / uid / 'services'
    install = Path(install_root) / uid
    for path in (state, install):
        info = path.stat()
        if path.resolve(strict=True) != path or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise RuntimeError('invalid private gateway directory')
    status = load(state / 'services-status.json')
    processes = load(state / 'processes.json')
    identity = processes['identity']
    prefix = uid + '/' + instance + '/'
    if not identity.startswith(prefix) or not re.fullmatch(r'[0-9a-f]{64}', identity[len(prefix):]):
        raise RuntimeError('foreign gateway service identity')
    if status['identity'] != identity or status['digest'] != processes['digest']:
        raise RuntimeError('gateway service receipt mismatch')
    age = (time.time() if now is None else now) - status['sampledAt']
    required = {'agent', 'gateway', 'task-manager'}
    if not 0 <= age < 4 or not required.issubset(status['services']):
        raise RuntimeError('gateway services not observed')
    package = install / identity[len(prefix):]
    if package.resolve(strict=True) != package:
        raise RuntimeError('gateway package is not canonical')
    config = load(package / 'services.json')
    if config['identity'] != identity or config.get('instanceID') != instance or config.get('privateAddress') != address or config['stateDirectory'] != str(state):
        raise RuntimeError('gateway configuration identity mismatch')
    if hashlib.sha256(json.dumps(config, sort_keys=True).encode()).hexdigest() != processes['digest']:
        raise RuntimeError('gateway configuration digest mismatch')
    binaries = {'agent': 'container-runtime-agent', 'gateway': 'container-runtime-gateway', 'task-manager': 'raw-supervisor'}
    for name in sorted(required):
        receipt = load(state / (name + '.pid.json'))
        process = processes['processes'][name]
        if receipt['identity'] != identity or receipt['digest'] != processes['digest'] or receipt['pid'] != process['pid'] or receipt['started'] != process['started']:
            raise RuntimeError('gateway process receipt mismatch')
        directory = proc / str(process['pid'])
        fields = (directory / 'stat').read_text().rsplit(') ', 1)[1].split()
        if fields[0] == 'Z' or fields[19] != process['started']:
            raise RuntimeError('gateway process replaced')
        executable = directory / 'exe'
        if executable.resolve(strict=True) != package / binaries[name]:
            raise RuntimeError('gateway process executable mismatch')
        if name == 'agent':
            digest = hashlib.sha256()
            with executable.open('rb') as binary:
                for chunk in iter(lambda: binary.read(1024 * 1024), b''):
                    digest.update(chunk)
            if digest.hexdigest() != agent_digest:
                raise RuntimeError('gateway Agent release mismatch')
    return {'identity': identity, 'agentSHA256': agent_digest, 'services': sorted(required)}


if __name__ == '__main__':
    if instance_id() != sys.argv[4] or private_address() != sys.argv[5]:
        raise RuntimeError('Notebook instance or address changed')
    print(json.dumps(verify(*sys.argv[1:7])))
