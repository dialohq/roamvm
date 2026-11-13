{pkgs}:
pkgs.stdenvNoCC.mkDerivation {
  pname = "amp";
  version = "0.0.1794052841-g6d76b7";
  src = pkgs.fetchurl {
    url = "https://static.ampcode.com/cli/0.0.1794052841-g6d76b7/amp-linux-x64";
    sha256 = "cb0900597781df3f465f69cab1aea0789fda6aa397e2a0e3f28c644f840d7acb";
  };
  dontUnpack = true;
  installPhase = ''install -Dm755 $src $out/bin/amp'';
}
