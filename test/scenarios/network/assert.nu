use std/assert
use ../vm.nu [observe-storage]

def response [] { open --raw ($env.SCENARIO_DATA | path join response) }

def main [check: string, expected: string = ""] {
  match $check {
    config-first => {
      assert equal (response | from json) {setting: first, credential: local-fixture-only}
    }
    config-second => { assert equal (response | from json | get setting) second }
    config-disks => {
      assert equal (response | from json) {agent: local-fixture-only, tool: first}
    }
    secondary-initial => { assert equal (response) prepared-by-kubernetes }
    response => { assert equal (response) $expected }
    forwarded => { assert equal (response) "ready\n" }
    deleted => {
      observe-storage
      let s = open $env.SCENARIO_STATE
      assert equal $s.head.state Stopped
      assert equal ($s.head.owner? | default "") ""
      assert equal $s.head.checkpoint.generation 2
      assert ((kubectl get pvc scenario-network -o name | complete).exit_code == 0)
    }
    _ => { error make {msg: $"Unknown assertion: ($check)"} }
  }
}
