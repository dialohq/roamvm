{pkgs}: let
  runtime = pkgs.buildGoModule {
    pname = "roamvm";
    version = "lab";
    src = pkgs.lib.fileset.toSource {
      root = ../..;
      fileset = pkgs.lib.fileset.unions [../../go.mod ../../go.sum ../../api ../../cmd ../../internal];
    };
    vendorHash = "sha256-P1t0NmWQAjtvR6t3K+2MyWHt8ymeua4eR78RjYF3GuA=";
    subPackages = ["cmd/roamvm"];
    env.CGO_ENABLED = "0";
    doCheck = false;
  };
in
  pkgs.dockerTools.buildLayeredImage {
    name = "roamvm";
    tag = "libvirt-lab";
    passthru = {inherit runtime;};
    contents = pkgs.buildEnv {
      name = "roamvm-runtime-root";
      paths = with pkgs; [runtime (lib.lowPrio busybox) cacert curl iproute2 iptables dnsmasq qemu_test cdrkit];
      pathsToLink = ["/bin" "/etc"];
    };
    extraCommands = ''
      mkdir -p tmp
      chmod 1777 tmp
      echo 'root:x:0:0:root:/root:/bin/sh' > etc/passwd
      echo 'root:x:0:' > etc/group
      printf 'passwd: files\ngroup: files\nhosts: files dns\n' > etc/nsswitch.conf
    '';
    config = {
      Entrypoint = ["${runtime}/bin/roamvm"];
      Env = ["PATH=/bin" "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"];
    };
  }
