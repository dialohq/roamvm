#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'HELP'
Usage: te2e [--slot 0..99] COMMAND

  list                     List available scenarios
  run SCENARIO|all         Reset the frozen lab and run tests
  lab plan|up|install      Plan, provision, or install the runtime
  lab fixtures            Build and publish scenario image fixtures
  lab freeze|freeze-warm   Capture a cold or warm baseline
  lab reset|reset-cold     Restore the baseline
  lab down|destroy        Stop nodes, or remove Terraform-owned resources
  lab check               Validate declarations without starting VMs

Run inside a RoamVM checkout. Provision and freeze once before running tests.
--slot selects an independent lab; defaults to ROAMVM_LAB_SLOT or 0.
HELP
}

fail() { echo "$*" >&2; usage >&2; exit 2; }
if [[ ${1:-} == --slot ]]; then
  [[ ${2:-} =~ ^([0-9]|[1-9][0-9])$ ]] || fail 'Expected --slot 0..99'
  export ROAMVM_LAB_SLOT=$2
  shift 2
fi
case "${1:-help}" in
  help|-h|--help) usage; exit 0 ;;
esac
root=$(git rev-parse --show-toplevel) || fail 'Run inside a RoamVM checkout'
test -f "$root/test/libvirt/config.nix" || fail 'Run inside a RoamVM checkout'
# The underlying harness deliberately requires the checkout root.
cd "$root"
case "${1:-}" in
  list)
    [[ $# == 1 ]] || fail 'list takes no arguments'
    for file in test/scenarios/*/scenario.yaml; do basename "$(dirname "$file")"; done
    ;;
  run)
    [[ $# == 2 ]] || fail 'Expected run SCENARIO|all'
    if [[ $2 == all ]]; then exec bash test/libvirt/scenario.sh run-all; fi
    [[ $2 =~ ^[a-zA-Z0-9_-]+$ && -f test/scenarios/$2/scenario.yaml ]] || fail "Unknown scenario: $2"
    exec bash test/libvirt/scenario.sh run "$2"
    ;;
  lab)
    [[ $# == 2 ]] || fail 'Expected lab COMMAND'
    case "$2" in
      plan|up|fixtures|down|destroy) exec bash test/libvirt/lab.sh "$2" ;;
      install|check) exec bash "test/libvirt/$2.sh" ;;
      freeze|freeze-warm|reset|reset-cold) exec bash test/libvirt/scenario.sh "$2" ;;
      *) fail "Unknown lab command: $2" ;;
    esac
    ;;
  *) fail "Unknown command: $1" ;;
esac
