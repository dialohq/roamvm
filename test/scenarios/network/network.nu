use ../harness.nu *

def remember-status [name: string, reply: record] {
  let old = values
  let submissions = ($old.submissions? | default {})
  save-values ($old | upsert submissions ($submissions | upsert $name {
    accepted: ($reply.status? != Failure)
    reason: ($reply.reason? | default "")
    code: ($reply.code? | default 0)
  }))
}

def main [action: string] {
  match $action {
    pending-stop => {
      let s = state
      wait-until {||
        ((checked { kubectl get pods -l $"vm.roamvm.io/name=($s.vm.metadata.name)" -o name } | lines | length) == 0)
      }
    }
    invalid => {
      remember-status unpinned (api POST /apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines (open unpinned.yaml))
      let duplicate = (open vm.yaml | upsert metadata.name scenario-network-duplicate-label
        | upsert spec.image $env.ROAMVM_TEST_IMAGE | update spec.configDisks.1.label TEST_AGENT)
      remember-status duplicate_label (api POST "/apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines?dryRun=All" $duplicate)
    }
    prepare-secondary => {
      checked { kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/scenario-network-prepare --timeout=180s }
      checked { kubectl delete pod scenario-network-prepare --wait=true }
    }
    apply => {
      let image = ($env.ROAMVM_TEST_IMAGE? | default "")
      if ($image | is-empty) { error make {msg: "ROAMVM_TEST_IMAGE is required"} }
      let manifest = (open vm.yaml | upsert spec.image $image | update spec.resources.limits.cpu 2)
      $manifest | to json | save --force (data numeric-vm.json)
      remember-status numeric_apply (api PATCH "/apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines/scenario-network?fieldManager=roamvm-test" $manifest application/apply-patch+yaml)
    }
    apply-dry-run => {
      remember-status numeric_apply_dry_run (api PATCH "/apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines/scenario-network?fieldManager=roamvm-test&dryRun=All" (open (data numeric-vm.json)) application/apply-patch+yaml)
    }
    allow-policy => {
      let probe = (state).probe
      let patch = ({spec: {ingress: [{from: [{podSelector: {matchLabels: {"roamvm.test/client": $probe}}}]}]}} | to json --raw)
      checked { kubectl patch networkpolicy scenario-network --type merge -p $patch }
    }
    mutate-image => {
      let patch = '{"spec":{"image":"registry.invalid/test@sha256:0000000000000000000000000000000000000000000000000000000000000000"}}'
      let result = api PATCH /apis/vm.roamvm.io/v1alpha1/namespaces/default/virtualmachines/scenario-network ($patch | from json) application/merge-patch+json
      remember-status image_mutation $result
    }
    update-config => {
      checked { kubectl patch configmap scenario-network --type merge -p '{"data":{"setting":"second"}}' }
    }
    remember-deleted => {
      let s = state
      save-values ((values) | upsert deleted_head $s.head)
    }
    seed-head => {
      let s = state
      let head = ($s.values.deleted_head | upsert vmID $s.vm.metadata.uid)
      s3-put $"vm/($s.vm.metadata.uid)/head.json" ($head | to json --raw)
    }
    _ => { error make {msg: $"unknown network action: ($action)"} }
  }
}
