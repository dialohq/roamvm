#!/usr/bin/env bash
set -euo pipefail
export PATH=/run/wrappers/bin:$PATH
export LIBVIRT_DEFAULT_URI=qemu:///session
root=$(git rev-parse --show-toplevel)
lab="$root/.lab/libvirt"
names=(roamvm-libvirt-control-plane roamvm-libvirt-worker roamvm-libvirt-worker2)
ips=(192.168.124.10 192.168.124.11 192.168.124.12)
ssh_options=(-i "$lab/id_ed25519" -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$lab/known_hosts" -o ConnectTimeout=5)
case "${1:-}" in
  ssh)
    for i in 0 1 2; do
      if [[ "$2" == "${names[$i]}" ]]; then
        shift 2
        exec ssh "${ssh_options[@]}" "root@${ips[$i]}" "$@"
      fi
    done
    echo "Not a lab node: $2" >&2; exit 1 ;;
  up)
    test -c /dev/kvm
    mkdir -p "$lab"
    if [[ ! -f "$lab/id_ed25519" ]]; then ssh-keygen -q -t ed25519 -N '' -f "$lab/id_ed25519"; fi
    make build
    # A private bridge and pre-created TAPs let unprivileged session libvirt
    # own the VMs without a privileged libvirt daemon or bridge helper.
    if ! ip link show rvm-lab >/dev/null 2>&1; then
      sudo ip link add rvm-lab type bridge
      sudo ip addr add 192.168.124.1/24 dev rvm-lab
      sudo ip link set rvm-lab up
    fi
    sudo sysctl -w net.ipv4.ip_forward=1
    sudo iptables -t nat -C POSTROUTING -s 192.168.124.0/24 ! -d 192.168.124.0/24 -j MASQUERADE 2>/dev/null ||
      sudo iptables -t nat -A POSTROUTING -s 192.168.124.0/24 ! -d 192.168.124.0/24 -j MASQUERADE
    for direction in -i -o; do
      sudo iptables -C FORWARD "$direction" rvm-lab -j ACCEPT 2>/dev/null ||
        sudo iptables -I FORWARD "$direction" rvm-lab -j ACCEPT
    done
    for i in 0 1 2; do
      name=${names[$i]}
      if ! ip link show "rvm-tap$i" >/dev/null 2>&1; then
        sudo ip tuntap add "rvm-tap$i" mode tap user "$(id -u)"
        sudo ip link set "rvm-tap$i" master rvm-lab up
      fi
      if virsh dominfo "$name" >/dev/null 2>&1; then
        test -f "$lab/$name.xml" || { echo "Refusing existing domain $name" >&2; exit 1; }
        if [[ $(virsh domstate "$name") == 'shut off' ]]; then virsh start "$name"; fi
        continue
      fi
      nix build ".#nixosConfigurations.$name.config.system.build.toplevel" --out-link "$lab/$name-system"
      system=$(readlink -f "$lab/$name-system")
      if [[ ! -f "$lab/$name.qcow2" ]]; then
        truncate -s 16G "$lab/$name.raw.partial"
        mkfs.ext4 -q -F "$lab/$name.raw.partial"
        qemu-img convert -f raw -O qcow2 -c "$lab/$name.raw.partial" "$lab/$name.qcow2"
        rm "$lab/$name.raw.partial"
      fi
      memory=3072
      if [[ $i == 0 ]]; then memory=2048; fi
      cat > "$lab/$name.xml" <<XML
<domain type='kvm'>
  <name>$name</name>
  <memory unit='MiB'>$memory</memory><vcpu>4</vcpu>
  <cpu mode='host-passthrough'/>
  <os><type arch='x86_64'>hvm</type>
    <kernel>$system/kernel</kernel><initrd>$system/initrd</initrd>
    <cmdline>init=$system/init console=ttyS0 net.ifnames=0</cmdline>
  </os>
  <features><acpi/><apic/></features>
  <devices>
    <emulator>$(command -v qemu-system-x86_64)</emulator>
    <disk type='file' device='disk'><driver name='qemu' type='qcow2' cache='none' discard='unmap'/><source file='$lab/$name.qcow2'/><target dev='vda' bus='virtio'/></disk>
    <filesystem type='mount' accessmode='passthrough'><source dir='/nix/store'/><target dir='nix-store'/><readonly/></filesystem>
    <filesystem type='mount' accessmode='passthrough'><source dir='$root'/><target dir='lab'/><readonly/></filesystem>
    <interface type='ethernet'><target dev='rvm-tap$i' managed='no'/><model type='virtio'/></interface>
    <serial type='file'><source path='$lab/$name.console'/><target port='0'/></serial>
  </devices>
</domain>
XML
      virsh define "$lab/$name.xml"
      virsh start "$name"
    done
    for i in 0 1 2; do
      for attempt in $(seq 1 90); do
        if ssh "${ssh_options[@]}" "root@${ips[$i]}" true; then break; fi
        if [[ $attempt == 90 ]]; then exit 1; fi
        sleep 2
      done
    done
    ssh "${ssh_options[@]}" root@192.168.124.10 cat /etc/rancher/k3s/k3s.yaml |
      sed -e 's/127.0.0.1/192.168.124.10/g' -e 's/default/roamvm-libvirt/g' > "$lab/kubeconfig"
    chmod 600 "$lab/kubeconfig"
    kubectl --kubeconfig "$lab/kubeconfig" wait nodes --all --for=condition=Ready --timeout=180s
    ;;
  down)
    for name in "${names[@]}"; do
      test -f "$lab/$name.xml" || continue
      if [[ $(virsh domstate "$name") != 'shut off' ]]; then virsh destroy "$name"; fi
      virsh undefine "$name"
    done
    for i in 0 1 2; do sudo ip link del "rvm-tap$i"; done
    sudo ip link del rvm-lab
    sudo iptables -t nat -D POSTROUTING -s 192.168.124.0/24 ! -d 192.168.124.0/24 -j MASQUERADE
    for direction in -i -o; do sudo iptables -D FORWARD "$direction" rvm-lab -j ACCEPT; done
    echo "Stopped lab; disks and SSH keys retained in $lab"
    ;;
  *) echo "Usage: $0 up|down|ssh NODE COMMAND..." >&2; exit 1 ;;
esac
