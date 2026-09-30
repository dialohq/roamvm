#!/usr/bin/env python3
"""Destructive only to this project's named local kind cluster and test VMs."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess as sp
import time
import urllib.parse

p = argparse.ArgumentParser()
p.add_argument('--image', required=True)
p.add_argument('--output', default='test-results/e2e.json')
p.add_argument('--cycles', type=int, default=3)
p.add_argument('--lab-tool', required=True)
p.add_argument('--cli', default='roamvm')
p.add_argument('--node-failure', action='store_true', help='kill and restart one kind worker container')
p.add_argument('--object-store-container', default='roamvm-s3')
a = p.parse_args()

def run(args, data=None, check=True):
    result=sp.run(args, input=data, stdout=sp.PIPE, stderr=sp.PIPE)
    if check and result.returncode:
        raise RuntimeError(f'{args}: {result.stderr.decode(errors="replace")[-4000:]}')
    return result.stdout

def k(*args, data=None):
    return run(['kubectl', *args], data)

assert k('config', 'current-context').strip() == b'kind-roamvm', 'refusing to touch another cluster'
name = 'e2e-' + str(int(time.time()))
network_client = name + '-client'
results = {'vm': name, 'image': a.image, 'checks': [], 'timings': []}

def check(name, condition):
    assert condition, name
    results['checks'].append(name)
    print('PASS', name, flush=True)

def vm():
    return json.loads(k('get', 'rvm', name, '-o', 'json'))

def wait(phase, timeout=90):
    start = time.monotonic()
    while time.monotonic() - start < timeout:
        obj = vm()
        if obj.get('status', {}).get('phase') == phase:
            return obj, time.monotonic() - start
        time.sleep(.2)
    raise AssertionError(f'timed out waiting for {phase}: {obj.get("status")}')

def apply(obj):
    k('apply', '-f', '-', data=json.dumps(obj).encode())

def power(state):
    k('patch', 'rvm', name, '--type=merge', '-p', json.dumps({'spec': {'powerState': state}}))

def http(path, data=None):
    cmd = ['kubectl', 'exec']
    if data is not None:
        cmd.append('-i')
    cmd += [network_client, '--', 'curl', '-fsS', '--max-time', '10']
    if data is not None:
        cmd += ['--data-binary', '@-']
    cmd += [f'http://{name}.default.svc.cluster.local:8080{path}']
    return run(cmd, data)

def reachable(timeout=15):
    start = time.monotonic()
    while time.monotonic() - start < timeout:
        try:
            if http('/ready') == b'ready\n':
                return
        except RuntimeError:
            pass
        time.sleep(.2)
    raise AssertionError('Service did not become reachable')

def head():
    return json.loads(run([a.lab_tool, 'read', uid]))

def stop():
    start = time.monotonic()
    power('Stopped')
    result, _ = wait('Stopped')
    elapsed = time.monotonic() - start
    h = head()
    check('Stopped is durably committed', h['state'] == 'Stopped' and not h.get('owner'))
    check('Kubernetes checkpoint agrees with S3', result['status']['checkpoint'] == h['checkpoint'])
    results['timings'].append({'operation': 'stop', 'seconds': round(elapsed, 3), 'checkpointBytes': h['checkpoint']['size']})
    return h

def start():
    before = time.monotonic()
    power('Running')
    result, _ = wait('Running')
    reachable()
    results['timings'].append({'operation': 'start', 'seconds': round(time.monotonic() - before, 3), 'node': result['status']['nodeName']})
    return result

try:
    apply({'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': network_client}, 'spec': {
        'nodeSelector': {'kubernetes.io/hostname': 'roamvm-control-plane'},
        'tolerations': [{'key': 'node-role.kubernetes.io/control-plane', 'operator': 'Exists', 'effect': 'NoSchedule'}],
        'automountServiceAccountToken': False,
        'containers': [{'name': 'curl', 'image': 'curlimages/curl:8.17.0', 'command': ['sh', '-c', 'exec sleep 3600']}]}})
    k('wait', 'pod/' + network_client, '--for=condition=Ready', '--timeout=90s')
    before = time.monotonic()
    apply({'apiVersion': 'vm.roamvm.io/v1alpha1', 'kind': 'VirtualMachine', 'metadata': {'name': name}, 'spec': {
        'powerState': 'Running', 'image': a.image, 'cpus': 2, 'memory': '512Mi', 'readinessPort': 8080,
        'resources': {'requests': {'cpu': '250m'}}}})
    apply({'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': name}, 'spec': {
        'selector': {'vm.roamvm.io/name': name}, 'ports': [{'port': 8080, 'targetPort': 8080}]}})
    obj, _ = wait('Running')
    uid = obj['metadata']['uid']
    reachable()
    results['timings'].append({'operation': 'first-start', 'seconds': round(time.monotonic() - before, 3), 'node': obj['status']['nodeName']})
    old_node = obj['status']['nodeName']
    pod = json.loads(k('get', 'pod', obj['status']['podName'], '-o', 'json'))
    check('ordinary scheduler selected node', pod['spec'].get('schedulerName', 'default-scheduler') == 'default-scheduler')
    check('VM Pod has no hostPath volumes', not any('hostPath' in v for v in pod['spec']['volumes']))
    check('root uses an ephemeral PVC', any(v['name'] == 'working' and 'ephemeral' in v for v in pod['spec']['volumes']))
    claim = pod['metadata']['name'] + '-working'
    pvc = json.loads(k('get', 'pvc', claim, '-o', 'json'))
    check('working PVC belongs to this Pod incarnation', pvc['metadata']['ownerReferences'][0]['uid'] == pod['metadata']['uid'])
    check('overcommitted CPU request preserved', pod['spec']['containers'][0]['resources']['requests']['cpu'] == '250m')
    check('guest DNS reaches Kubernetes service', len(json.loads(http('/dns?name=kubernetes.default.svc.cluster.local'))) > 0)
    payload = b'project files and installed packages\n' + os.urandom(1024 * 1024)
    check('guest write via Service', http('/data', payload) == payload)
    first = stop()
    check('overlay is much smaller than base virtual size', first['checkpoint']['size'] < 8 << 20)
    k('wait', 'pod/' + pod['metadata']['name'], '--for=delete', '--timeout=30s')
    k('wait', 'pvc/' + claim, '--for=delete', '--timeout=30s')
    check('working PVC removed after durable stop', True)
    k('cordon', old_node)
    moved = start()
    check('restart scheduled on another node', moved['status']['nodeName'] != old_node)
    check('all bytes survived cross-node restore', http('/data') == payload)
    k('uncordon', old_node)

    # Native sidecar restart must retain the running VMM and its acquired epoch.
    runtime_pod = moved['status']['podName']
    before = json.loads(k('get', 'pod', runtime_pod, '-o', 'json'))['status']['initContainerStatuses'][0]['restartCount']
    k('exec', runtime_pod, '-c', 'runtime', '--', '/bin/sh', '-c', 'kill -TERM 1')
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        statuses = json.loads(k('get', 'pod', runtime_pod, '-o', 'json'))['status'].get('initContainerStatuses', [])
        if statuses and statuses[0]['restartCount'] > before and statuses[0]['state'].get('running'):
            break
        time.sleep(.2)
    else:
        raise AssertionError('runtime sidecar did not restart')
    reachable()
    check('runtime sidecar restart preserves running VM', http('/data') == payload)
    containers = json.loads(k('get', 'pod', runtime_pod, '-o', 'json'))['status']['containerStatuses']
    check('sidecar restart did not restart the VMM container', containers[0]['restartCount'] == 0)

    # Pause S3 during stop. A paused test server is a real network failure, not a
    # mocked successful PUT. The working copy and runner must remain available.
    run(['docker', 'pause', a.object_store_container])
    try:
        power('Stopped')
        wait('Checkpointing', timeout=45)
        time.sleep(3)
        check('S3 outage never reports Stopped', vm()['status']['phase'] == 'Checkpointing')
        check('runner retained during failed upload', bool(k('get', 'pods', '-l', f'vm.roamvm.io/uid={uid}', '-o', 'name').strip()))
    finally:
        run(['docker', 'unpause', a.object_store_container])
    wait('Stopped', timeout=90)
    check('upload retries commit exactly one generation', head()['checkpoint']['generation'] == first['checkpoint']['generation'] + 1)
    start()
    check('state survives interrupted upload', http('/data') == payload)

    for i in range(a.cycles):
        payload += f'\ncycle={i}'.encode()
        check(f'write cycle {i}', http('/data', payload) == payload)
        stop()
        start()
        check(f'cold restart preserves cycle {i}', http('/data') == payload)

    if a.node_failure:
        obj = vm()
        node = obj['status']['nodeName']
        pod_uid = json.loads(k('get', 'pod', obj['status']['podName'], '-o', 'json'))['metadata']['uid']
        durable = head()['checkpoint']
        http('/data', b'uncommitted data that node failure may discard')
        k('cordon', node)
        # SIGKILL of the kind node container fences all its VMM processes.
        run(['docker', 'stop', '--time', '0', node])
        try:
            check('node death preserves previous durable generation', head()['checkpoint'] == durable)
            check('node death never releases ownership automatically', head()['owner'] == pod_uid)
            run([a.cli, 'recover', '--vm-id', uid, '--owner', pod_uid, '--fenced'])
            wait('Stopped')
            recovered = start()
            check('fenced-node recovery schedules elsewhere', recovered['status']['nodeName'] != node)
            check('node recovery discards only uncommitted changes', http('/data') == payload)
        finally:
            run(['docker', 'start', node])
            k('wait', 'node/' + node, '--for=condition=Ready', '--timeout=120s')
            k('uncordon', node)

    # An exited VMM may not publish a checkpoint or trigger ownership takeover.
    before_failure = head()['checkpoint']
    obj = vm()
    pod_name = obj['status']['podName']
    pod_uid = json.loads(k('get', 'pod', pod_name, '-o', 'json'))['metadata']['uid']
    k('exec', pod_name, '--', '/bin/sh', '-ec', '''
        for process in /proc/[0-9]*; do
            read -r name < "$process/comm" || continue
            case "$name" in cloud-hypervis*) kill -KILL "${process##*/}"; exit 0;; esac
        done
        exit 1
    ''')
    wait('RecoveryRequired')
    check('crash retains last checkpoint', head()['checkpoint'] == before_failure)
    check('crash does not release ownership', head()['owner'] == pod_uid)
    # The VMM has just been killed and the Pod has terminated: fencing is proven.
    run([a.cli, 'recover', '--vm-id', uid, '--owner', pod_uid, '--fenced'])
    wait('Stopped')
    start()
    check('explicit recovery restores last durable bytes', http('/data') == payload)
    stop()

    # Corrupt the object as an administrator, bypassing the runtime's immutable
    # write path. The restored VM must fail closed before launching a VMM.
    backup = str(Path(a.output).resolve().parent / 'checkpoint-backup.qcow2')
    Path(backup).parent.mkdir(parents=True, exist_ok=True)
    run([a.lab_tool, 'corrupt', uid, backup])
    power('Running')
    wait('RecoveryRequired')
    obj = vm()
    log = k('logs', obj['status']['podName']).decode()
    check('corrupt checkpoint rejected before guest boot', 'integrity mismatch' in log and 'Linux version' not in log)
    failed_uid = head()['owner']
    run([a.lab_tool, 'restore', uid, backup])
    run([a.cli, 'recover', '--vm-id', uid, '--owner', failed_uid, '--fenced'])
    Path(backup).unlink()
    start()
    check('verified checkpoint can be restored after repair', http('/data') == payload)
    stop()
    results['payloadSHA256'] = hashlib.sha256(payload).hexdigest()
    results['finalCheckpoint'] = head()['checkpoint']
    results['success'] = True
finally:
    run(['kubectl', 'delete', 'pod', network_client, '--ignore-not-found', '--wait=false'], check=False)
    for node in ['roamvm-worker', 'roamvm-worker2']:
        run(['kubectl', 'uncordon', node], check=False)
    Path(a.output).parent.mkdir(parents=True, exist_ok=True)
    Path(a.output).write_text(json.dumps(results, indent=2) + '\n')
    print('Results:', a.output, flush=True)
