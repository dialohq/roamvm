use std/assert
use ../vm.nu [observe]

def response [] { open --raw ($env.SCENARIO_DATA | path join response) }

def main [check: string, expected: string = ""] {
  observe
  let s = open $env.SCENARIO_STATE
  match $check {
    pending-stop => {
      assert ($s.head == null)
    }
    invalid => {
      assert equal $s.values.submissions.unpinned.reason Invalid
      assert equal $s.values.submissions.unpinned.code 422
      assert equal $s.values.submissions.duplicate_label.reason Invalid
      assert equal $s.values.submissions.duplicate_label.code 422
    }
    submissions => {
      assert $s.values.submissions.numeric_apply.accepted
      assert $s.values.submissions.numeric_apply_dry_run.accepted
    }
    config-first => {
      assert equal (response | from json) {setting: first, credential: local-fixture-only}
    }
    config-second => { assert equal (response | from json | get setting) second }
    config-disks => {
      assert equal (response | from json) {agent: local-fixture-only, tool: first}
    }
    secondary-initial => { assert equal (response) prepared-by-kubernetes }
    response => { assert equal (response) $expected }
    image-mutation => {
      assert equal $s.values.submissions.image_mutation.reason Invalid
      assert equal $s.values.submissions.image_mutation.code 422
    }
    forwarded => { assert equal (response) "ready\n" }
    deleted => {
      assert equal $s.head.state Stopped
      assert equal ($s.head.owner? | default "") ""
      assert equal $s.head.checkpoint.generation 2
      assert ((kubectl get pvc scenario-network -o name | complete).exit_code == 0)
    }
    _ => { error make {msg: $"Unknown assertion: ($check)"} }
  }
}
