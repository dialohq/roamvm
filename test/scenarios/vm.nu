# Ordinary scenario helpers for the VM integration lab.  Importing this module
# has no side effects; scenario.yaml explicitly calls cleanup and then init.
use harness.nu [checked data guest-request wait-until mc-config s3-get-optional]

def current [] { open $env.SCENARIO_STATE }
def save-state [state: record] { $state | to json | save --force $env.SCENARIO_STATE }
def vm-name [] {
  let name = (current).selected?
  if ($name == null) or ($name == "") { error make {msg: "select or create a VM first"} }
  $name
}
def kget [kind: string, name: string] { checked { kubectl get $kind $name -n default -o json } | from json }
def register [kind: string, name: string, namespace: string = default] {
  let s = current
  let item = {kind: $kind, name: $name, namespace: $namespace}
  if not ($item in ($s.resources? | default [])) { save-state ($s | upsert resources (($s.resources? | default []) | append $item)) }
}
def response-path [] { data response }

export def init [] {
  let context = checked { kubectl config current-context } | str trim
  let allowed = ($context in [kind-roamvm-test kind-roamvm kind-roamvm-cilium kind-roamvm-crash-test roamvm-libvirt])
  let explicit = ($env.ROAMVM_TEST_CONTEXT? | default "")
  if not ($allowed or (($context == $explicit) and ($context | str starts-with "roamvm-libvirt-"))) {
    error make {msg: $"refusing context ($context); use a disposable lab"}
  }
  if (($env.S3_ENDPOINT? | default "") == "") or (($env.S3_BUCKET? | default "") == "") { error make {msg: "S3_ENDPOINT and S3_BUCKET are required"} }
  mkdir $env.SCENARIO_DATA
  save-state {selected: null, uid: null, deleted: false, paused: false, probe: "", resources: [], snapshots: {}, vm: null, pod: null, pvc: null, head: null, values: {}}
  mc-config | ignore
}

# Successful cases remove all registered workloads concurrently. Failed cases
# retain them for debugging; host/store recovery belongs in unconditional YAML
# cleanup steps and is deliberately not coupled to this decision.
export def cleanup [] {
  if (($env.SCENARIO_FAILED? | default "0") == "1") { return }
  if not ($env.SCENARIO_STATE | path exists) { return }
  let resources = (current).resources? | default []
  if ($resources | is-empty) { return }
  # Submit every deletion before waiting, so PVC protection and independent
  # finalizers do not unnecessarily serialize teardown.
  for r in $resources { checked { kubectl delete -n $r.namespace $r.kind $r.name --ignore-not-found --wait=false } | ignore }
  for r in $resources { checked { kubectl wait -n $r.namespace --for=delete $"($r.kind)/($r.name)" --timeout=180s } | ignore }
}

export def create [file: string] {
  let source = if ($file | path exists) { $file } else { $env.SCENARIO_ROOT | path join test scenarios $file }
  mut rendered = open --raw $source
  for name in ($env | columns) {
    let marker = '${' + $name + '}'
    if ($rendered | str contains $marker) { $rendered = $rendered | str replace --all $marker ($env | get $name | into string) }
  }
  mut object = ($rendered | from yaml)
  if (($object.metadata.namespace? | default "") == "") { $object = $object | upsert metadata.namespace default }
  if $object.kind == VirtualMachine {
    let defaults = {powerState: Running, image: ($env.ROAMVM_TEST_IMAGE? | default ""), cpus: 2, memory: 512Mi, readinessPort: 8080, shutdownTimeoutSeconds: 120, resources: {requests: {cpu: 250m}}}
    $object = $object | upsert spec ($defaults | merge deep $object.spec)
  }
  $object | to yaml | checked { kubectl create -f - } | ignore
  register $object.kind $object.metadata.name $object.metadata.namespace
  if $object.kind == VirtualMachine { select-vm $object.metadata.name }
  observe
}

export def select-vm [name: string] {
  let v = kget virtualmachine $name
  save-state ((current) | upsert selected $name | upsert vm $v | upsert uid $v.metadata.uid | upsert deleted false)
}

export def expose [] {
  let name = vm-name
  mut s = current
  if (($s.probe? | default "") == "") {
    let probe = $"probe-(random uuid)"
    let cluster = checked { kubectl config current-context } | str trim | str replace 'kind-' ''
    let manifest = {apiVersion: v1, kind: Pod, metadata: {name: $probe, namespace: default, labels: {"roamvm.test/client": $probe}}, spec: {terminationGracePeriodSeconds: 1, automountServiceAccountToken: false, nodeSelector: {"kubernetes.io/hostname": $"($cluster)-control-plane"}, tolerations: [{key: "node-role.kubernetes.io/control-plane", operator: Exists, effect: NoSchedule}], containers: [{name: curl, image: "curlimages/curl:8.17.0", command: [sh -c "trap 'exit 0' TERM INT; sleep 7200 & wait"]}]}}
    $manifest | to yaml | checked { kubectl create -f - } | ignore
    register Pod $probe
    $s = current | upsert probe $probe
    save-state $s
  }
  let v = kget virtualmachine $name
  let port = $v.spec.readinessPort
  {apiVersion: v1, kind: Service, metadata: {name: $name, namespace: default}, spec: {selector: {"vm.roamvm.io/name": $name}, ports: [{port: $port, targetPort: $port}]}} | to yaml | checked { kubectl create -f - } | ignore
  register Service $name
  observe
}

