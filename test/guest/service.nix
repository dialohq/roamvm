{pkgs}:
pkgs.buildGoModule {
  pname = "roamvm-test-guest";
  version = "0.1.0";
  src = pkgs.lib.fileset.toSource {
    root = ./.;
    fileset = ./main.go;
  };
  postPatch = ''
    printf 'module guest\n\ngo 1.26.0\n' > go.mod
  '';
  vendorHash = null;
  subPackages = ["."];
  env.CGO_ENABLED = "0";
  ldflags = ["-s" "-w"];
}
