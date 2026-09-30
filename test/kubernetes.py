#!/usr/bin/env python3
"""API, Service, port-forward, configuration, PVC and deletion integration tests."""

import argparse
import json
import re
import subprocess as sp
import tempfile
import time
import urllib.request
from pathlib import Path

from support import apply, power
from support import command as run
from support import kubectl as k
from support import wait_phase as wait

p = argparse.ArgumentParser()
p.add_argument("--image", required=True)
p.add_argument("--lab-tool", required=True)
p.add_argument("--output", default="test-results/kubernetes.json")
p.add_argument(
    "--check-network-policy",
    action="store_true",
    help="requires a CNI that enforces NetworkPolicy",
)
a = p.parse_args()
checks = []


def check(name, value):
    assert value, name
    checks.append(name)
    print("PASS", name, flush=True)


def get(name):
    return json.loads(k("get", "rvm", name, "-o", "json").stdout)


def spec(name, **extra):
    return {
        "apiVersion": "vm.roamvm.io/v1alpha1",
        "kind": "VirtualMachine",
        "metadata": {"name": name},
        "spec": {
            "powerState": "Running",
            "image": a.image,
            "cpus": 1,
            "memory": "256Mi",
            "readinessPort": 8080,
            **extra,
        },
    }


assert k("config", "current-context").stdout.strip() in {
    b"kind-roamvm",
    b"kind-roamvm-cilium",
}
name = "integration-" + str(int(time.time()))
result = {"vm": name, "image": a.image, "checks": checks}
try:
    pending = name + "-pending"
    apply(spec(pending, nodeSelector={"roamvm.test/nonexistent": "true"}))
    obj = wait(pending, "Pending")
    k(
        "wait",
        "pod",
        "-l",
        f"vm.roamvm.io/name={pending}",
        "--for=jsonpath={.status.phase}=Pending",
        "--timeout=20s",
    )
    power(pending, "Stopped")
    wait(pending, "Stopped")
    check(
        "unschedulable VM cancels without fencing or creating state",
        not k(
            "get", "pods", "-l", f"vm.roamvm.io/name={pending}", "-o", "name"
        ).stdout.strip(),
    )
    k("delete", "rvm", pending, "--wait=true", "--timeout=20s")

    bad = spec(name + "-unpinned")
    bad["spec"]["image"] = "registry.invalid/devbox:latest"
    check(
        "CRD rejects mutable image references",
        k("apply", "-f", "-", data=json.dumps(bad).encode(), check=False).returncode
        != 0,
    )
    config_disks = [
        {
            "name": "agent",
            "label": "TEST_AGENT",
            "projection": {"sources": [{"secret": {"name": name}}]},
        },
        {
            "name": "tool",
            "label": "TEST_TOOL",
            "projection": {"sources": [{"configMap": {"name": name}}]},
        },
    ]
    bad = spec(name + "-duplicate-label", configDisks=config_disks)
    bad = json.loads(json.dumps(bad))
    bad["spec"]["configDisks"][1]["label"] = "TEST_AGENT"
    check(
        "CRD rejects ambiguous configuration disk labels",
        k(
            "apply",
            "--dry-run=server",
            "-f",
            "-",
            data=json.dumps(bad).encode(),
            check=False,
        ).returncode
        != 0,
    )
    apply(
        {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": name},
            "data": {"setting": "first"},
        }
    )
    apply(
        {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {"name": name},
            "stringData": {"credential": "local-fixture-only"},
        }
    )
    network_client = name + "-client"
    apply(
        {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {
                "name": network_client,
                "labels": {"roamvm.test/client": name},
            },
            "spec": {
                "containers": [
                    {
                        "name": "curl",
                        "image": "curlimages/curl:8.17.0",
                        "command": ["sh", "-c", "exec sleep 1800"],
                    }
                ],
                "automountServiceAccountToken": False,
            },
        }
    )
    k("wait", "pod/" + network_client, "--for=condition=Ready", "--timeout=90s")
    apply(
        {
            "apiVersion": "v1",
            "kind": "PersistentVolumeClaim",
            "metadata": {"name": name},
            "spec": {
                "accessModes": ["ReadWriteOnce"],
                "resources": {"requests": {"storage": "256Mi"}},
            },
        }
    )
    helper = name + "-prepare"
    apply(
        {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {"name": helper},
            "spec": {
                "restartPolicy": "Never",
                "containers": [
                    {
                        "name": "prepare",
                        "image": "alpine:3.23",
                        "command": [
                            "sh",
                            "-ec",
                            "apk add --no-cache e2fsprogs; mkdir /tmp/root; printf prepared-by-kubernetes > /tmp/root/marker; "
                            "truncate -s 64M /volume/disk.img; mke2fs -q -t ext4 -F -d /tmp/root /volume/disk.img; touch /tmp/ready; exec sleep 600",
                        ],
                        "readinessProbe": {
                            "exec": {"command": ["sh", "-c", "test -f /tmp/ready"]},
                            "periodSeconds": 1,
                        },
                        "volumeMounts": [{"name": "disk", "mountPath": "/volume"}],
                    }
                ],
                "volumes": [
                    {"name": "disk", "persistentVolumeClaim": {"claimName": name}}
                ],
            },
        }
    )
    k("wait", "pod/" + helper, "--for=condition=Ready", "--timeout=90s")
    k("delete", "pod", helper, "--grace-period=1", "--wait=true", "--timeout=30s")
    vm_spec = spec(
        name,
        resources={"limits": {"cpu": 2}},
        configDisks=config_disks,
        disks=[{"name": "workspace", "claimName": name, "volumeMode": "Filesystem"}],
        config={"sources": [{"configMap": {"name": name}}, {"secret": {"name": name}}]},
    )
    k(
        "apply",
        "--server-side",
        "--field-manager=roamvm-test",
        "-f",
        "-",
        data=json.dumps(vm_spec).encode(),
    )
    apply(
        {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {"name": name},
            "spec": {
                "selector": {"vm.roamvm.io/name": name},
                "ports": [{"port": 8080}],
            },
        }
    )
    obj = wait(name, "Running")
    uid = obj["metadata"]["uid"]
    check(
        "controller finalizer preserves numeric quantities and declarative field ownership",
        k(
            "apply",
            "--server-side",
            "--dry-run=server",
            "--field-manager=roamvm-test",
            "-f",
            "-",
            data=json.dumps(vm_spec).encode(),
            check=False,
        ).returncode
        == 0,
    )
    k("wait", "rvm/" + name, "--for=condition=Ready", "--timeout=30s")

    def http(path, body=None):
        args = ["exec"]
        if body is not None:
            args.append("-i")
        args += [
            network_client,
            "--",
            "curl",
            "-fsS",
            "--max-time",
            "10",
            "--retry",
            "10",
            "--retry-connrefused",
            "--retry-delay",
            "1",
        ]
        if body is not None:
            args += ["--data-binary", "@-"]
        return k(
            *args, f"http://{name}.default.svc.cluster.local:8080{path}", data=body
        ).stdout

    check(
        "ConfigMap and Secret delivered to guest ISO",
        json.loads(http("/config"))
        == {"setting": "first", "credential": "local-fixture-only"},
    )
    check(
        "labelled configuration disks stay distinct and read-only",
        json.loads(http("/config-disks"))
        == {"agent": "local-fixture-only", "tool": "first"},
    )
    check(
        "secondary PVC disk is mounted in guest",
        http("/secondary") == b"prepared-by-kubernetes",
    )
    check(
        "guest writes secondary PVC",
        http("/secondary", b"guest-pvc-write") == b"guest-pvc-write",
    )
    if a.check_network_policy:
        policy = {
            "apiVersion": "networking.k8s.io/v1",
            "kind": "NetworkPolicy",
            "metadata": {"name": name},
            "spec": {
                "podSelector": {"matchLabels": {"vm.roamvm.io/name": name}},
                "policyTypes": ["Ingress"],
                "ingress": [],
            },
        }
        apply(policy)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            denied = (
                k(
                    "exec",
                    network_client,
                    "--",
                    "curl",
                    "-fsS",
                    "--max-time",
                    "2",
                    f"http://{name}.default.svc.cluster.local:8080/ready",
                    check=False,
                ).returncode
                != 0
            )
            if denied:
                break
            time.sleep(0.5)
        check("CNI NetworkPolicy blocks guest ingress", denied)
        policy["spec"]["ingress"] = [
            {"from": [{"podSelector": {"matchLabels": {"roamvm.test/client": name}}}]}
        ]
        apply(policy)
        deadline = time.monotonic() + 20
        while True:
            try:
                check(
                    "CNI NetworkPolicy allows the selected client",
                    http("/ready") == b"ready\n",
                )
                break
            except RuntimeError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.5)
    patch = {"spec": {"image": a.image.split("@")[0] + "@sha256:" + "0" * 64}}
    check(
        "CRD rejects changing base under a checkpoint",
        k(
            "patch", "rvm", name, "--type=merge", "-p", json.dumps(patch), check=False
        ).returncode
        != 0,
    )
    with tempfile.TemporaryFile() as log:
        pf = sp.Popen(
            [
                "kubectl",
                "port-forward",
                "--address=127.0.0.1",
                "pod/" + obj["status"]["podName"],
                ":8080",
            ],
            stdout=log,
            stderr=log,
        )
        try:
            reached = False
            for _ in range(40):
                log.seek(0)
                match = re.search(rb"127\.0\.0\.1:(\d+) ->", log.read())
                if not match:
                    time.sleep(0.25)
                    continue
                port = int(match[1])
                try:
                    with urllib.request.urlopen(
                        f"http://127.0.0.1:{port}/ready", timeout=1
                    ) as res:
                        reached = res.read() == b"ready\n"
                    if reached:
                        break
                except OSError:
                    time.sleep(0.25)
            if not reached:
                log.seek(0)
                print(log.read().decode(), flush=True)
            check("kubectl port-forward reaches the guest", reached)
        finally:
            pf.terminate()
            pf.wait(timeout=5)
    k("patch", "configmap", name, "--type=merge", "-p", '{"data":{"setting":"second"}}')
    check(
        "configuration remains a boot-time snapshot",
        json.loads(http("/config"))["setting"] == "first",
    )
    power(name, "Stopped")
    wait(name, "Stopped")
    power(name, "Running")
    wait(name, "Running")
    k("wait", "rvm/" + name, "--for=condition=Ready", "--timeout=30s")
    check(
        "updated configuration appears on next boot",
        json.loads(http("/config"))["setting"] == "second",
    )
    check(
        "secondary PVC survives root checkpoint cycle",
        http("/secondary") == b"guest-pvc-write",
    )
    http("/data", b"finalizer-commits-this")
    k("delete", "rvm", name, "--wait=true", "--timeout=100s")
    head = json.loads(run([a.lab_tool, "read", uid]).stdout)
    check(
        "VM deletion waits for durable checkpoint",
        head["state"] == "Stopped"
        and head["checkpoint"]["generation"] == 2
        and not head.get("owner"),
    )
    check(
        "deleting VM preserves independently owned PVC",
        k("get", "pvc", name, check=False).returncode == 0,
    )
    k("delete", "pvc,configmap,secret,service", name, "--wait=true", "--timeout=30s")
    k(
        "delete",
        "pod",
        network_client,
        "--grace-period=1",
        "--wait=true",
        "--timeout=30s",
    )
    if a.check_network_policy:
        k("delete", "networkpolicy", name)
    result["success"] = True
finally:
    Path(a.output).parent.mkdir(parents=True, exist_ok=True)
    Path(a.output).write_text(json.dumps(result, indent=2) + "\n")