export def phase [state: string] { let name = vm-name; wait-until { (kget virtualmachine $name).status.phase? == $state }; observe }
export def ready [] {
  phase Running
  let s = current
  wait-until { ((kget pod $s.probe).status.conditions | where type == Ready | get 0.status?) == True }
  wait-until { try { (checked { kubectl exec $s.probe -c curl -- curl -fsS --connect-timeout 0.1 --max-time 20 $"http://((vm-name)).default.svc.cluster.local:8080/ready" }) == "ready\n" } catch { false } }
  observe
}

export def request [path: string, body?: any] {
  guest-request (vm-name) $path $body | save --raw --force (response-path)
}

export def random [name: string, bytes: int] {
  if ($bytes <= 0) or ($bytes > 16777216) { error make {msg: "payload size must be 1..16777216"} }
  checked { openssl rand -out (data $"payload-($name)") $bytes } | ignore
}
export def text-data [name: string, value: string] { $value | save --raw --force (data $"payload-($name)") }
export def append-data [name: string] {
  let path = data $"payload-($name)"
  let size = ls $path | get 0.size | into int
  # printf emits the exact single byte (including NUL) through the external pipeline.
  checked { bash -c 'printf "\\$(printf %03o $(( $1 % 251 )))" >> "$2"' -- $size $path } | ignore
}
export def write-data [name: string] {
  request /data (open --raw (data $"payload-($name)"))
}
export def read-data [] { request /data }

export def power [state: string] { checked { kubectl patch virtualmachine (vm-name) --type merge -p $'{"spec":{"powerState":"($state)"}}' } | ignore }
export def start [] { power Running; ready }

export def capture [name: string] {
  observe
  observe-storage
  let s = current
  mut snap = {node: ($s.vm.status.nodeName? | default ($s.vm.status.local?.nodeName? | default "")), head: $s.head, pod: $s.pod, vm: $s.vm, response: ""}
  if (response-path | path exists) {
    let response = open --raw (response-path)
    if ($response | describe) == string { $snap = $snap | upsert response $response }
  }
  save-state ($s | upsert snapshots ($s.snapshots | upsert $name $snap))
}

export def stop [name: string] {
  let old = (kget virtualmachine (vm-name)).status.podName? | default ""
  power Stopped; phase Stopped
  wait-until { let v = kget virtualmachine (vm-name); (($v.status.conditions | where type == CheckpointReady | where status == True | where observedGeneration == $v.metadata.generation | length) == 1) }
  if $old != "" { checked { kubectl wait --for=delete $"pod/($old)" --timeout=180s } | ignore }
  capture $name
  checked { nu --no-config-file ($env.SCENARIO_ROOT | path join test scenarios stopped.nu) } | ignore
}

export def forward [] {
  let pod = (kget virtualmachine (vm-name)).status.podName
  checked { bash -c 'kubectl port-forward "pod/$1" :8080 >"$2" 2>&1 & p=$!; trap "kill $p 2>/dev/null || true; wait $p 2>/dev/null || true" EXIT; for i in $(seq 1 100); do port=$(sed -n "s/Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p" "$2"); if [ -n "$port" ]; then curl -fsS --max-time 3 "http://127.0.0.1:$port/ready" && exit; fi; sleep .1; done; exit 1' -- $pod (data forward.log) } | save --raw --force (response-path)
}

export def delete-vm [] {
  let name = vm-name; let v = kget virtualmachine $name
  checked { kubectl delete virtualmachine $name --wait=true --timeout=180s } | ignore
  save-state ((current) | upsert uid $v.metadata.uid | upsert deleted true)
  observe
  observe-storage
}

export def observe [] {
  mut s = current
  let name = $s.selected?
  mut vm = $s.vm?
  if (($name | default "") != "") and not ($s.deleted? | default false) { $vm = kget virtualmachine $name; $s = $s | upsert uid $vm.metadata.uid }
  mut pod = null; mut pvc = null
  if ($vm != null) and not ($s.deleted? | default false) {
    let podname = $vm.status.podName? | default ""
    if $podname != "" { let result = checked { kubectl get pod $podname -o json --ignore-not-found }; if ($result | str trim | is-not-empty) { $pod = $result | from json } }
    mut claim = ""
    if $pod != null { for volume in ($pod.spec.volumes? | default []) { if ($volume.name == working) and (($volume.persistentVolumeClaim? | default null) != null) { $claim = $volume.persistentVolumeClaim.claimName } } }
    if ($claim == "") { $claim = $vm.status.local?.claimName? | default "" }
    if $claim != "" { $pvc = kget pvc $claim }
  }
  save-state ($s | merge {vm: $vm, pod: $pod, pvc: $pvc})
}

export def observe-storage [] {
  let s = current
  mut head = null
  if not ($s.paused? | default false) and (($s.uid? | default "") != "") {
    let uid = $s.uid
    let result = s3-get-optional $"vm/($uid)/head.json"; if $result.found { $head = $result.body | from json }
  }
  save-state ($s | upsert head $head)
}
