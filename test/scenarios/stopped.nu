use std/assert
let s = open $env.SCENARIO_STATE
assert equal $s.head.state Stopped
assert equal ($s.head.owner? | default "") ""
assert ($s.head.checkpoint? != null)
assert equal $s.vm.status.checkpoint $s.head.checkpoint
assert equal $s.objects [$s.head.checkpoint.key]
assert $s.vm.status.local.durable
assert equal $s.pvc.metadata.name $s.vm.status.local.claimName
