# Generic script runner. No cluster, VM, or object-store operations belong here.
export def command [script: string] {
  if ($script | str trim | is-empty) { error make {msg: "empty script"} }
  if not ($script =~ '[\r\n]') and (($script | path exists) or not ($script =~ '\s')) {
    match ($script | path parse | get extension) {
      nu => { [nu --no-config-file ($script | path expand)] }
      sh => { [bash --noprofile --norc ($script | path expand)] }
      _ => { if ($script | path exists) { [($script | path expand)] } else { [$script] } }
    }
  } else { [nu --no-config-file -c $script] }
}

def fields [value: record, allowed: list<string>] {
  let extra = $value | columns | where {|key| $key not-in $allowed }
  if ($extra | is-not-empty) { error make {msg: $"unknown fields: ($extra | str join ', ')"} }
}

def deadline [step: record] {
  let duration = $step.timeout? | default '180sec' | into string | str replace -r '([0-9])s$' '${1}sec' | str replace -r '([0-9])m$' '${1}min' | into duration
  if $duration <= 0sec or $duration > 20min { error make {msg: "timeout must be between zero and 20 minutes"} }
  $duration
}

export def validate [spec: record] {
  fields $spec [lab fixtures requires steps cases]
  if ($spec.lab? | describe) != string or ($spec.lab | is-empty) { error make {msg: "lab definition required"} }
  if (($spec.steps? | default [] | is-empty) == ($spec.cases? | default [] | is-empty)) { error make {msg: "expected steps or cases"} }
  let cases = $spec.cases? | default [{name: default, steps: $spec.steps?}]
  mut names = []
  for case in $cases {
    fields $case [name env steps]
    if ($case.name? | describe) != string or ($case.name | is-empty) or ($case.name in $names) { error make {msg: "unique case name required"} }
    $names = $names | append $case.name
    for value in ($case.env? | default {} | values) {
      if ($value | describe) != string { error make {msg: "case environment values must be strings"} }
    }
    if ($case.steps? | default [] | is-empty) { error make {msg: "case steps required"} }
    for step in $case.steps {
      fields $step [run assert cleanup eventually timeout]
      let actions = $step | columns | where {|key| $key in [run assert cleanup] }
      if ($actions | length) != 1 { error make {msg: "expected one script per step"} }
      command ($step | get $actions.0) | ignore
      if ($step.eventually? | default false | describe) != bool { error make {msg: "eventually must be boolean"} }
      if ($step.eventually? | default false) and $actions.0 != assert { error make {msg: "eventually requires assert"} }
      deadline $step | ignore
    }
  }
  $cases
}

# GNU timeout owns a process group, including grandchildren and background jobs.
# Capture both streams so the last failed retry remains visible in the report.
export def execute [script: string, duration: duration] {
  let argv = command $script
  with-env {BASH_ENV: ""} {
    do { ^timeout --kill-after=1s ($duration / 1sec | into string) ...$argv } | complete
  }
}

def event [entry: record] { $entry | to json --raw | print }

def step-run [step: record] {
  let script = $step.run? | default $step.assert?
  let until = (date now) + (deadline $step)
  loop {
    let result = execute $script ($until - (date now))
    if $result.exit_code == 0 { return $result }
    if not ($step.eventually? | default false) or (date now) >= $until { return $result }
    sleep 100ms
    if (date now) >= $until { return $result }
  }
}

def run-case [case: record, requires: list<string>] {
  let started = date now
  for key in $requires {
    if ($env | get -o $key | default "" | is-empty) { error make {msg: $"required capability: ($key)"} }
  }
  mut cleanup = []
  mut failed = false
  for indexed in ($case.steps | enumerate) {
    let step = $indexed.item
    if 'cleanup' in $step { $cleanup = $cleanup | prepend $step; continue }
    let started = date now
    event {event: step, case: $case.name, step: ($indexed.index + 1), message: ($step.run? | default $step.assert?)}
    let result = try { step-run $step } catch {|err| {exit_code: 1, stdout: "", stderr: $err.msg} }
    event {event: result, case: $case.name, step: ($indexed.index + 1), exit_code: $result.exit_code, elapsed_ms: (((date now) - $started) / 1ms), message: ($result.stdout + $result.stderr)}
    if $result.exit_code != 0 { $failed = true; break }
  }
  for step in $cleanup {
    let flag = if $failed { "1" } else { "0" }
    let result = try { with-env {SCENARIO_FAILED: $flag} { execute $step.cleanup (deadline $step) } } catch {|err| {exit_code: 1, stdout: "", stderr: $err.msg} }
    event {event: cleanup, case: $case.name, exit_code: $result.exit_code, message: ($result.stdout + $result.stderr)}
    if $result.exit_code != 0 { $failed = true }
  }
  let status = if $failed { "failed" } else { "passed" }
  event {event: case, name: $case.name, status: $status, elapsed_ms: (((date now) - $started) / 1ms), message: $"($status): ($case.name)"}
  not $failed
}

export def run [file: path] {
  let file = $file | path expand
  let spec = open $file
  let cases = validate $spec
  let root = $env.SCENARIO_ROOT? | default ($file | path dirname | path join ../../.. | path expand)
  cd ($file | path dirname)
  for case in $cases {
    let data = ^mktemp -d | str trim
    {} | to json | save ($data | path join state.json)
    let vars = $case.env? | default {} | transpose key value | each {|pair|
      mut value = $pair.value
      for match in ($value | parse -r '\$\{(?<name>[A-Za-z_][A-Za-z0-9_]*)\}') {
        $value = $value | str replace --all ('${' + $match.name + '}') ($env | get -o $match.name | default "")
      }
      {key: $pair.key, value: $value}
    } | reduce -f {} {|pair, vars| $vars | upsert $pair.key $pair.value }
    let ok = try {
      with-env ($vars | merge {SCENARIO_DATA: $data, SCENARIO_STATE: ($data | path join state.json), SCENARIO_ROOT: $root}) {
        run-case $case ($spec.requires? | default [])
      }
    } catch {|err| event {event: case, name: $case.name, status: failed, message: $err.msg}; false }
    rm -r $data
    if not $ok { return false }
  }
  true
}

def main [file: path, --check] {
  if $check { validate (open $file) | ignore; return }
  if not (run $file) { exit 1 }
}
