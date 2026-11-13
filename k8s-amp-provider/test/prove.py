"""Run from the repository root; see k8s-amp-provider/README.md for setup."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import time

from mock_amp import AmpMock

root = Path.cwd()
if os.environ.get("ROAMVM_TEST_ORBS") != "1":
    raise RuntimeError("set ROAMVM_TEST_ORBS=1 for the disposable lab")
if (
    subprocess.check_output(["kubectl", "config", "current-context"]).decode().strip()
    != "roamvm-libvirt"
):
    raise RuntimeError("refusing a non-lab Kubernetes context")
mock = AmpMock("192.168.124.1")
runtime = os.environ["RUNTIME_BIN"]
amp = os.environ["AMP_BIN"]
image = os.environ["ROAMVM_TEST_ORB_IMAGE"]
orb = "mock-orb-" + str(time.time_ns())
snapshot_id = orb + "-project"
kind = "rvm2"


def name(prefix, identity):
    return prefix + "-" + hashlib.sha256(identity.encode()).hexdigest()[:40]


def kubectl(*args, body=None):
    return subprocess.check_output(
        ["kubectl", *args],
        input=None if body is None else json.dumps(body).encode(),
        stderr=subprocess.PIPE,
    )


def get(vm):
    return json.loads(kubectl("get", kind, vm, "-o", "json"))


def wait(predicate, timeout=180):
    until = time.monotonic() + timeout
    while time.monotonic() < until:
        if predicate():
            return
        time.sleep(0.2)
    raise TimeoutError("orb state observation timed out")


# The disposable cluster has 2 GiB nodes. Exercise real guest/bootstrap and
# provider resume on a 512 MiB fixture; production profile mapping has unit tests.
def fixture(identity, clone=None):
    spec = dict(
        powerState="Stopped",
        image=image,
        cpus=2,
        memory="512Mi",
        rootDiskSize="64Gi",
        hibernate=True,
        guestAgent=True,
        readinessPort=22,
        shutdownTimeoutSeconds=120,
        hostname=name("orb", identity),
        nodeSelector={"kubernetes.io/hostname": "roamvm-libvirt-worker"},
        resources={"requests": {"cpu": "250m"}},
    )
    if clone:
        spec["cloneFrom"] = clone
    kubectl(
        "create",
        "-f",
        "-",
        body={
            "apiVersion": "v2.vm.roamvm.io/v1alpha1",
            "kind": "VirtualMachine",
            "metadata": {
                "name": name("orb", identity),
                "namespace": "default",
                "annotations": {
                    "amp.roamvm.io/identity": "orb:" + identity,
                    "amp.roamvm.io/type": "small",
                },
            },
            "spec": spec,
        },
    )


kubectl("apply", "-f", "k8s-amp-provider/rbac.yaml")
credentials = json.loads(kubectl("config", "view", "--raw", "--flatten", "-o", "json"))
token = (
    kubectl("create", "token", "roamvm-orb-provider", "--duration=30m").decode().strip()
)
credentials["users"] = [{"name": "provider", "user": {"token": token}}]
for context in credentials["contexts"]:
    context["context"]["user"] = "provider"
config = root / ".amp/in/provider-kubeconfig.json"
config.write_text(json.dumps(credentials))
config.chmod(0o600)
fixture(orb)
env = dict(
    os.environ,
    KUBECONFIG=str(config),
    AMP_ORB_PROVIDER_TOKEN=mock.provider_token,
    AMP_SKIP_UPDATE_CHECK="1",
    ORB_VM_IMAGE=image,
    PATH=str(Path(runtime).parent) + ":" + os.environ["PATH"],
)
log = open(root / ".amp/in/provider-mock.log", "w")
provider = subprocess.Popen(
    [
        amp,
        "--log-file",
        str(root / ".amp/in/amp-mock.log"),
        "orb",
        "provider",
        "--amp-url",
        mock.url,
        str(root / "k8s-amp-provider/dispatch.sh"),
    ],
    env=env,
    stdout=log,
    stderr=log,
)


def job(operation, identity, **fields):
    started = time.monotonic()
    if operation == "spawn":
        fields["token"] = "synthetic-" + str(time.time_ns())
        fields["ampURL"] = mock.url
    result = mock.result(mock.submit(operation, identity, **fields))
    assert not result.get("error"), result
    if operation == "spawn":
        digest = hashlib.sha256(("Bearer " + fields["token"]).encode()).hexdigest()
        mock.wait(lambda: digest in mock.bootstrap, timeout=45)
        assert get(name("orb", identity))["status"]["phase"] == "Running"
    print(
        json.dumps(
            {
                "operation": operation,
                "orb": identity,
                "seconds": round(time.monotonic() - started, 3),
            }
        ),
        flush=True,
    )


try:
    job("spawn", orb)
    job("pause", orb, snapshotId=snapshot_id)
    snapshot = get(name("snapshot", snapshot_id))
    assert snapshot["status"]["checkpoint"].get("hibernation") is None
    assert snapshot["status"]["checkpoint"]["key"].startswith(
        "vm/" + snapshot["metadata"]["uid"] + "/"
    )
    job("spawn", orb)
    job("pause", orb)
    wait(lambda: get(name("orb", orb))["status"].get("local", {}).get("durable"))
    obj = get(name("orb", orb))
    obj["status"]["local"]["stoppedAt"] = "2026-01-01T00:00:00Z"
    kubectl("replace", "--subresource=status", "-f", "-", body=obj)
    wait(lambda: get(name("orb", orb))["status"].get("local") is None)
    job("spawn", orb)
    job("delete", orb)
    assert (
        get(name("snapshot", snapshot_id))["metadata"]["uid"]
        == snapshot["metadata"]["uid"]
    )
    clone = orb + "-clone"
    fixture(
        clone,
        {"name": snapshot["metadata"]["name"], "uid": snapshot["metadata"]["uid"]},
    )
    job("spawn", clone)
    job("delete", clone)
    job("delete", clone)
    kubectl("delete", kind, snapshot["metadata"]["name"], "--timeout=180s")
    print(
        "PASS: real Amp provider, restricted RBAC, QGA credential delivery, warm/cold resume, independent project snapshot and clone, idempotent deletion. Mock deliberately stops guest bootstrap at authenticated discovery (401).",
        flush=True,
    )
finally:
    provider.terminate()
    try:
        provider.wait(timeout=15)
    except subprocess.TimeoutExpired:
        provider.kill()
        provider.wait()
    log.close()
    mock.close()
    config.unlink(missing_ok=True)
