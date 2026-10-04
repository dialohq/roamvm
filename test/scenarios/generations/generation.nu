use std/assert
use ../harness.nu [data]

export def main [expected: string, cmdline: string = ""] {
  let generation = open --raw (data response) | from json
  assert equal $generation.hostname generation-workspace
  assert equal $generation.generation $expected
  if $cmdline != "" { assert ($generation.cmdline | str contains $cmdline) }
  $generation
}
