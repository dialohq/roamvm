package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Link-local-only interfaces are treated as offline by common network monitors.
const GuestIP = "192.168.127.2"
const gatewayIP = "192.168.127.1"

func command(ctx context.Context, bin string, args ...string) error {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", bin, args, err, out)
	}
	return nil
}
func network(ctx context.Context) (*exec.Cmd, error) {
	forward, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil || strings.TrimSpace(string(forward)) != "1" {
		return nil, fmt.Errorf("Pod needs net.ipv4.ip_forward=1 (allow this namespaced sysctl on runtime nodes)")
	}
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		return nil, fmt.Errorf("CNI interface eth0: %w", err)
	}
	mtu := strconv.Itoa(iface.MTU)
	calls := [][]string{
		{"ip", "tuntap", "add", "dev", "vm-tap", "mode", "tap"},
		{"ip", "addr", "add", gatewayIP + "/30", "dev", "vm-tap"},
		{"ip", "link", "set", "vm-tap", "mtu", mtu, "up"},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", GuestIP + "/32", "-o", "eth0", "-j", "MASQUERADE"},
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", "eth0", "-j", "DNAT", "--to-destination", GuestIP},
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-d", "127.0.0.1/32", "-j", "DNAT", "--to-destination", GuestIP},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "127.0.0.0/8", "-o", "vm-tap", "-j", "SNAT", "--to-source", gatewayIP},
		{"iptables", "-A", "FORWARD", "-i", "eth0", "-o", "vm-tap", "-j", "ACCEPT"},
		{"iptables", "-A", "FORWARD", "-i", "vm-tap", "-o", "eth0", "-j", "ACCEPT"},
	}
	for _, a := range calls {
		if err := command(ctx, a[0], a[1:]...); err != nil {
			return nil, err
		}
	}
	// The guest uses the Pod resolver (including CoreDNS search suffixes). No CNI
	// interface, route or Pod IP is reassigned; ingress is forwarded to virtio-net.
	resolver, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	dns := ""
	search := ""
	for _, line := range strings.Split(string(resolver), "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == "nameserver" && dns == "" {
			dns = f[1]
		}
		if len(f) > 1 && f[0] == "search" {
			search = strings.Join(f[1:], ",")
		}
	}
	args := []string{"--no-daemon", "--log-facility=-", "--port=0", "--interface=vm-tap", "--bind-interfaces", "--except-interface=lo", "--dhcp-range=" + GuestIP + "," + GuestIP + ",255.255.255.252,12h", "--dhcp-option=option:router," + gatewayIP, "--dhcp-option=option:dns-server," + dns, "--dhcp-option=26," + mtu, "--dhcp-authoritative", "--user=root", "--no-hosts", "--pid-file=", "--dhcp-leasefile=/tmp/dnsmasq.leases"}
	if search != "" {
		args = append(args, "--dhcp-option=option:domain-search,"+search)
	}
	cmd := exec.Command("dnsmasq", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
