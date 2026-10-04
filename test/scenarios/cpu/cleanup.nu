use ../harness.nu [node checked wait-until]
use setup.nu node-ready

def main [] {
  let fixture_path = ($env.SCENARIO_DATA | path join cpu-fixture.json)
  if not ($fixture_path | path exists) { return }
  let fixture = (open $fixture_path)
  let config_path = ($env.SCENARIO_DATA | path join kubelet-config.yaml)
  let config_error = try {
    let encoded = (open --raw $config_path | encode base64)
    let shell_flag = "-c"
    node $fixture.node sh $shell_flag $"echo '($encoded)' | base64 -d > /var/lib/kubelet/config.yaml"
    node $fixture.node systemctl restart k3s
    ""
  } catch {|err| $err.msg }
  let pin_errors = $fixture.pins | each {|pin|
    try { checked { virsh vcpupin $fixture.node $pin.vcpu $pin.set --live } | ignore; "" } catch {|err| $err.msg }
  } | where {|message| $message != "" }
  if $config_error != "" or ($pin_errors | is-not-empty) {
    error make {msg: $"Restore failed: ($config_error) ($pin_errors | str join '; ')"}
  }
  wait-until {|| node-ready $fixture.node $fixture.capacity }
}
