# Shared transports and scratch state; scenario decisions belong in each directory.
export def state [] { open $env.SCENARIO_STATE }
export def data [name: string] { $env.SCENARIO_DATA | path join $name }
export def values [] { if (data values.json | path exists) { open (data values.json) } else { {} } }
export def save-values [v: record] { $v | to json | save --force (data values.json) }

export def checked [command: closure] {
  let result = $in | do $command | complete
  if $result.exit_code != 0 { error make {msg: $"Command failed [($result.exit_code)]: ($result.stderr)($result.stdout)"} }
  $result.stdout
}

export def --wrapped node [name: string, ...args: string] {
  # SSH sends a command string, not an argv vector.
  let command = $args | each {|arg| "'" + ($arg | str replace --all "'" "'\"'\"'") + "'" } | str join " "
  checked { bash ($env.SCENARIO_ROOT | path join test libvirt lab.sh) ssh $name $command }
}

export def guest-request [name: string, path: string, body?: string] {
  let probe = (state).probe
  let url = $"http://($name).default.svc.cluster.local:8080($path)"
  if $body == null {
    checked { kubectl exec $probe -c curl -- curl -fsS --max-time 20 $url }
  } else {
    checked { $body | kubectl exec -i $probe -c curl -- curl -fsS --max-time 20 --data-binary @- $url }
  }
}

export def wait-until [condition: closure, timeout: duration = 180sec] {
  let deadline = (date now) + $timeout
  loop {
    let result = try { {ok: (do $condition), message: "condition remained false"} } catch {|err| {ok: false, message: $err.msg} }
    if $result.ok { return }
    if (date now) >= $deadline { error make {msg: $"Timed out: ($result.message)"} }
    sleep 100ms
  }
}

export def cpus [set: string] {
  $set | split row , | each {|part|
    let bounds = $part | split row - | into int
    if ($bounds | length) == 1 { [$bounds.0] } else { $bounds.0..$bounds.1 | each {|n| $n } }
  } | flatten | sort | uniq
}

# The disposable lab authenticates with certificates embedded in its kubeconfig.
# curl preserves Kubernetes Status documents on rejected requests, unlike kubectl.
export def api [method: string, path: string, body: record, content_type: string = "application/json"] {
  let config = checked { kubectl config view --raw --minify -o json } | from json
  let cluster = $config.clusters.0.cluster
  let user = $config.users.0.user
  $cluster.certificate-authority-data | decode base64 | save --force (data ca.pem)
  $user.client-certificate-data | decode base64 | save --force (data cert.pem)
  $user.client-key-data | decode base64 | save --force (data key.pem)
  checked {
    $body | to json --raw | curl --silent --show-error --max-time 30 --cacert (data ca.pem) --cert (data cert.pem) --key (data key.pem) --request $method --header $"Content-Type: ($content_type)" --data-binary @- $"($cluster.server)($path)"
  } | from json
}

export def s3-put [key: string, body: string] {
  let config = mc-config
  checked { $body | mc --config-dir $config pipe $"fixture/($env.S3_BUCKET)/($key)" } | ignore
}

# Configure the client once per scenario case.  In particular, don't put its
# credentials in the checkout or pay the alias handshake on every observation.
export def mc-config [] {
  let config = data mc
  let marker = $config | path join .scenario-configured
  if not ($marker | path exists) {
    mkdir $config
    checked { mc --config-dir $config alias set fixture $env.S3_ENDPOINT $env.AWS_ACCESS_KEY_ID $env.AWS_SECRET_ACCESS_KEY } | ignore
    touch $marker
  }
  $config
}

export def s3-get [key: string] {
  checked { mc --config-dir (mc-config) cat $"fixture/($env.S3_BUCKET)/($key)" }
}

export def s3-list [prefix: string] {
  let root = $"fixture/($env.S3_BUCKET)/($prefix)"
  checked { mc --json --config-dir (mc-config) ls --recursive $root } | lines | each {|line| $prefix + ($line | from json).key }
}

# Absence is expected before the first durable commit.  Any other S3 failure
# remains fatal rather than being accidentally reported as an absent head.
export def s3-get-optional [key: string] {
  let result = (do { mc --json --config-dir (mc-config) cat $"fixture/($env.S3_BUCKET)/($key)" } | complete)
  if $result.exit_code == 0 { return {found: true, body: $result.stdout} }
  let message = try { ($result.stdout + $result.stderr | from json).error.cause.message } catch { "" }
  if $message == "Object does not exist" {
    {found: false, body: ""}
  } else {
    error make {msg: $"S3 read failed for ($key): ($result.stderr)"}
  }
}
