use std/assert
use ../vm.nu [observe]

def container [pod: record, name: string] {
  $pod.status.containerStatuses | where name == $name | first
}

def main [check: string, reference: string = "", delta: int = 0] {
  observe
  let s = open $env.SCENARIO_STATE
  match $check {
    runtime => {
      assert equal $s.pod.spec.schedulerName default-scheduler
      assert equal $s.pod.spec.containers.0.resources.requests.cpu 250m
      assert ($s.pod.spec.volumes | all {|v| $v.hostPath? == null })
      assert ($s.pod.spec.volumes | any {|v| $v.name == working and $v.persistentVolumeClaim? != null })
      assert equal $s.pvc.metadata.ownerReferences.0.uid $s.vm.metadata.uid
    }
    away => { assert ($s.vm.status.nodeName != ($s.snapshots | get $reference | get node)) }
    retained => {
      let old = $s.snapshots | get $reference
      assert equal $s.head.checkpoint $old.head.checkpoint
      assert equal $s.head.owner $old.pod.metadata.uid
    }
    generation => {
      let old = $s.snapshots | get $reference
      assert equal $s.head.checkpoint.generation ($old.head.checkpoint.generation + $delta)
    }
    sidecar => {
      let old = $s.snapshots | get $reference
      let before = container $old.pod runtime
      let after = container $s.pod runtime
      assert ($after.restartCount > $before.restartCount)
      assert ($after.state.running? != null)
    }
    runner => { assert equal (container $s.pod runner).restartCount 0 }
    local => {
      assert equal $s.vm.status.phase Stopped
      assert ($s.vm.status.local != null)
      assert (not ($s.vm.status.local.durable? | default false))
      assert ($s.vm.status.conditions | where type == CheckpointReady | all {|c| $c.status != "True" })
      assert equal $s.pvc.metadata.name $s.vm.status.local.claimName
    }
    corruption => {
      let log = open --raw ($env.SCENARIO_DATA | path join logs)
      assert ($log | str contains "integrity mismatch")
      assert (not ($log | str contains "startup stage=hypervisor"))
      assert (not ($log | str contains "Linux version"))
    }
    _ => { error make {msg: $"Unknown assertion: ($check)"} }
  }
}
