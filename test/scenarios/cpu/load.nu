use ../harness.nu [state values save-values checked guest-request]
use observe.nu read-cgroup

def usage [name: string] {
  let s = (state)
  let vm = (checked { kubectl get virtualmachine $name -o json } | from json)
  let stat = read-cgroup $vm.status.podName cpu.stat
  let line = ($stat | lines | where {|x| $x | str starts-with "usage_usec " } | first | split row -r '\s+')
  ($line.1 | into float) / 1_000_000
}

def measure [name: string] {
  let before = (usage $name)
  let started = (date now)
  let report = (guest-request $name "/cpu?seconds=10" "" | from json)
  let after = (usage $name)
  let elapsed = (((date now) - $started) / 1sec)
  {cores: (($after - $before) / $elapsed), report: $report}
}

def main [mode: string] {
  let old = (values)
  let cpu = ($old.cpu | default {})
  let loads = ($cpu.loads? | default {})
  let loads = if $mode == "burst" {
    $loads | upsert burst (measure (state).vm.metadata.name)
  } else if $mode == "parallel" {
    let concurrent = ([scenario-cpu-one scenario-cpu-two] | par-each {|name| {name: $name, load: (measure $name)} } | reduce -f {} {|it, acc| $acc | upsert $it.name $it.load })
    $loads | upsert concurrent $concurrent
  } else {
    error make {msg: $"unknown load mode: ($mode)"}
  }
  save-values ($old | upsert cpu ($cpu | upsert loads $loads))
}
