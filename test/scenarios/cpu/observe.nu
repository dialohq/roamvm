use ../harness.nu [state values save-values checked cpus]

export def read-cgroup [pod: string, file: string] {
  checked { kubectl exec $pod -c runner -- /bin/sh -c 'for p in /nix/store/*-coreutils-*/bin/cat /bin/cat /usr/bin/cat; do if [ -x "$p" ]; then exec "$p" "$1"; fi; done; exit 1' cat $"/sys/fs/cgroup/($file)" }
}

def main [] {
  let s = (state)
  let pod = $s.vm.status.podName
  let set = (cpus (read-cgroup $pod cpuset.cpus.effective | str trim))
  let maximum = (read-cgroup $pod cpu.max | str trim | split row -r '\s+')
  let old = (values)
  let cpu = ($old.cpu | default {})
  let cgroups = ($cpu.cgroups? | default {} | upsert $s.vm.metadata.name {cpus: $set, max: $maximum})
  save-values ($old | upsert cpu ($cpu | upsert cgroups $cgroups))
}
