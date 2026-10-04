use std/assert
use runner.nu [validate command execute]

let root = $env.FILE_PWD | path dirname
let runner = $root | path join test runner.nu
let cli = $root | path join test cli.sh

def syntax [body: string] {
  let result = do { $body | nu --no-config-file --stdin -c 'if not ($in | nu-check --debug) { exit 1 }' } | complete
  assert equal $result.exit_code 0 $result.stderr
}

# Validate every checked-in case and parse both inline and file-based Nushell.
for file in (glob ($root | path join test scenarios '*' scenario.yaml)) {
  let cases = validate (open $file)
  cd ($file | path dirname)
  for script in (glob '*.nu') { syntax (open --raw $script) }
  for case in $cases {
    for step in $case.steps {
      let script = $step.run? | default $step.assert? | default $step.cleanup?
      let argv = command $script
      if $argv.0 == nu and ($argv | length) == 4 { syntax $script }
    }
  }
}
cd $root
for body in ['steps: []' 'steps: [{}]' 'steps: [{cordon: node}]' 'steps: [{run: null}]' 'steps: [{run: ""}]' 'steps: [{run: ok, assert: ok}]' 'steps: [{run: ok, eventually: true}]' 'steps: [{assert: ok, timeout: forever}]' 'steps: [{assert: ok, timeout: -1s}]' 'cases: [{name: x, steps: []}]' 'cases: [{name: x, steps: [{run: ok}]}, {name: x, steps: [{run: ok}]}]'] {
  let rejected = try { validate ($"lab: lab.nix\n($body)" | from yaml) | ignore; false } catch { true }
  assert $rejected $body
}
assert (try { validate {steps: [{run: true}]} | ignore; false } catch { true })

let scratch = ^mktemp -d | str trim
let failure = try {
  cd $scratch
  'exit 99' | save startup.sh
  'printf success' | save good.sh
  'echo failure >&2; exit 7' | save bad.sh
  'print -n success' | save good.nu
  'print -e failure; exit 7' | save bad.nu
  "#!/bin/sh\nprintf executable\n" | save executable
  ^chmod +x executable
  for script in [good.sh good.nu "print -n success\n"] {
    let result = with-env {BASH_ENV: ($scratch | path join startup.sh)} { execute $script 2sec }
    assert equal $result.exit_code 0
    assert equal $result.stdout success
  }
  for script in [bad.sh bad.nu "print -e failure; exit 7\n"] {
    let result = execute $script 2sec
    assert equal $result.exit_code 7
    assert ($result.stderr | str contains failure)
  }
  assert equal (execute executable 2sec).stdout executable
  assert equal (execute 'false' 2sec).exit_code 1
  assert equal (execute "false\n" 2sec).exit_code 0
  assert equal (execute "printf '%s' 'spaces; $(touch unexpected)'" 2sec).stdout 'spaces; $(touch unexpected)'
  assert not ('unexpected' | path exists)
  for script in [missing.sh missing.nu missing-executable] { assert ((execute $script 2sec).exit_code != 0) }
  # A timed-out step must terminate its descendants, not just its direct shell.
  'sleep 2; touch leaked' | save slow.sh
  assert equal (execute slow.sh 50ms).exit_code 124
  sleep 2100ms
  assert not ('leaked' | path exists)

  for mode in [success failure timeout cleanup-failure] {
    let script = match $mode { failure => 'exit 7', timeout => 'sleep 2sec', _ => 'print ok' }
    let last_cleanup = if $mode == cleanup-failure { 'exit 9' } else { 'print ok' }
    let spec = {lab: lab.nix, steps: [
      {cleanup: '[$env.SCENARIO_FAILED first] | str join ":" | save --append result'}
      {cleanup: '[$env.SCENARIO_FAILED second] | str join ":" | save --append result'}
      {cleanup: $last_cleanup}
      {assert: 'if not ("retried" | path exists) { touch retried; exit 1 }', eventually: true, timeout: 2s}
      {assert: $script, timeout: 100ms}
      {run: 'touch reached'}
    ]}
    $spec | to yaml | save --force scenario.yaml
    let result = do { nu --no-config-file $runner scenario.yaml } | complete
    assert equal ($result.exit_code == 0) ($mode == success) $"($mode): ($result.stdout) ($result.stderr)"
    let expected = if $mode == success { "0:second0:first" } else { "1:second1:first" }
    assert equal (open --raw result) $expected
    assert equal ('reached' | path exists) ($mode in [success cleanup-failure])
    let events = $result.stdout | lines | each { from json }
    assert equal ($events | last | get event) case
    rm -f result retried reached
  }
  # Each case gets isolated environment and scratch state; anchors remain YAML.
  {lab: lab.nix, cases: [
    {name: first, env: {VALUE: '${FIXTURE}'}, steps: [{assert: 'use std/assert; assert equal $env.VALUE expanded; touch ($env.SCENARIO_DATA | path join marker)'}]}
    {name: second, steps: [{assert: 'use std/assert; assert equal ($env.VALUE? | default "") ""; assert not (($env.SCENARIO_DATA | path join marker) | path exists)'}]}
  ]} | to yaml | save --force scenario.yaml
  let isolated = with-env {FIXTURE: expanded} { do { nu --no-config-file $runner scenario.yaml } | complete }
  assert equal $isolated.exit_code 0 $isolated.stdout

  cd $root
  assert equal (^bash $cli list | lines) [cpu crash generations lifecycle network resize]
  for args in [[run] [run ../bad] [list extra] [--slot 100 run all] [lab unknown]] {
    assert equal (do { bash $cli ...$args } | complete).exit_code 2
  }
  # Intercept dispatch so these checks never provision or reset a lab.
  let bash = which bash | first | get path
  "#!/bin/sh\nprintf '%s\\n' \"$ROAMVM_LAB_SLOT\" \"$@\"\nexit 17\n" | save ($scratch | path join bash)
  ^chmod +x ($scratch | path join bash)
  let dispatch = with-env {PATH: ($env.PATH | prepend $scratch)} { do { ^$bash $cli --slot 7 run lifecycle } | complete }
  assert equal $dispatch.exit_code 17
  assert equal ($dispatch.stdout | lines) ["7" test/libvirt/scenario.sh run lifecycle]
  # A refused context must not trigger a mutating "restore stopped" cleanup.
  '#!/bin/sh
printf "%s\n" "$*" >> "$GUARD_LOG"
echo unrelated-cluster
' | save ($scratch | path join kubectl)
  ^chmod +x ($scratch | path join kubectl)
  let guard_log = $scratch | path join guard.log
  let guard = with-env {PATH: ($env.PATH | prepend $scratch), GUARD_LOG: $guard_log, ROAMVM_TEST_EXISTING_VM: fixture} {
    do { nu --no-config-file ($root | path join test existing-vm.nu) } | complete
  }
  assert ($guard.exit_code != 0)
  assert ($guard.stderr | str contains 'refusing context')
  assert equal (open --raw $guard_log | lines) ['config current-context']
  null
} catch {|err| $err }
cd $root
rm -r $scratch
if $failure != null { error make {msg: $failure.msg, help: $failure.debug} }
print 'Runner validation, scripts, deadlines, retries, cleanup, isolation and CLI checks passed.'
