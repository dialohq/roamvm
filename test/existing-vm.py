#!/usr/bin/env python3
"""Boot an existing SSH-enabled VM twice and verify its persistent host identity."""
import argparse
import json
from pathlib import Path
import re
import subprocess as sp
import tempfile
import time

p = argparse.ArgumentParser()
p.add_argument('--vm', required=True)
p.add_argument('--output', default='test-results/existing-vm.json')
a = p.parse_args()

def k(*args):
    return sp.check_output(['kubectl', *args])

assert k('config', 'current-context').strip() == b'kind-roamvm'
initial = json.loads(k('get', 'rvm', a.vm, '-o', 'json'))
assert initial['spec']['powerState'] == 'Stopped', 'test starts from a stopped VM'
result = {'vm': a.vm, 'image': initial['spec']['image'], 'timings': []}
keys = []
for cycle in range(2):
    start = time.monotonic()
    k('patch', 'rvm', a.vm, '--type=merge', '-p', '{"spec":{"powerState":"Running"}}')
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        vm = json.loads(k('get', 'rvm', a.vm, '-o', 'json'))
        ready = any(c['type'] == 'Ready' and c['status'] == 'True' for c in vm.get('status', {}).get('conditions', []))
        if ready:
            break
        time.sleep(.2)
    assert ready, vm
    result['timings'].append({'operation': 'start-to-SSH-ready', 'seconds': round(time.monotonic()-start, 3), 'node': vm['status']['nodeName']})
    with tempfile.TemporaryFile() as log:
        pf = sp.Popen(['kubectl', 'port-forward', '--address=127.0.0.1', 'pod/'+vm['status']['podName'], ':22'], stdout=log, stderr=log)
        try:
            for _ in range(60):
                log.seek(0)
                match = re.search(rb'127\.0\.0\.1:(\d+) ->', log.read())
                if match:
                    break
                time.sleep(.2)
            assert match, 'port-forward did not start'
            scan = sp.check_output(['ssh-keyscan', '-T', '5', '-t', 'ed25519', '-p', match[1].decode(), '127.0.0.1'], stderr=sp.DEVNULL).decode()
            identity = [l.split()[1:] for l in scan.splitlines() if 'ssh-ed25519' in l]
            assert identity, scan
            keys.append(identity)
        finally:
            pf.terminate()
            pf.wait(timeout=5)
    start = time.monotonic()
    k('patch', 'rvm', a.vm, '--type=merge', '-p', '{"spec":{"powerState":"Stopped"}}')
    k('wait', 'rvm/'+a.vm, '--for=condition=Stopped', '--timeout=180s')
    vm = json.loads(k('get', 'rvm', a.vm, '-o', 'json'))
    result['timings'].append({'operation': 'durable-stop', 'seconds': round(time.monotonic()-start, 3), 'checkpointBytes': vm['status']['checkpoint']['size']})
    print('PASS boot, SSH through port-forward, durable stop', cycle, flush=True)
assert keys[0] == keys[1], 'SSH identity changed across restore'
result['checks'] = ['real image boots', 'SSH reachable through Kubernetes port forwarding', 'graceful shutdown commits checkpoint', 'SSH host identity persists across stop/restore']
result['success'] = True
Path(a.output).parent.mkdir(parents=True, exist_ok=True)
Path(a.output).write_text(json.dumps(result, indent=2)+'\n')
print('PASS persistent SSH identity', flush=True)
