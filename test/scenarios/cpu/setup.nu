use ../harness.nu [node values save-values checked wait-until cpus]

def main [] {
  let node_name = $env.ROAMVM_TEST_NODE
  let context = (checked { kubectl config current-context } | str trim)
  if not ($node_name | str starts-with $"($context)-worker") {
    error make {msg: $"not a lab worker: ($node_name)"}
  }

  # This guard must precede every host-affinity or kubelet mutation.
  let vms = (checked { kubectl get virtualmachines.vm.roamvm.io -A -o json } | from json)
  let occupants = ($vms.items | where {|vm| ($vm.status.nodeName? | default "") == $node_name })
  if ($occupants | is-not-empty) {
    let names = ($occupants | each {|vm| $"($vm.metadata.namespace)/($vm.metadata.name)" } | str join ", ")
    error make {msg: $"worker ($node_name) has another VM: ($names)"}
  }

  let n = (checked { kubectl get node $node_name -o json } | from json)
  let capacity = ($n.status.capacity.cpu | into int)
  if $capacity <= 2 { error make {msg: "test requires more than two host CPUs"} }

  let config = (node $node_name cat /var/lib/kubelet/config.yaml)
  if ($config | str contains "kubeReserved:") {
    error make {msg: "use an unmodified worker"}
  }
  $config | save --raw --force ($env.SCENARIO_DATA | path join kubelet-config.yaml)

  let effective = (cpus (node $node_name cat /sys/fs/cgroup/cpuset.cpus.effective | str trim))
  if ($effective | length) < 2 { error make {msg: "fewer than two effective CPUs"} }

  let allowed_line = (open --raw /proc/self/status | lines | where {|line| $line | str starts-with "Cpus_allowed_list:" } | first)
  let host_cpus = (cpus ($allowed_line | split row ":" | last | str trim))
  if ($host_cpus | length) < 2 { error make {msg: "fewer than two available host CPUs"} }
  let pinned = ($host_cpus | first 2)
  let pin_set = ($pinned | str join ",")

  let pin_lines = (checked { virsh vcpupin $node_name --live } | lines)
  let pins = ($pin_lines | each {|line|
    let fields = ($line | split row -r '\s+' | where {|x| $x != "" })
    if ($fields | length) == 2 and $fields.0 =~ '^[0-9]+$' {
      {vcpu: $fields.0, set: $fields.1}
    }
  } | compact)
  if ($pins | length) < 3 { error make {msg: "expected more than two libvirt vCPUs"} }

  # setup.nu has not mutated anything before the original configuration and
  # complete affinity map are durable in scenario scratch space.
  {node: $node_name, capacity: $capacity, pins: $pins} | save --force ($env.SCENARIO_DATA | path join cpu-fixture.json)
  for pin in $pins { checked { virsh vcpupin $node_name $pin.vcpu $pin_set --live } | ignore }

  let actual_pins = (checked { virsh vcpupin $node_name --live } | lines | each {|line|
    let fields = ($line | split row -r '\s+' | where {|x| $x != "" })
    if ($fields | length) == 2 and $fields.0 =~ '^[0-9]+$' { cpus $fields.1 }
  } | compact)
  if ($actual_pins | length) != ($pins | length) or ($actual_pins | any {|set| $set != $pinned }) {
    error make {msg: "failed to constrain physical host CPU affinity"}
  }

  let encoded = ($"($config)\nkubeReserved:\n  cpu: \"($capacity - 2)\"\n" | encode base64)
  let shell_flag = "-c"
  node $node_name sh $shell_flag $"echo '($encoded)' | base64 -d > /var/lib/kubelet/config.yaml"
  node $node_name systemctl restart k3s

  wait-until {|| node-ready $node_name 2 }
  let old = (values)
  save-values ($old | upsert cpu {node: $node_name, runner_cpus: $effective})
}

export def node-ready [name: string, expected: int] {
  let n = (kubectl get node $name -o json | complete)
  if $n.exit_code != 0 { return false }
  let n = ($n.stdout | from json)
  let slots = ($n.status.allocatable."vm.roamvm.io/kvm"? | default "0" | into int)
  let bad = ($n.spec.taints? | default [] | any {|t| $t.key in [node.kubernetes.io/not-ready node.kubernetes.io/unreachable] })
  let ready = ($n.status.conditions | any {|c| $c.type == "Ready" and $c.status == "True" })
  (($n.status.allocatable.cpu | into int) == $expected) and $slots >= 2 and not $bad and $ready
}
