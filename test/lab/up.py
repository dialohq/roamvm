#!/usr/bin/env python3
"""Create an isolated, three-node kind lab. Never deletes an existing cluster."""
import argparse
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess as sp
import time

p = argparse.ArgumentParser()
p.add_argument('--reuse', action='store_true', help='reuse this project\'s existing kind cluster and service containers')
p.add_argument('--skip-build', action='store_true', help='use the existing roamvm:dev image')
p.add_argument('--minio-binary', default=shutil.which('minio'), help='local Linux x86-64 MinIO binary (only used by this test lab)')
a = p.parse_args()
root = Path(__file__).resolve().parents[2]
lab = root / '.lab'
lab.mkdir(exist_ok=True)
os.chdir(root)
for binary in ['docker', 'kind', 'kubectl', 'go']:
    assert shutil.which(binary), f'missing {binary}'
assert Path('/dev/kvm').exists(), 'real KVM is required; this lab does not emulate it'

def run(args, **kwargs):
    print('+', shlex.join(map(str, args)), flush=True)
    return sp.check_output(list(map(str, args)), **kwargs)

def exists(container):
    return sp.run(['docker', 'inspect', container], capture_output=True).returncode == 0

clusters = run(['kind', 'get', 'clusters'], text=True).splitlines()
if 'roamvm' in clusters and not a.reuse:
    raise SystemExit('kind cluster roamvm already exists; use --reuse or remove it explicitly')
os.environ['KUBECONFIG'] = str(lab / 'kubeconfig')
if 'roamvm' not in clusters:
    nodes = [{'role': 'control-plane'}]
    nodes += [{'role': 'worker'}, {'role': 'worker'}]
    config = {'kind': 'Cluster', 'apiVersion': 'kind.x-k8s.io/v1alpha4', 'nodes': nodes,
        'kubeadmConfigPatches': ['kind: KubeletConfiguration\nallowedUnsafeSysctls:\n- net.ipv4.ip_forward\n- net.ipv4.conf.all.route_localnet\n']}
    (lab / 'kind.json').write_text(json.dumps(config))
    run(['kind', 'create', 'cluster', '--name', 'roamvm', '--image', 'kindest/node:v1.35.0', '--config', lab / 'kind.json', '--kubeconfig', lab / 'kubeconfig'])
else:
    (lab / 'kubeconfig').write_bytes(run(['kind', 'get', 'kubeconfig', '--name', 'roamvm']))
assert run(['kubectl', 'config', 'current-context']).strip() == b'kind-roamvm'
for worker in ['roamvm-worker', 'roamvm-worker2']:
    run(['kubectl', 'label', 'node', worker, 'vm.roamvm.io/enabled=true', '--overwrite'])

if not exists('roamvm-registry'):
    run(['docker', 'run', '-d', '--name', 'roamvm-registry', '--network', 'kind', '-p', '127.0.0.1:15000:5000', 'registry:3.0.0'])
if not exists('roamvm-s3'):
    assert a.minio_binary, 'provide --minio-binary; use a version supporting conditional multipart completion'
    binary = Path(a.minio_binary).resolve()
    data = lab / 's3'
    data.mkdir(exist_ok=True)
    args = ['docker', 'run', '-d', '--name', 'roamvm-s3', '--network', 'kind', '-p', '127.0.0.1:19000:9000',
        '-v', f'{binary}:/usr/local/bin/minio:ro', '-v', f'{data}:/data',
        '-e', 'MINIO_ROOT_USER=roamvm-local', '-e', 'MINIO_ROOT_PASSWORD=roamvm-local-test-only']
    if str(binary).startswith('/nix/store/'):
        args += ['-v', '/nix/store:/nix/store:ro']
    run(args + ['alpine:3.23', '/usr/local/bin/minio', 'server', '/data', '--address', ':9000'])

def address(container):
    return run(['docker', 'inspect', '--format', '{{(index .NetworkSettings.Networks "kind").IPAddress}}', container], text=True).strip()
registry = address('roamvm-registry') + ':5000'
s3 = 'http://' + address('roamvm-s3') + ':9000'
settings = {'KUBECONFIG': str(lab / 'kubeconfig'), 'S3_ENDPOINT': 'http://127.0.0.1:19000',
    'S3_BUCKET': 'roamvm', 'AWS_ACCESS_KEY_ID': 'roamvm-local', 'AWS_SECRET_ACCESS_KEY': 'roamvm-local-test-only',
    'TEST_S3_ENDPOINT': 'http://127.0.0.1:19000', 'TEST_S3_BUCKET': 'roamvm', 'LAB_REGISTRY': registry}
os.environ.update(settings)
(lab / 'env').write_text('\n'.join('export ' + k + '=' + shlex.quote(v) for k, v in settings.items()) + '\n')
run(['make', 'build'])
for attempt in range(20):
    try:
        run([root / 'bin/lab-tool', 'init'])
        break
    except sp.CalledProcessError:
        if attempt == 19:
            raise
        time.sleep(1)
if not a.skip_build:
    run(['docker', 'build', '-t', 'roamvm:dev', '.'])
run(['kind', 'load', 'docker-image', 'roamvm:dev', '--name', 'roamvm'])
run(['kubectl', 'apply', '--server-side', '-f', 'config/crd'])
namespace = {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': 'roamvm-system'}}
run(['kubectl', 'apply', '-f', '-'], input=json.dumps(namespace).encode())
secret = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'roamvm-object-store', 'namespace': 'default'},
    'stringData': {'S3_ENDPOINT': s3, 'S3_BUCKET': 'roamvm', 'AWS_ACCESS_KEY_ID': 'roamvm-local',
        'AWS_SECRET_ACCESS_KEY': 'roamvm-local-test-only'}}
run(['kubectl', 'apply', '-f', '-'], input=json.dumps(secret).encode())
run(['kubectl', 'apply', '-f', 'config/install.yaml'])
for node in ['roamvm-worker', 'roamvm-worker2']:
    run(['docker', 'exec', node, 'test', '-c', '/dev/kvm'])
    run(['docker', 'cp', root / 'bin/roamvm', node + ':/usr/local/bin/roamvm'])
    run(['docker', 'cp', root / 'config/roamvm-device-plugin.service', node + ':/etc/systemd/system/roamvm-device-plugin.service'])
    run(['docker', 'exec', node, 'systemctl', 'daemon-reload'])
    run(['docker', 'exec', node, 'systemctl', 'enable', '--now', 'roamvm-device-plugin'])
    run(['docker', 'exec', node, 'mkdir', '-p', '/etc/containerd/certs.d/' + registry])
    hosts = f'server = "http://{registry}"\n[host."http://{registry}"]\n  capabilities = ["pull", "resolve"]\n'
    run(['docker', 'exec', '-i', node, 'sh', '-c', 'cat > ' + shlex.quote('/etc/containerd/certs.d/' + registry + '/hosts.toml')], input=hosts.encode())
run(['kubectl', '-n', 'roamvm-system', 'rollout', 'restart', 'deployment/controller'])
run(['kubectl', '-n', 'roamvm-system', 'rollout', 'status', 'deployment/controller', '--timeout=120s'])
print(f'Lab ready. source {lab}/env; export PATH={root}/bin:$PATH', flush=True)
