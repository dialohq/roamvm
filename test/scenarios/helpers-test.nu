use std/assert
use harness.nu [state values save-values guest-request data]
use vm.nu [observe observe-storage request write-data capture]

let scratch = mktemp -d
let root = $env.FILE_PWD
let failure = try {
  mkdir ($scratch | path join bin)
  # Mock only the transport boundary; unexpected cluster reads fail the test.
  '#!/bin/sh
if [ "${FAIL_REQUEST:-0}" = 1 ]; then echo unavailable >&2; exit 7; fi
if [ "$1" = exec ]; then
  case " $* " in
    *" --data-binary @- "*) cat ;;
    *) printf "GET\n" ;;
  esac
elif [ "$1 $2" = "get virtualmachine" ]; then
  printf "{\"metadata\":{\"uid\":\"fresh\"},\"status\":{}}"
else exit 90
fi
' | save ($scratch | path join bin kubectl)
  '#!/bin/sh
echo unexpected-object-store-access >&2
exit 91
' | save ($scratch | path join bin mc)
  ^chmod +x ($scratch | path join bin kubectl) ($scratch | path join bin mc)
  with-env {PATH: ($env.PATH | prepend ($scratch | path join bin)), SCENARIO_DATA: $scratch, SCENARIO_STATE: ($scratch | path join state.json), S3_BUCKET: fixture} {
    mkdir (data mc)
    touch (data mc/.scenario-configured)
    {selected: fixture, probe: probe, deleted: false, paused: false, snapshots: {}, values: {}, head: null} | to json | save $env.SCENARIO_STATE
    save-values {worker: node-a}
    observe
    assert equal (state).values {worker: node-a}
    assert equal (state).uid fresh
    save-values ((values) | upsert counter 3)
    assert equal (state).uid fresh
    assert equal (state).values.counter 3
    assert not (data values.json | path exists)

    # Missing body is GET; an empty string is still POST, not GET.
    assert equal (guest-request fixture /data) "GET\n"
    assert equal (guest-request fixture /data "") ""
    let bytes = 0x[00FFFE610A0D]
    $bytes | save (data payload-binary)
    write-data binary
    assert equal (open --raw (data response)) $bytes
    state | upsert paused true | to json | save --force $env.SCENARIO_STATE
    capture binary
    assert not ('responseFile' in (state).snapshots.binary)
    assert equal (state).values.counter 3
    request /data "text\n"
    assert equal (open --raw (data response)) "text\n"
    capture text
    assert equal (state).snapshots.text.response "text\n"
    assert (with-env {FAIL_REQUEST: "1"} { try { request /data; false } catch { true } })

    # A response-only assertion works even with an unavailable cluster/store.
    with-env {FAIL_REQUEST: "1"} {
      let result = do { nu --no-config-file ($root | path join network/assert.nu) response "text\n" } | complete
      assert equal $result.exit_code 0 $result.stderr
    }
    # Storage assertions must not silently use a previously cached head.
    state | upsert paused false | to json | save --force $env.SCENARIO_STATE
    let error = try { observe-storage; "" } catch {|err| $err.msg }
    assert ($error | str contains unexpected-object-store-access)
  }
  null
} catch {|err| $err }
rm -r $scratch
if $failure != null { error make {msg: $failure.msg, help: $failure.debug} }
print 'Scenario state ownership, targeted reads, and binary transport checks passed.'
