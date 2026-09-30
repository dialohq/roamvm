"""Shared command and polling helpers for the local integration suites."""

import json
import subprocess
import time


def command(args, data=None, check=True):
    result = subprocess.run(args, input=data, capture_output=True)
    if check and result.returncode:
        raise RuntimeError(f"{args}: {result.stderr.decode(errors='replace')[-4000:]}")
    return result


def kubectl(*args, data=None, check=True):
    return command(["kubectl", *args], data, check)


def apply(obj):
    kubectl("apply", "-f", "-", data=json.dumps(obj).encode())


def get(kind, name):
    return json.loads(kubectl("get", kind, name, "-o", "json").stdout)


def power(name, state):
    kubectl(
        "patch",
        "rvm",
        name,
        "--type=merge",
        "-p",
        json.dumps({"spec": {"powerState": state}}),
    )


def wait_for(check, timeout=120, interval=0.2):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(interval)
    raise AssertionError("timed out waiting for " + check.__name__)


def wait_phase(name, phase, timeout=100):
    def reached():
        obj = get("rvm", name)
        return obj if obj.get("status", {}).get("phase") == phase else None

    return wait_for(reached, timeout)
