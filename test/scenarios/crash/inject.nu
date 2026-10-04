use ../harness.nu [state node checked]
use std/assert

let s = state
match $env.CRASH_MODE {
  oom => {
    # The allocating exec is expected to die with its container.
    let command = "awk 'BEGIN { s=\"xxxxxxxxxxxxxxxx\"; for(i=0;i<16;i++) s=s s; for(i=0;i<4096;i++) a[i]=s i }'"
    kubectl exec $s.vm.status.podName -c runner -- /bin/sh -c $command | complete | ignore
  }
  hypervisor => {
    checked { kubectl exec $s.vm.status.podName -c runner -- /bin/sh -ec 'for p in /proc/[0-9]*; do read -r name < "$p/comm" || continue; case "$name" in qemu-system-*|.qemu-system-*) kill -KILL "${p##*/}"; exit 0;; esac; done; exit 1' } | ignore
  }
  runner => {
    let container = $s.pod.status.containerStatuses | where name == runner | first
    let id = $container.containerID | str replace containerd:// ""
    let info = node $s.pod.spec.nodeName crictl --config /dev/null --runtime-endpoint unix:///run/k3s/containerd/containerd.sock inspect $id | from json
    assert ($info.info.pid > 1)
    node $s.pod.spec.nodeName kill -KILL ($info.info.pid | into string)
  }
}
