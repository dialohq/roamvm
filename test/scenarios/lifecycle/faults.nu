use ../harness.nu [checked data state values save-values node wait-until mc-config]

def update-flag [name: string, value: any] {
  save-values ((values) | upsert $name $value)
}

export def stop-worker [node_name: string] {
  let cluster = checked { kubectl config current-context } | str trim
  if not ($node_name | str starts-with $"($cluster)-worker") { error make {msg: $"not a lab worker: ($node_name)"} }
  let others = checked { kubectl get virtualmachines.vm.roamvm.io -A -o json } | from json | get items | where {|vm|
    ($vm.status.nodeName? | default "") == $node_name and $vm.metadata.name != (state).vm.metadata.name
  }
  if ($others | is-not-empty) { error make {msg: $"worker ($node_name) contains another VM"} }
  update-flag worker $node_name
  update-flag worker_started false
  checked { virsh destroy $node_name } | ignore
}

export def start-worker [node_name: string] {
  let started = date now | format date "%+"
  checked { virsh start $node_name } | ignore
  update-flag worker_started $started
}

export def wait-worker [node_name: string] {
  let started = (values).worker_started | into datetime
  wait-until {
    let n = checked { kubectl get node $node_name -o json } | from json
    let ready = $n.status.conditions | where type == Ready | where status == True | where {|c| ($c.lastHeartbeatTime | into datetime) > $started }
    ($ready | is-not-empty) and (($n.status.allocatable | get 'vm.roamvm.io/kvm' | into int) > 0)
  }
}

export def restore-worker [] {
  let v = values
  if (($v.worker? | default "") | is-empty) { return }
  if (($v.worker_started? | default false) == false) { start-worker $v.worker }
  wait-worker $v.worker
  checked { kubectl uncordon $v.worker } | ignore
  update-flag worker ""
}

export def recover [snapshot: string] {
  let s = state
  let owner = $s.snapshots | get $snapshot | get head.owner
  let cli = $env.ROAMVM_TEST_CLI? | default ($env.SCENARIO_ROOT | path join bin roamvm)
  checked { ^$cli recover --vm-id $s.vm.metadata.uid --owner $owner --fenced } | ignore
  use ../vm.nu [phase]
  phase Stopped
}

export def kill-container [name: string] {
  let s = state
  let status = $s.pod.status.containerStatuses | where name == $name | first
  let id = $status.containerID | str replace 'containerd://' ''
  let details = node $s.vm.status.nodeName crictl --config /dev/null --runtime-endpoint unix:///run/k3s/containerd/containerd.sock inspect $id | from json
  let pid = $details.info.pid
  if $pid <= 1 { error make {msg: $"invalid container PID ($pid)"} }
  node $s.vm.status.nodeName kill -KILL ($pid | into string) | ignore
}

export def kill-hypervisor [] {
  let pod = (state).vm.status.podName
  checked { kubectl exec $pod -c runner -- /bin/sh -ec 'for p in /proc/[0-9]*; do read -r n < "$p/comm" || continue; case "$n" in qemu-system-*|.qemu-system-*) kill -KILL "${p##*/}"; exit 0;; esac; done; exit 1' } | ignore
}

export def runtime-ready [] {
  wait-until {
    let pod = (state).vm.status.podName
    (do { kubectl exec $pod -c runtime -- curl --silent --max-time 1 --unix-socket /run/roamvm/runtime.sock http://runtime/ -o /dev/null } | complete).exit_code == 0
  }
}

export def store [paused: bool] {
  let signal = if $paused { "SIGSTOP" } else { "SIGCONT" }
  let control = checked { kubectl get nodes -o json } | from json | get items.metadata.name | where {|n| $n | str ends-with '-control-plane' } | first
  if $paused { update-flag store_paused true }
  node $control systemctl kill $"--signal=($signal)" --kill-whom=main minio | ignore
  open $env.SCENARIO_STATE | upsert paused $paused | to json | save --force $env.SCENARIO_STATE
  update-flag store_paused $paused
}

export def resume-store [] {
  if ((values).store_paused? | default false) { store false }
}

export def corrupt [snapshot: string] {
  let key = (state).snapshots | get $snapshot | get head.checkpoint.key
  let object = $"fixture/($env.S3_BUCKET)/($key)"
  checked { mc --config-dir (mc-config) cp $object (data checkpoint-original) } | ignore
  {key: $key} | to json | save --force (data repair.json)
  update-flag repair_pending true
  let config = data mc
  checked { "injected corruption" | ^mc --config-dir $config pipe $object } | ignore
}

export def repair [] {
  if not ((values).repair_pending? | default false) { return }
  let key = (open (data repair.json)).key
  checked { mc --config-dir (mc-config) cp (data checkpoint-original) $"fixture/($env.S3_BUCKET)/($key)" } | ignore
  update-flag repair_pending false
}

export def save-logs [] {
  let pod = (state).vm.status.podName
  checked { kubectl logs $pod -c runner } | save --force (data logs)
}
