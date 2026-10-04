use std/assert
use ../vm.nu [observe]

def main [check: string, size: int = 0] {
  observe
  let s = open $env.SCENARIO_STATE
  let response = open --raw ($env.SCENARIO_DATA | path join response)
  match $check {
    initial => {
      let before = $s.snapshots.initial.response | from json
      assert ($before.bootID | is-not-empty)
      assert ($before.filesystemBytes > 0)
    }
    held => { assert $s.sample.growthHeld }
    capacity => {
      assert $s.sample.growthHeld
      assert equal $s.sample.diskSectors $"($size // 512)\n"
    }
    grown => {
      let before = $s.snapshots.initial.response | from json
      assert equal $s.sample.bootID $before.bootID
      assert equal $s.sample.pid $before.pid
      assert ($s.sample.uptime > $before.uptime)
      assert ($s.sample.filesystemBytes > ($size * 9 // 10))
      assert equal $s.sample.diskSectors $"($size // 512)\n"
    }
    reported => {
      assert equal $s.vm.status.rootDiskSize $size
      assert ($s.vm.status.conditions | any {|c| $c.type == DiskReady and $c.status == "True" })
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
