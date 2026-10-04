use scenarios/harness.nu [checked wait-until]
use scenarios/vm.nu

let name = ($env.ROAMVM_TEST_EXISTING_VM? | default "")
if $name == "" {
  error make {msg: "Set ROAMVM_TEST_EXISTING_VM to an existing stopped SSH VM"}
}

let root = ($env.FILE_PWD | path dirname)
let scratch = (mktemp -d --tmpdir roamvm-existing-vm.XXXXXX | str trim)
$env.SCENARIO_ROOT = $root
$env.SCENARIO_DATA = $scratch
$env.SCENARIO_STATE = ($scratch | path join state.json)
{} | to json | save $env.SCENARIO_STATE

def host-key [] {
  let pod = (open $env.SCENARIO_STATE).vm.status.podName
  checked { bash -c '
    set -eu
    log="$1"; pod="$2"
    kubectl port-forward "pod/$pod" :22 >"$log" 2>&1 & pid=$!
    trap "kill $pid 2>/dev/null || true; wait $pid 2>/dev/null || true" EXIT
    i=0
    while [ "$i" -lt 100 ]; do
      port=$(sed -n "s/Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p" "$log" | head -1)
      if [ -n "$port" ]; then
        ssh-keyscan -T 5 -t ed25519 -p "$port" 127.0.0.1 2>/dev/null | awk "\$2 == \"ssh-ed25519\" { print \$2 \" \" \$3; exit }"
        exit
      fi
      i=$((i + 1)); sleep .1
    done
    exit 1
  ' -- ($env.SCENARIO_DATA | path join port-forward.log) $pod } | str trim
}

let failure = try {
  vm init
  vm select-vm $name
  if (open $env.SCENARIO_STATE).vm.spec.powerState != "Stopped" {
    error make {msg: "existing VM must initially be stopped"}
  }
  touch ($scratch | path join armed)
  mut identity = ""
  for boot in 1..2 {
    vm power Running
    vm phase Running
    let pod = (open $env.SCENARIO_STATE).vm.status.podName
    wait-until { ((checked { kubectl get pod $pod -o json } | from json).status.conditions | where type == Ready | where status == True | length) == 1 }
    let observed = host-key
    if $observed == "" { error make {msg: "missing SSH host identity"} }
    if ($identity != "") and ($observed != $identity) { error make {msg: "SSH host identity changed across boots"} }
    $identity = $observed
    vm stop $"boot-($boot)"
  }
  null
} catch {|err| $err }

# Leave the caller's durable VM stopped even if probing failed mid-boot.
let cleanup_failure = try {
  if ($scratch | path join armed | path exists) {
    vm select-vm $name
    if (open $env.SCENARIO_STATE).vm.spec.powerState != "Stopped" { vm stop cleanup }
  }
  null
} catch {|err| $err }
rm -rf $scratch
let result = if $failure != null { $failure } else { $cleanup_failure }
if $result != null { error make {msg: $result.msg} }
