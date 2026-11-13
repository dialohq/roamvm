qemu:
qemu.overrideAttrs (old: {
  patches = (old.patches or []) ++ [./qemu-external-ram.patch];
  configureFlags = (old.configureFlags or []) ++ ["--with-pkgversion=roamvm-external-ram-v1"];
})
