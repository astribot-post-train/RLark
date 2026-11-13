#!/usr/bin/env python3
"""Configure a routed x86 address through its Orin; run --help for usage."""
import argparse
import base64
import ipaddress
import json
import os
import re
from pathlib import Path
import subprocess
import sys

STATE = Path('/var/lib/rlark-robot-x86-network/state.json')
SYSCTL = Path('/etc/sysctl.d/60-rlark-robot-x86-network.conf')
UNIT = Path('/etc/systemd/system/rlark-robot-x86-forward.service')


def run(*args, capture=True):
    return subprocess.run(args, check=True, text=True, stdout=subprocess.PIPE if capture else None).stdout or ''


def remote(role, action, cfg):
    def nm(*args):
        return run('nmcli', *args).strip()
    def prop(profile, name):
        return nm('-g', name, 'connection', 'show', 'uuid', profile)
    def setprop(profile, name, value):
        run('nmcli', 'connection', 'modify', 'uuid', profile, name, value)
    def sysget(name):
        return run('sysctl', '-n', name).strip()
    def sysset(name, value):
        run('sysctl', '-w', name + '=' + value)
    target = cfg['x86_internal'] if role == 'orin' else cfg['orin_internal']
    route = json.loads(run('ip', '-j', 'route', 'get', target))[0]
    dev = route['dev']
    lan_dev = None
    if role == 'orin':
        links = json.loads(run('ip', '-j', 'address', 'show'))
        network = ipaddress.ip_network(cfg['lan'])
        candidates = [link['ifname'] for link in links if any(
            a.get('family') == 'inet' and ipaddress.ip_address(a['local']) in network
            for a in link.get('addr_info', []))]
        if len(candidates) != 1 or candidates[0] == dev:
            raise RuntimeError('cannot uniquely identify distinct LAN and robot interfaces')
        lan_dev = candidates[0]
    conn = nm('-g', 'GENERAL.CONNECTION', 'device', 'show', dev)
    if not conn or conn == '--':
        raise RuntimeError('no active NetworkManager profile for ' + dev)
    uuid = nm('-g', 'GENERAL.CON-UUID', 'device', 'show', dev)
    if not uuid or uuid == '--':
        raise RuntimeError('cannot identify NetworkManager profile UUID')
    keys = ['ipv4.routes'] if role == 'orin' else ['ipv4.addresses', 'ipv4.routes']
    before = {k: prop(uuid, k) for k in keys}
    actual = {'role': role, 'device': dev, 'uuid': uuid, 'settings': before}
    if role == 'orin':
        actual['lan_device'] = lan_dev
        actual['forward'] = sysget('net.ipv4.ip_forward')
        actual['proxy'] = sysget('net.ipv4.conf.' + lan_dev + '.proxy_arp')
        actual['sysctl_file'] = SYSCTL.read_text() if SYSCTL.exists() else None
    print(json.dumps(actual, indent=2), flush=True)
    if action == 'check':
        return
    if os.geteuid() != 0:
        raise RuntimeError('apply and restore require root')
    def rules():
        return [['-i', lan_dev, '-o', dev, '-d', cfg['external'] + '/32'],
                ['-i', dev, '-o', lan_dev, '-s', cfg['external'] + '/32']]
    if action == 'restore':
        if not STATE.exists():
            print('No managed backup on this host; nothing to restore', flush=True)
            return
        state = json.loads(STATE.read_text())
        if state['role'] != role or state['cfg'] != cfg or state['uuid'] != uuid:
            raise RuntimeError('backup does not match this device and configuration')
        # Never overwrite an operator's changes after this script ran.
        for k in keys:
            if before[k] not in (state['before'][k], state['after'].get(k)):
                raise RuntimeError('profile changed since apply: ' + k)
        if role == 'orin' and UNIT.exists() and UNIT.read_text() != state.get('unit'):
            raise RuntimeError('managed firewall unit changed; refusing restore')
        if role == 'orin' and SYSCTL.exists() and SYSCTL.read_text() != state['managed_sysctl']:
            raise RuntimeError('managed sysctl file changed; refusing restore')
        for k in keys:
            setprop(uuid, k, state['before'][k])
        run('nmcli', 'device', 'reapply', dev)
        if role == 'orin':
            if UNIT.exists():
                run('systemctl', 'disable', '--now', 'rlark-robot-x86-forward.service')
                UNIT.unlink()
                run('systemctl', 'daemon-reload')
            if SYSCTL.exists():
                SYSCTL.unlink()
            sysset('net.ipv4.ip_forward', state['forward'])
            sysset('net.ipv4.conf.' + lan_dev + '.proxy_arp', state['proxy'])
        STATE.unlink()
        print('Restored original settings', flush=True)
        return
    if STATE.exists():
        state = json.loads(STATE.read_text())
        if state['role'] != role or state['cfg'] != cfg or state['uuid'] != uuid:
            raise RuntimeError('existing backup belongs to a different configuration')
        for k in keys:
            if before[k] not in (state['before'][k], state['after'].get(k)):
                raise RuntimeError('profile changed outside this script: ' + k)
    else:
        if role == 'orin' and (SYSCTL.exists() or UNIT.exists()):
            raise RuntimeError('managed file already exists and ownership is unknown')
        if role == 'orin':
            for rule in rules():
                probe = subprocess.run(['iptables', '-C', 'FORWARD', *rule, '-j', 'ACCEPT'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                if probe.returncode == 0:
                    raise RuntimeError('matching firewall rule already exists; ownership unknown')
                if probe.returncode != 1:
                    raise RuntimeError('cannot inspect firewall rules')
        # Refuse to adopt pre-existing address or route entries.
        live_addr = json.loads(run('ip', '-j', 'address', 'show'))
        if any(a.get('local') == cfg['external'] for link in live_addr for a in link.get('addr_info', [])):
            raise RuntimeError('target address already exists on this host')
        destination = cfg['external'] + '/32' if role == 'orin' else cfg['lan']
        if json.loads(run('ip', '-j', 'route', 'show', 'exact', destination)):
            raise RuntimeError('target route already exists; ownership unknown')
        if any(cfg['external'] in value for value in before.values()):
            raise RuntimeError('target address already appears in profile; ownership unknown')
        state = {'role': role, 'cfg': cfg, 'uuid': uuid, 'before': before, 'after': {},
                 'forward': actual.get('forward'), 'proxy': actual.get('proxy'),
                 'managed_sysctl': '# Managed by rlark robot-x86-network.py\nnet.ipv4.ip_forward=1\nnet.ipv4.conf.' + (lan_dev or '') + '.proxy_arp=1\n'}
        if role == 'orin':
            import shlex
            lines = [' '.join(shlex.quote(v) for v in rule) for rule in rules()]
            start = ' && '.join('(/usr/sbin/iptables -C FORWARD ' + line + ' -j ACCEPT || /usr/sbin/iptables -I FORWARD 1 ' + line + ' -j ACCEPT)'  for line in lines)
            stop = ' ; '.join('/usr/sbin/iptables -D FORWARD ' + line + ' -j ACCEPT || true' for line in lines)
            state['unit'] = '[Unit]\nDescription=RLark robot x86 forwarding\nAfter=network-online.target docker.service\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/sh -c ' + shlex.quote(start) + '\nExecStop=/bin/sh -c ' + shlex.quote(stop) + '\n[Install]\nWantedBy=multi-user.target\n'
        STATE.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        STATE.write_text(json.dumps(state, indent=2))
        STATE.chmod(0o600)
    desired = {'ipv4.routes': (cfg['external'] + '/32 ' + cfg['x86_internal']) if role == 'orin' else (cfg['lan'] + ' ' + cfg['orin_internal'] + ' src=' + cfg['external'])}
    if role == 'x86':
        desired['ipv4.addresses'] = cfg['external'] + '/32'
    for k, value in desired.items():
        current = prop(uuid, k)
        if current == state['before'][k]:
            # Record intent first so a lost SSH connection after nmcli succeeds is recoverable.
            state['after'][k] = (current + ', ' if current else '') + value
            temporary = STATE.with_suffix('.tmp')
            temporary.write_text(json.dumps(state, indent=2))
            temporary.chmod(0o600)
            os.replace(temporary, STATE)
            setprop(uuid, '+' + k, value)
            observed = prop(uuid, k)
            if observed != state['after'][k]:
                raise RuntimeError('NetworkManager normalized settings unexpectedly; inspect backup before recovery')
        elif current != state['after'].get(k):
            raise RuntimeError('unexpected profile state for ' + k)
    if role == 'orin':
        if 'unit' not in state:
            import shlex
            lines = [' '.join(shlex.quote(v) for v in rule) for rule in rules()]
            start = ' && '.join('(/usr/sbin/iptables -C FORWARD ' + line + ' -j ACCEPT || /usr/sbin/iptables -I FORWARD 1 ' + line + ' -j ACCEPT)'  for line in lines)
            stop = ' ; '.join('/usr/sbin/iptables -D FORWARD ' + line + ' -j ACCEPT || true' for line in lines)
            state['unit'] = '[Unit]\nDescription=RLark robot x86 forwarding\nAfter=network-online.target docker.service\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/sh -c ' + shlex.quote(start) + '\nExecStop=/bin/sh -c ' + shlex.quote(stop) + '\n[Install]\nWantedBy=multi-user.target\n'
            STATE.write_text(json.dumps(state, indent=2))
        if SYSCTL.exists() and SYSCTL.read_text() != state['managed_sysctl']:
            raise RuntimeError('sysctl file ownership conflict')
        if UNIT.exists() and UNIT.read_text() != state['unit']:
            raise RuntimeError('firewall unit ownership conflict')
        UNIT.write_text(state['unit'])
        run('systemctl', 'daemon-reload')
        run('systemctl', 'enable', '--now', 'rlark-robot-x86-forward.service')
        SYSCTL.write_text(state['managed_sysctl'])
        sysset('net.ipv4.ip_forward', '1')
        sysset('net.ipv4.conf.' + lan_dev + '.proxy_arp', '1')
    run('nmcli', 'device', 'reapply', dev)
    print('Applied persistent settings', flush=True)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('action', choices=['check', 'apply', 'restore'])
    p.add_argument('--orin', required=True, help='Orin LAN IPv4 address')
    p.add_argument('--external', required=True, help='unique LAN IPv4 address for x86')
    p.add_argument('--lan', default='192.168.31.0/24', help='LAN IPv4 CIDR')
    p.add_argument('--x86-internal', default='192.168.0.10')
    p.add_argument('--orin-internal', default='192.168.0.11')
    p.add_argument('--orin-user', default='astribot')
    p.add_argument('--x86-user', default='astribot')
    p.add_argument('--address-reserved', action='store_true', help='confirm LAN address allocation and DHCP conflict check')
    a = p.parse_args()
    for user in (a.orin_user, a.x86_user):
        if not re.fullmatch(r'[a-zA-Z_][a-zA-Z0-9_.-]*', user):
            p.error('invalid SSH username')
    for value in (a.orin, a.external, a.x86_internal, a.orin_internal):
        if ipaddress.ip_address(value).version != 4:
            p.error('only IPv4 is supported')
    network = ipaddress.ip_network(a.lan, strict=True)
    if network.version != 4 or ipaddress.ip_address(a.external) not in network or ipaddress.ip_address(a.orin) not in network:
        p.error('Orin and external addresses must belong to LAN')
    if a.external in (a.orin, a.x86_internal, a.orin_internal) or ipaddress.ip_address(a.external) in (network.network_address, network.broadcast_address):
        p.error('external address is invalid or conflicts with another endpoint')
    if a.action == 'apply' and not a.address_reserved:
        p.error('apply requires --address-reserved after checking DHCP and allocation')
    cfg = {'external': a.external, 'lan': str(network), 'x86_internal': a.x86_internal, 'orin_internal': a.orin_internal}
    source = base64.b64encode(Path(__file__).read_bytes()).decode('ascii')
    payload = base64.b64encode(json.dumps(cfg).encode()).decode('ascii')
    # HostKeyAlias keeps identical robot-internal IPs distinct in known_hosts.
    jump = a.orin_user + '@' + a.orin
    def invoke(role):
        host = jump if role == 'orin' else a.x86_user + '@' + a.x86_internal
        opts = ['-o', 'ConnectTimeout=10', '-o', 'StrictHostKeyChecking=accept-new']
        if role == 'x86':
            opts += ['-J', jump, '-o', 'HostKeyAlias=robot-x86-' + a.orin.replace('.', '-')]
        code = "import base64;exec(compile(base64.b64decode('" + source + "'),'<robot-network>','exec'))"
        command = ['python3', '-c', code, '--remote', role, a.action, payload]
        if a.action != 'check':
            command = ['sudo', '--'] + command
        import shlex
        print('\n== ' + role + ' ==', flush=True)
        subprocess.run(['ssh', '-t', *opts, host, shlex.join(command)], check=True)
    # Check both endpoints before touching either. Apply x86 first; restore Orin first.
    if a.action == 'apply':
        original = a.action
        a.action = 'check'
        invoke('orin'); invoke('x86')
        a.action = original
    for role in (('orin', 'x86') if a.action != 'apply' else ('x86', 'orin')):
        invoke(role)


if __name__ == '__main__':
    try:
        if len(sys.argv) == 5 and sys.argv[1] == '--remote':
            remote(sys.argv[2], sys.argv[3], json.loads(base64.b64decode(sys.argv[4])))
        else:
            main()
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError, RuntimeError) as exc:
        print('Error: ' + (('command failed with exit status ' + str(exc.returncode)) if isinstance(exc, subprocess.CalledProcessError) else str(exc)), file=sys.stderr)
        sys.exit(1)
