use std/assert
use ../vm.nu [observe]

def main [check: string] {
  if $check in [restored denied preserved] { observe }
  let s = open $env.SCENARIO_STATE
  let response = open --raw ($env.SCENARIO_DATA | path join response)
  match $check {
    initial => {
      let before = $s.snapshots.initial.response | from json
      assert ($before.bootID | is-not-empty)
      assert ($before.filesystemBytes > 0)
    }
    passes => { assert (($response | from json).growthPasses > 0) }
    settled => { assert (($response | from json).growthPasses <= (($s.snapshots.passes.response | from json).growthPasses + 1)) }
    allocation => {
      let before = $s.snapshots.initial.response | from json
      let grown = $s.snapshots.grown.response | from json
      assert ($grown.growthSize > $before.filesystemBytes)
      assert ($grown.growthHash | is-not-empty)
    }
    restored => {
      let before = $s.snapshots.initial.response | from json
      let grown = $s.snapshots.grown.response | from json
      let restored = $response | from json
      assert ($s.vm.status.nodeName != $s.snapshots.before-restart.node)
      assert ($restored.bootID != $before.bootID)
      for field in [diskSectors filesystemBytes growthSize growthHash] {
        assert equal ($restored | get $field) ($grown | get $field)
      }
    }
    denied => {
      let condition = $s.vm.status.conditions | where type == DiskReady | first
      assert equal $s.vm.status.phase Running
      assert equal $condition.status "False"
      assert ($condition.message | str contains "support resize")
    }
    preserved => {
      assert equal $response $s.snapshots.before.response
      assert equal $s.vm.status.podName $s.snapshots.before.pod.metadata.name
    }
    _ => { error make {msg: $"Unknown assertion: ($check)"} }
  }
}
