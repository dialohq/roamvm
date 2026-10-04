{
  pkgs,
  layout,
  systems,
  virtiofsd,
  ...
}: {
  terraform.required_providers.libvirt = {
    source = "dmacvicar/libvirt";
    version = "= 0.9.9";
  };
  variable.lab.type = "string";
  variable.uri = {
    type = "string";
    default = layout.uri;
  };
  provider.libvirt.uri = "\${var.uri}";
  resource.libvirt_network.lab = {
    name = layout.network.name;
    autostart = true;
    bridge = {
      name = layout.network.bridge;
      stp = "off";
      delay = "0";
    };
    forward.mode = "nat";
    ips = [
      {
        address = layout.network.gateway;
        prefix = layout.network.prefix;
      }
    ];
  };
  resource.libvirt_domain =
    builtins.mapAttrs (name: node: {
      inherit name;
      type = "kvm";
      memory = node.memory;
      memory_unit = "MiB";
      vcpu = node.cpus;
      # Omit running: the harness owns power state, including save/restore.
      cpu = {
        mode = "host-passthrough";
        # Libvirt otherwise presents one socket per vCPU. On AMD without
        # constant_tsc, Linux treats that as an unsynchronized multi-socket
        # system, disabling fast clock reads and degrading nested KVM.
        topology = {
          sockets = 1;
          cores = node.cpus;
          threads = 1;
        };
      };
      memory_backing = {
        memory_source.type = "memfd";
        memory_access.mode = "shared";
      };
      os = {
        type = "hvm";
        type_arch = "x86_64";
        kernel = "${systems.${name}}/kernel";
        initrd = "${systems.${name}}/initrd";
        cmdline = "init=${systems.${name}}/init console=ttyS0 net.ifnames=0";
      };
      features = {
        acpi = true;
        apic = {};
      };
      devices = {
        emulator = "${pkgs.qemu_kvm}/bin/qemu-system-x86_64";
        # The harness owns working overlays and immutable baseline files.
        # Terraform owns the attachment, never the replaceable file contents.
        disks = [
          {
            device = "disk";
            driver = {
              name = "qemu";
              type = "qcow2";
              cache = "none";
              discard = "unmap";
            };
            source.file.file = "\${var.lab}/${name}.qcow2";
            target = {
              dev = "vda";
              bus = "virtio";
            };
          }
        ];
        filesystems =
          map (share: {
            access_mode = "passthrough";
            driver.type = "virtiofs";
            binary = {path = "${virtiofsd}/bin/virtiofsd";} // pkgs.lib.optionalAttrs (share.target == "nix-store") {cache.mode = "always";};
            source.mount.dir = share.source;
            target.dir = share.target;
            readonly = true;
          }) [
            {
              source = "/nix/store";
              target = "nix-store";
            }
            {
              source = "\${var.lab}";
              target = "lab-state";
            }
          ];
        interfaces = [
          {
            source.network.network = "\${libvirt_network.lab.name}";
            model.type = "virtio";
            mac.address = node.mac;
          }
        ];
        mem_balloon = {
          model = "virtio";
          free_page_reporting = "on";
        };
        channels = [
          {
            source.unix = {};
            target.virt_io.name = "org.qemu.guest_agent.0";
          }
        ];
        serials = [
          {
            source.file.path = "\${var.lab}/${name}.console";
            target.port = 0;
          }
        ];
      };
    })
    layout.nodes;
}
