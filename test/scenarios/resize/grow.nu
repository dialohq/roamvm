use std/assert
use ../harness.nu [checked wait-until state data]
use ../vm.nu [request]

def storage [] {
  request /storage
  open --raw (data response) | from json
}

export def main [] {
  let before = (state).snapshots.initial.response | from json
  let name = (state).vm.metadata.name
  for size in [2147483648 3221225472 4294967296] {
    request "/growth?action=hold" ""
    wait-until { (storage).growthHeld }

    checked { kubectl patch virtualmachine $name --type merge --patch $'{"spec":{"rootDiskSize":"($size)"}}' } | ignore
    wait-until {
      let sample = storage
      $sample.growthHeld and $sample.diskSectors == $"($size // 512)\n"
    }

    request "/growth?action=release" ""
    wait-until {
      let sample = storage
      assert equal $sample.bootID $before.bootID
      assert equal $sample.pid $before.pid
      assert ($sample.uptime > $before.uptime)
      assert ($sample.filesystemBytes > ($size * 9 // 10))
      assert equal $sample.diskSectors $"($size // 512)\n"
      true
    } 30sec
    request "/growth?action=check" ""
    wait-until {
      let vm = checked { kubectl get virtualmachine $name -o json } | from json
      $vm.status.rootDiskSize == $size and ($vm.status.conditions | any {|c| $c.type == DiskReady and $c.status == "True" })
    }
  }
}
