use ../harness.nu [checked wait-until]
use ../vm.nu *

def assertion [name: string, size: int] {
  checked { nu --no-config-file assert.nu $name $size } | ignore
}

export def main [] {
  for size in [2147483648 3221225472 4294967296] {
    request "/growth?action=hold" ""
    wait-until { try { assertion held $size; true } catch { false } }

    let name = (open $env.SCENARIO_STATE).vm.metadata.name
    checked { kubectl patch virtualmachine $name --type merge --patch $'{"spec":{"rootDiskSize":"($size)"}}' } | ignore
    wait-until { try { assertion capacity $size; true } catch { false } }

    request "/growth?action=release" ""
    wait-until { try { assertion grown $size; true } catch { false } } 30sec
    request "/growth?action=check" ""
    wait-until { try { assertion reported $size; true } catch { false } }
  }
}
