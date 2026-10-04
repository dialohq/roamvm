use std/assert
use ../vm.nu [observe observe-storage]

def load-ok [load: record] {
  assert equal $load.report.cpus 4
  assert equal ($load.report.iterations | length) 4
  assert ($load.report.iterations | all {|n| $n > 0 })
}

export def main [check: string, name: string = ""] {
  if $check in [runtime unschedulable checkpoint] { observe }
  if $check == checkpoint { observe-storage }
  let s = open $env.SCENARIO_STATE
  match $check {
    runtime => {
      assert equal $s.pod.spec.nodeName $env.ROAMVM_TEST_NODE
      assert equal $s.pod.spec.schedulerName default-scheduler
      assert equal $s.pod.status.qosClass Burstable
      assert equal $s.pod.spec.containers.0.resources.requests.cpu 500m
      let observed = $s.values.cpu.cgroups | get $s.vm.metadata.name
      assert equal $observed.cpus $s.values.cpu.runner_cpus
      assert equal $observed.max ["400000" "100000"]
    }
    response => { assert equal (open --raw ($env.SCENARIO_DATA | path join response)) $name }
    burst => {
      let load = $s.values.cpu.loads.burst
      load-ok $load
      assert ($load.cores > 0.75)
    }
    contention => {
      for name in [scenario-cpu-one scenario-cpu-two] {
        let load = $s.values.cpu.loads.concurrent | get $name
        load-ok $load
        assert ($load.cores > 0.25)
      }
    }
    unschedulable => {
      assert ($s.pod.status.conditions | any {|c| $c.reason? == Unschedulable and ($c.message? | default "" | str contains "Insufficient cpu") })
    }
    checkpoint => {
      assert equal $s.head.checkpoint.generation 1
      assert ($s.pod == null)
    }
    capped => {
      assert equal ($s.values.cpu.cgroups | get scenario-cpu-one | get max) ["25000" "100000"]
      let load = $s.values.cpu.loads.burst
      load-ok $load
      assert ($load.cores >= 0.1)
      assert ($load.cores <= 0.4)
    }
    _ => { error make {msg: $"Unknown assertion: ($check)"} }
  }
}
