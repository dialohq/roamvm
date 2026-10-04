use ../harness.nu *
use std/assert

def invalid [reply: record] {
  assert equal $reply.reason Invalid
  assert equal $reply.code 422
}

export def admission [] {
  invalid (api POST /apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines (open unpinned.yaml))
  let duplicate = (open vm.yaml | upsert metadata.name scenario-network-duplicate-label
    | upsert spec.image $env.ROAMVM_TEST_IMAGE | update spec.configDisks.1.label TEST_AGENT)
  invalid (api POST "/apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines?dryRun=All" $duplicate)
}

export def apply [--dry-run] {
  let manifest = open vm.yaml | upsert spec.image $env.ROAMVM_TEST_IMAGE | update spec.resources.limits.cpu 2
  let suffix = if $dry_run { "&dryRun=All" } else { "" }
  let reply = api PATCH $"/apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines/scenario-network?fieldManager=roamvm-test($suffix)" $manifest application/apply-patch+yaml
  assert ($reply.status? != Failure)
}

export def allow-policy [] {
  let patch = {spec: {ingress: [{from: [{podSelector: {matchLabels: {"roamvm.test/client": (state).probe}}}]}]}} | to json --raw
  checked { kubectl patch networkpolicy scenario-network --type merge -p $patch } | ignore
}

export def immutable-image [] {
  let patch = {spec: {image: "registry.invalid/test@sha256:0000000000000000000000000000000000000000000000000000000000000000"}}
  invalid (api PATCH /apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines/scenario-network $patch application/merge-patch+json)
}
