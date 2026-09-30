#!/usr/bin/env python3
"""Measure create/restore to an actual guest response on a disposable kind lab."""
import argparse
import json
from pathlib import Path
import socket
import statistics
import subprocess as sp
import time
import urllib.error
import urllib.request

p = argparse.ArgumentParser()
p.add_argument('--image', required=True)
p.add_argument('--node', required=True, help='hold placement constant when comparing runs')
p.add_argument('--port', type=int, default=8080)
p.add_argument('--cpus', type=int, default=2)
p.add_argument('--memory', default='512Mi')
p.add_argument('--runs', type=int, default=5)
p.add_argument('--cold-cache', action='store_true', help='evict this base from the idle test node before each boot')
p.add_argument('--output', required=True)
a = p.parse_args()
if a.runs < 1:
    p.error('--runs must be positive')
Path(a.output).parent.mkdir(parents=True, exist_ok=True)

def k(*args):
    return sp.check_output(['kubectl', *args])

context = k('config', 'current-context').decode().strip()
assert context in {'kind-roamvm', 'kind-roamvm-cilium'}, 'disposable lab only'
assert a.node.startswith(context.removeprefix('kind-') + '-worker')
node_ip = json.loads(k('get', 'node', a.node, '-o', 'json'))['status']['addresses'][0]['address']
name = 'startup-' + str(int(time.time()))
proxy = sp.Popen(['kubectl', 'proxy', '--port=0'], stdout=sp.PIPE, stderr=sp.PIPE, text=True)
url = 'http://' + proxy.stdout.readline().strip().split()[-1]
base = '/apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines'
core = '/api/v1/namespaces/default'
results = {'image': a.image, 'node': a.node, 'cpus': a.cpus, 'memory': a.memory, 'coldCache': a.cold_cache, 'runs': []}

def api(path, method='GET', body=None):
    data = None if body is None else json.dumps(body).encode()
    content_type = 'application/merge-patch+json' if method == 'PATCH' else 'application/json'
    req = urllib.request.Request(url + path, data=data, method=method, headers={'Content-Type': content_type})
    with urllib.request.urlopen(req, timeout=15) as res:
        return json.load(res)

def wait_for(fn, timeout=180):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = fn()
        if value:
            return value
        time.sleep(.02)
    raise TimeoutError('waiting for ' + fn.__name__)

def guest_ready():
    try:
        with socket.create_connection((node_ip, node_port), timeout=.1) as conn:
            conn.settimeout(.1)
            if a.port == 22:
                return conn.recv(128).startswith(b'SSH-2.0-')
            conn.sendall(b'GET /ready HTTP/1.0\r\nHost: guest\r\n\r\n')
            return b'200 OK' in conn.recv(512)
    except OSError:
        return False

try:
    service = api(core + '/services', 'POST', {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': name}, 'spec': {
        'type': 'NodePort', 'selector': {'vm.roamvm.io/name': name}, 'ports': [{'port': a.port}]}})
    node_port = service['spec']['ports'][0]['nodePort']
    for cycle in range(a.runs):
        if a.cold_cache:
            vms = json.loads(k('get', 'rvm', '-A', '-o', 'json'))['items']
            assert not any(v.get('status', {}).get('nodeName') == a.node and v['spec']['image'] == a.image for v in vms), 'base still in use'
            digest = a.image.split('@sha256:')[1]
            assert len(digest) == 64 and all(c in '0123456789abcdef' for c in digest)
            sp.run(['docker', 'exec', a.node, 'crictl', 'rmi', a.image], check=True, stdout=sp.DEVNULL)
        start = time.monotonic()
        if cycle == 0:
            api(base, 'POST', {'apiVersion': 'vm.roamvm.io/v1alpha1', 'kind': 'VirtualMachine', 'metadata': {'name': name}, 'spec': {
                'powerState': 'Running', 'image': a.image, 'cpus': a.cpus, 'memory': a.memory, 'readinessPort': a.port,
                'nodeSelector': {'kubernetes.io/hostname': a.node}, 'resources': {'requests': {'cpu': '500m'}, 'limits': {'cpu': str(a.cpus)}}}})
        else:
            api(base + '/' + name, 'PATCH', {'spec': {'powerState': 'Running'}})
        timing = {'operation': 'create' if cycle == 0 else 'restore'}
        while time.monotonic() - start < 180:
            vm = api(base + '/' + name)
            status = vm.get('status', {})
            phase = status.get('phase')
            if phase:
                timing.setdefault(phase, round(time.monotonic() - start, 3))
            assert phase not in {'Error', 'RecoveryRequired', 'Blocked'}, vm
            if status.get('podName'):
                pods = api(core + '/pods?labelSelector=vm.roamvm.io/name%3D' + name)['items']
                if pods:
                    pod = pods[0]
                    for label, value in [('podCreated', True), ('scheduled', pod['spec'].get('nodeName')), ('containerRunning', any('running' in c.get('state', {}) for c in pod.get('status', {}).get('containerStatuses', [])))]:
                        if value:
                            timing.setdefault(label, round(time.monotonic() - start, 3))
            if any(c['type'] == 'Ready' and c['status'] == 'True' for c in status.get('conditions', [])):
                timing.setdefault('vmReady', round(time.monotonic() - start, 3))
            if guest_ready():
                timing['guestResponse'] = round(time.monotonic() - start, 3)
                break
            time.sleep(.02)
        assert 'guestResponse' in timing, vm
        results['runs'].append(timing)
        print(json.dumps(timing), flush=True)
        logs = k('logs', status['podName'], '--timestamps').decode()
        Path(a.output + f'.{cycle}.log').write_text(logs)
        api(base + '/' + name, 'PATCH', {'spec': {'powerState': 'Stopped'}})
        wait_for(lambda: api(base + '/' + name).get('status', {}).get('phase') == 'Stopped')
        wait_for(lambda: not api(core + '/pods?labelSelector=vm.roamvm.io/name%3D' + name)['items'])
        wait_for(lambda: not any(p['metadata']['name'] == status['podName'] + '-working' for p in api(core + '/persistentvolumeclaims')['items']))
    results['medianSeconds'] = statistics.median(r['guestResponse'] for r in results['runs'])
    results['success'] = True
    print('Median:', results['medianSeconds'], flush=True)
finally:
    try:
        for path in [base + '/' + name, core + '/services/' + name]:
            try:
                api(path, 'DELETE', {})
            except urllib.error.HTTPError as e:
                if e.code != 404:
                    raise
    finally:
        proxy.terminate()
        proxy.wait(timeout=5)
        Path(a.output).write_text(json.dumps(results, indent=2) + '\n')
