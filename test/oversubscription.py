#!/usr/bin/env python3
"""Constrain one disposable kind worker to two CPUs and exercise real KVM guests."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import subprocess as sp
import time

p = argparse.ArgumentParser()
p.add_argument('--image', required=True, help='test/guest image, including the /cpu endpoint')
p.add_argument('--node', required=True)
p.add_argument('--output', default='test-results/oversubscription.json')
a = p.parse_args()

def run(args, data=None):
    return sp.run(args, input=data, capture_output=True, check=True).stdout

def k(*args, data=None):
    return run(['kubectl', *args], data)

def obj(kind, name):
    return json.loads(k('get', kind, name, '-o', 'json'))

def apply(value):
    k('apply', '-f', '-', data=json.dumps(value).encode())

def wait_for(fn, timeout=120):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = fn()
        if value:
            return value
        time.sleep(.5)
    raise AssertionError('timed out waiting for ' + fn.__name__)

def phase(name, expected):
    wait_for(lambda: obj('rvm', name).get('status', {}).get('phase') == expected)
    if expected == 'Running':
        k('wait', 'pod/' + obj('rvm', name)['status']['podName'], '--for=condition=Ready', '--timeout=60s')
        wait_for(lambda: http(name, '/ready') == b'ready\n')

def power(name, state):
    k('patch', 'rvm', name, '--type=merge', '-p', json.dumps({'spec': {'powerState': state}}))

def cpu_ids(value):
    ids = set()
    for part in value.split(','):
        ends = [int(n) for n in part.split('-')]
        ids.update(range(ends[0], ends[-1] + 1))
    return ids

context = k('config', 'current-context').decode().strip()
assert context in {'kind-roamvm', 'kind-roamvm-cilium'}, 'local lab only'
assert a.node.startswith(context.removeprefix('kind-') + '-worker')
assert not any(v.get('status', {}).get('nodeName') == a.node
               for v in json.loads(k('get', 'rvm', '-A', '-o', 'json'))['items']), 'worker must have no VMs'
node = obj('node', a.node)
capacity = int(node['status']['capacity']['cpu'])
assert capacity > 2
original_config = run(['docker', 'exec', a.node, 'cat', '/var/lib/kubelet/config.yaml'])
assert b'kubeReserved:' not in original_config, 'use an unmodified kind worker'
original_cpuset = run(['docker', 'inspect', a.node, '--format', '{{.HostConfig.CpusetCpus}}']).decode().strip()
effective = run(['docker', 'exec', a.node, 'cat', '/sys/fs/cgroup/cpuset.cpus.effective']).decode().strip()
cpus = sorted(cpu_ids(effective))
assert len(cpus) >= 2
cpuset = ','.join(str(n) for n in cpus[:2])
prefix = 'overcommit-' + str(int(time.time()))
client = prefix + '-client'
names = [prefix + '-a', prefix + '-b']
pending = prefix + '-pending'
results = {'node': a.node, 'cpuset': cpuset, 'allocatableCPUs': 2, 'guestCPUs': 8,
           'cpuRequestPerVM': '500m', 'checks': []}

def check(name, value):
    assert value, name
    results['checks'].append(name)
    print('PASS', name, flush=True)

def vm(name, request='500m', limit='4'):
    resources = {'requests': {'cpu': request}}
    if limit:
        resources['limits'] = {'cpu': limit}
    return {'apiVersion': 'vm.roamvm.io/v1alpha1', 'kind': 'VirtualMachine', 'metadata': {'name': name},
            'spec': {'powerState': 'Running', 'image': a.image, 'cpus': 4, 'memory': '256Mi',
                     'readinessPort': 8080, 'nodeSelector': {'kubernetes.io/hostname': a.node},
                     'resources': resources}}

def http(name, path, body=None):
    args = ['exec'] + (['-i'] if body is not None else []) + [client, '--', 'curl', '-fsS', '--max-time', '40',
            '--retry', '5', '--retry-connrefused', '--retry-delay', '1']
    if body is not None:
        args += ['--data-binary', '@-']
    return k(*args, f'http://{name}:8080{path}', data=body)

def cgroup(name, filename):
    pod = obj('rvm', name)['status']['podName']
    script = 'for p in /nix/store/*-coreutils-*/bin/cat /bin/cat /usr/bin/cat; do if [ -x "$p" ]; then exec "$p" "$1"; fi; done; exit 1'
    return k('exec', pod, '--', '/bin/sh', '-c', script, 'cat', '/sys/fs/cgroup/' + filename).decode().strip()

def usage(name):
    values = dict(line.split() for line in cgroup(name, 'cpu.stat').splitlines())
    return int(values['usage_usec']) / 1e6

def load(name):
    start_cpu, start = usage(name), time.monotonic()
    data = json.loads(http(name, '/cpu?seconds=10', b''))
    elapsed = time.monotonic() - start
    cores = (usage(name) - start_cpu) / elapsed
    assert data['cpus'] == 4 and all(n > 0 for n in data['iterations']), data
    return {'coresUsed': round(cores, 3), 'seconds': round(elapsed, 3), 'guest': data}

try:
    run(['docker', 'update', '--cpuset-cpus', cpuset, a.node])
    config = original_config + f'\nkubeReserved:\n  cpu: "{capacity - 2}"\n'.encode()
    run(['docker', 'exec', '-i', a.node, 'sh', '-c', 'cat > /var/lib/kubelet/config.yaml'], config)
    run(['docker', 'exec', a.node, 'systemctl', 'restart', 'kubelet'])
    wait_for(lambda: obj('node', a.node)['status']['allocatable']['cpu'] == '2')
    k('wait', 'node/' + a.node, '--for=condition=Ready', '--timeout=60s')
    apply({'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': client}, 'spec': {
        'nodeSelector': {'kubernetes.io/hostname': context.removeprefix('kind-') + '-worker'},
        'automountServiceAccountToken': False, 'containers': [{'name': 'curl', 'image': 'curlimages/curl:8.17.0',
            'command': ['sh', '-c', 'exec sleep 1800']}]}})
    k('wait', 'pod/' + client, '--for=condition=Ready', '--timeout=90s')
    for name in names:
        apply(vm(name))
        apply({'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': name}, 'spec': {
            'selector': {'vm.roamvm.io/name': name}, 'ports': [{'port': 8080}]}})
    for name in names:
        phase(name, 'Running')
        pod = obj('pod', obj('rvm', name)['status']['podName'])
        check(name + ' scheduled normally on the constrained worker', pod['spec']['nodeName'] == a.node
              and pod['spec']['schedulerName'] == 'default-scheduler')
        check(name + ' has shared CPU accounting', pod['status']['qosClass'] == 'Burstable'
              and pod['spec']['containers'][0]['resources']['requests']['cpu'] == '500m')
        check(name + ' runs on only two physical CPU threads', cpu_ids(cgroup(name, 'cpuset.cpus.effective')) == set(cpus[:2]))
        check(name + ' has a four-CPU cgroup ceiling', cgroup(name, 'cpu.max').split() == ['400000', '100000'])
        check(name + ' writes its overlay', http(name, '/data', name.encode()) == name.encode())
    check('eight guest vCPUs run on two host CPU threads', len(names) * 4 > 2)
    results['burst'] = load(names[0])
    check('idle capacity lets one guest burst above its 500m request', results['burst']['coresUsed'] > .75)
    with ThreadPoolExecutor(max_workers=2) as pool:
        results['concurrent'] = list(pool.map(load, names))
    check('both guests make CPU progress under contention', all(x['coresUsed'] > .25 for x in results['concurrent']))
    apply(vm(pending, request='2'))
    phase(pending, 'Pending')
    def insufficient_cpu():
        pod = obj('pod', obj('rvm', pending)['status']['podName'])
        return any(c.get('reason') == 'Unschedulable' and 'Insufficient cpu' in c.get('message', '')
                   for c in pod.get('status', {}).get('conditions', []))
    wait_for(insufficient_cpu)
    check('scheduler still rejects requests exceeding available CPU', True)
    power(pending, 'Stopped')
    phase(pending, 'Stopped')
    for name in names:
        power(name, 'Stopped')
    for name in names:
        phase(name, 'Stopped')
        check(name + ' durably checkpoints after contention', obj('rvm', name)['status']['checkpoint']['generation'] == 1)
    k('patch', 'rvm', names[0], '--type=merge', '-p', json.dumps({'spec': {
        'resources': {'requests': {'cpu': '125m'}, 'limits': {'cpu': '250m'}}}}))
    power(names[0], 'Running')
    phase(names[0], 'Running')
    check('CPU cap applies to the VMM cgroup', cgroup(names[0], 'cpu.max').split() == ['25000', '100000'])
    results['capped'] = load(names[0])
    check('explicit CPU limit throttles the VM', .1 < results['capped']['coresUsed'] < .4)
    for name in names:
        power(name, 'Running')
        phase(name, 'Running')
        check(name + ' restores its own disk bytes', http(name, '/data') == name.encode())
    results['success'] = True
finally:
    try:
        for name in names + [pending]:
            k('delete', 'rvm', name, '--ignore-not-found', '--wait=true', '--timeout=120s')
        k('delete', 'service', *names, '--ignore-not-found')
        k('delete', 'pod', client, '--ignore-not-found', '--wait=false')
    finally:
        run(['docker', 'exec', '-i', a.node, 'sh', '-c', 'cat > /var/lib/kubelet/config.yaml'], original_config)
        run(['docker', 'update', '--cpuset-cpus', original_cpuset, a.node])
        run(['docker', 'exec', a.node, 'systemctl', 'restart', 'kubelet'])
        wait_for(lambda: obj('node', a.node)['status']['allocatable']['cpu'] == str(capacity))
        Path(a.output).parent.mkdir(parents=True, exist_ok=True)
        Path(a.output).write_text(json.dumps(results, indent=2) + '\n')
