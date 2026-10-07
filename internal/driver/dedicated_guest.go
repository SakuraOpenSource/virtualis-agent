package driver

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

type staticGuestFile struct {
	Path, Content string
	Mode          os.FileMode
}

// The provisioned guest has one owned NIC. Match by deterministic MAC, not a
// distribution-specific eth0/ens3 name. Keep desired pool CIDR in the API.
func dedicatedGuestFiles(network protocol.NetworkConfig, a dedicatedAttachment) []staticGuestFile {
	address, gateway := network.IPv4, network.Gateway
	if !strings.Contains(address, "/") {
		address += "/32"
	}
	if a.Mode == "routed" {
		address = a.IP + "/32"
		gateway = routedGateway
	}
	mac := strings.ToLower(network.MAC)
	match := "Name=eth0\n"
	if mac != "" {
		match = "MACAddress=" + mac + "\n"
	}
	var dns strings.Builder
	for _, d := range network.DNS {
		fmt.Fprintf(&dns, "DNS=%s\n", d)
	}
	networkd := "[Match]\n" + match + "[Network]\nDHCP=no\nIPv6AcceptRA=no\nLinkLocalAddressing=no\nAddress=" + address + "\n" + dns.String() + "[Route]\nDestination=" + gateway + "/32\nScope=link\n[Route]\nDestination=0.0.0.0/0\nGateway=" + gateway + "\nGatewayOnLink=yes\n"
	script := fmt.Sprintf(`#!/bin/sh
set -eu
nic=eth0
for p in /sys/class/net/*; do
  [ "$(cat "$p/address")" = %s ] && nic="${p##*/}"
done
ip link set "$nic" up
# Static network managers below disable DHCP persistently. Release an old lease
# on this NIC only; never solicit a new lease for a routed guest.
if command -v dhclient >/dev/null 2>&1; then dhclient -r "$nic" || true; fi
ip -4 address flush dev "$nic" scope global
ip address replace %s dev "$nic"
ip route replace %s/32 dev "$nic" scope link
ip route replace default via %s dev "$nic" onlink
`, shellQuote(mac), address, gateway, gateway)
	unit := "[Unit]\nDescription=Virtualis persistent static guest network\nAfter=network-online.target\nWants=network-online.target\nBefore=sshd.service ssh.service\n[Service]\nType=oneshot\nExecStart=" + guestNetScriptPath + "\nRemainAfterExit=yes\n[Install]\nWantedBy=multi-user.target\n"
	nm := "[keyfile]\nunmanaged-devices=interface-name:eth0\n"
	if mac != "" {
		nm = "[keyfile]\nunmanaged-devices=mac:" + mac + "\n"
	}
	// ifupdown addresses this NIC statically too. networkd takes its first match;
	// NetworkManager permanently leaves only the owned interface unmanaged.
	interfaces := "auto lo\niface lo inet loopback\nauto eth0\niface eth0 inet static\n    address " + address + "\n    post-up ip route replace " + gateway + "/32 dev eth0 scope link\n    post-up ip route replace default via " + gateway + " dev eth0 onlink\n"
	resolv := ""
	for _, d := range network.DNS {
		resolv += "nameserver " + d + "\n"
	}
	files := []staticGuestFile{{guestNetScriptPath, script, 0755}, {guestNetServicePath, unit, 0644}, {"/etc/systemd/network/00-virtualis.network", networkd, 0644}, {"/etc/NetworkManager/conf.d/00-virtualis-unmanaged.conf", nm, 0644}, {"/etc/cloud/cloud.cfg.d/99-virtualis-network.cfg", "network: {config: disabled}\n", 0644}, {"/etc/network/interfaces", interfaces, 0644}}
	if resolv != "" {
		files = append(files, staticGuestFile{"/etc/resolv.conf", resolv, 0644})
	}
	return files
}

func injectDedicatedGuest(ctx context.Context, disk string, network protocol.NetworkConfig, a dedicatedAttachment) error {
	if !commandAvailable(ctx, "virt-customize") {
		return fmt.Errorf("独立静态网络需要 virt-customize 离线注入；未安装，拒绝静默 DHCP")
	}
	dir, e := os.MkdirTemp("", "virtualis-static-guest-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	args := []string{"-a", disk, "--no-network"}
	for n, f := range dedicatedGuestFiles(network, a) {
		p := filepath.Join(dir, fmt.Sprintf("file-%d", n))
		if e = os.WriteFile(p, []byte(f.Content), 0600); e != nil {
			return e
		}
		args = append(args, "--mkdir", filepath.Dir(f.Path), "--upload", p+":"+f.Path, "--chmod", fmt.Sprintf("%04o:%s", f.Mode, f.Path))
	}
	args = append(args, "--run-command", "mkdir -p /etc/systemd/system/multi-user.target.wants && ln -sfn "+guestNetServicePath+" /etc/systemd/system/multi-user.target.wants/virtualis-net.service")
	if e = run(ctx, "virt-customize", args...); e != nil {
		return fmt.Errorf("持久化独立静态网络失败: %w", e)
	}
	return nil
}

func (d *Incus) persistDedicatedGuest(ctx context.Context, inst *protocol.Instance) error {
	a, err := resolveDedicated(ctx, inst.Network)
	if err != nil {
		return err
	}
	var script strings.Builder
	script.WriteString("set -eu\ncommand -v systemctl >/dev/null\n")
	for _, f := range dedicatedGuestFiles(inst.Network, a) {
		fmt.Fprintf(&script, "mkdir -p %s\nprintf '%%s' %s > %s\nchmod %04o %s\n", shellQuote(filepath.Dir(f.Path)), shellQuote(f.Content), shellQuote(f.Path), f.Mode, shellQuote(f.Path))
	}
	script.WriteString("systemctl daemon-reload\nsystemctl enable virtualis-net.service\n" + guestNetScriptPath + "\n")
	if err = run(ctx, d.cli(), "exec", resourceName("incus", inst), "--", "sh", "-c", script.String()); err != nil {
		return fmt.Errorf("持久化独立网络失败（VM 需要 Incus guest agent，宿主 Incus 必须支持 routed NIC）: %w", err)
	}
	return nil
}

func qemuInstanceInterfaceXML(inst *protocol.Instance) string {
	n := inst.Network
	if NormalizeNetworkMode(n.Mode) != NetworkModeDedicated {
		return qemuInterfaceXML(n)
	}
	bridge := n.Bridge
	if n.DedicatedMode == "routed" || ((n.DedicatedMode == "" || n.DedicatedMode == "auto") && !isLinuxBridge(bridge)) {
		bridge = ownedBridge(inst.ID)
	}
	return fmt.Sprintf("    <interface type='bridge'>%s<source bridge='%s'/><target dev='%s'/>%s<model type='virtio'/></interface>\n", macXML(n.MAC), html.EscapeString(bridge), ownedNIC(inst.ID), bandwidthXML(n.BandwidthMbps))
}
