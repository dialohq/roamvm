{slot ? 0}:
assert builtins.isInt slot && slot >= 0 && slot <= 99; let
  cluster =
    "roamvm-libvirt"
    + (
      if slot == 0
      then ""
      else "-${toString slot}"
    );
  subnet = "192.168.${toString (124 + slot)}";
  macSlot =
    (
      if slot < 10
      then "0"
      else ""
    )
    + toString slot;
in {
  inherit cluster slot;
  uri = "qemu:///system";
  network = {
    name = "roamvm-lab-${toString slot}";
    bridge = "rvm-lab${toString slot}";
    gateway = "${subnet}.1";
    prefix = 24;
  };
  nodes = {
    "${cluster}-control-plane" = {
      role = "server";
      ip = "${subnet}.10";
      mac = "52:54:00:72:${macSlot}:10";
      memory = 2048;
      cpus = 4;
    };
    "${cluster}-worker" = {
      role = "agent";
      ip = "${subnet}.11";
      mac = "52:54:00:72:${macSlot}:11";
      memory = 2048;
      cpus = 4;
    };
    "${cluster}-worker2" = {
      role = "agent";
      ip = "${subnet}.12";
      mac = "52:54:00:72:${macSlot}:12";
      memory = 2048;
      cpus = 4;
    };
  };
  scenarios = {
    crash = {tests = ["TestLocalCrashRecovery"];};
    lifecycle = {tests = ["TestLifecycle"];};
    network = {tests = ["TestKubernetes"];};
    cpu = {tests = ["TestOversubscription"];};
    resize = {
      tests = ["TestOnlineResize" "TestResizeWithoutExpandableStorage"];
      fixtures.ROAMVM_TEST_RESIZE_IMAGE = "nixos";
    };
    generations = {
      tests = ["TestNixOSGenerations"];
      fixtures = {
        ROAMVM_TEST_GENERATION_IMAGE = "generation";
        ROAMVM_TEST_FIRMWARE_IMAGE = "firmware";
      };
    };
  };
}
