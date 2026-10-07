package driver

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

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
mac=%s
if [ -n "$mac" ]; then
  for p in /sys/class/net/*; do
    [ -r "$p/address" ] || continue
    if [ "$(cat "$p/address")" = "$mac" ]; then
      nic="${p##*/}"
      break
    fi
  done
fi
ip link set "$nic" up
# Reload the static profile before dropping the old lease, otherwise a running
# DHCP client can remove the routed address again after this script returns.
if command -v networkctl >/dev/null 2>&1 && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet systemd-networkd; then
  if ! networkctl_help="$(networkctl --help)"; then
    echo 'active systemd-networkd requires networkctl reload and reconfigure; failed to inspect capabilities' >&2
    exit 1
  fi
  if ! printf '%%s\n' "$networkctl_help" | grep -Eq '^[[:space:]]*reload([[:space:]]|$)' || ! printf '%%s\n' "$networkctl_help" | grep -Eq '^[[:space:]]*reconfigure([[:space:]]|$)'; then
    echo 'active systemd-networkd requires networkctl reload and reconfigure; upgrade systemd' >&2
    exit 1
  fi
  networkctl reload
  networkctl reconfigure "$nic"
fi
# Static network managers below disable DHCP persistently. Release an old lease
# on this NIC only; never solicit a new lease for a routed guest.
if command -v dhclient >/dev/null 2>&1; then dhclient -r "$nic" || true; fi
ip -4 address flush dev "$nic" scope global
ip address replace %s dev "$nic"
ip route replace %s/32 dev "$nic" scope link
ip route replace default via %s dev "$nic" onlink
`, shellQuote(mac), address, gateway, gateway)
	if len(network.DNS) > 0 {
		script += "if command -v resolvectl >/dev/null 2>&1 && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet systemd-resolved; then\n  resolvectl dns \"$nic\" " + strings.Join(network.DNS, " ") + "\n  resolvectl domain \"$nic\" '~.'\nfi\n"
	}
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

const dedicatedGuestInitProbe = `command -v systemctl >/dev/null || exit 127
if ! systemctl show-environment >/dev/null 2>&1 || { [ -L /etc/resolv.conf ] && [ ! -e /etc/resolv.conf ]; }; then
  echo 'virtualis guest init not ready' >&2
  exit 75
fi
`

// launch/restart 返回时 VM agent 或 guest init 可能尚未启动。只重试
// 无副作用的就绪探针；配置写入和网络命令仍只执行一次，真实错误直接返回。
func (d *Incus) waitDedicatedGuestReady(ctx context.Context, name string) error {
	waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("等待独立网络 guest 就绪失败: %w", err)
		}
		out, err := output(waitCtx, d.cli(), "exec", name, "--", "true")
		if err == nil {
			// exec 可用不代表 PID 1 的 bus 和 resolved 的运行时文件已就绪；
			// 精简容器首启时不能提前写入仍悬空的 resolv.conf symlink。
			out, err = output(waitCtx, d.cli(), "exec", name, "--", "sh", "-c", dedicatedGuestInitProbe)
		}
		if ctxErr := waitCtx.Err(); ctxErr != nil {
			return fmt.Errorf("等待独立网络 guest 就绪失败: %w", ctxErr)
		}
		if err == nil {
			return nil
		}
		// run 的错误包含脚本参数；暂态分类只能使用实际输出和原始错误。
		message := strings.ToLower(string(out) + "\n" + err.Error())
		if !strings.Contains(message, "vm agent isn't currently running") && !strings.Contains(message, "instance is not running") && !strings.Contains(message, "virtualis guest init not ready") {
			return fmt.Errorf("等待独立网络 guest 就绪失败: %w: %s", err, strings.TrimSpace(string(out)))
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return fmt.Errorf("等待独立网络 guest 就绪失败: %w（最后错误: %v: %s）", waitCtx.Err(), err, strings.TrimSpace(string(out)))
		case <-timer.C:
		}
	}
}

func (d *Incus) persistDedicatedGuest(ctx context.Context, inst *protocol.Instance) error {
	a, err := resolveDedicated(ctx, inst.Network)
	if err != nil {
		return err
	}
	if err := d.waitDedicatedGuestReady(ctx, resourceName("incus", inst)); err != nil {
		return err
	}
	var script strings.Builder
	script.WriteString("set -eu\ncommand -v systemctl >/dev/null\n")
	for _, f := range dedicatedGuestFiles(inst.Network, a) {
		// resolved 活跃不等于 resolv.conf 接入了它。仅保留它管理的
		// 文件或实际指向本地 stub 的 DNS；普通静态文件仍更新期望 DNS。
		if f.Path == "/etc/resolv.conf" {
			script.WriteString(`resolved_resolv=false
if command -v resolvectl >/dev/null 2>&1 && systemctl is-active --quiet systemd-resolved; then
  for resolved_file in '/run/systemd/resolve/stub-resolv.conf' '/run/systemd/resolve/resolv.conf' '/usr/lib/systemd/resolv.conf'; do
    if [ '/etc/resolv.conf' -ef "$resolved_file" ]; then
      resolved_resolv=true
      break
    fi
  done
  if [ -f '/etc/resolv.conf' ] && grep -Eq '^[[:space:]]*nameserver[[:space:]]+127[.]0[.]0[.](53|54)([[:space:]]|$)' '/etc/resolv.conf'; then
    resolved_resolv=true
  fi
fi
if [ "$resolved_resolv" = false ]; then
`)
		}
		fmt.Fprintf(&script, "mkdir -p %s\nprintf '%%s' %s > %s\nchmod %04o %s\n", shellQuote(filepath.Dir(f.Path)), shellQuote(f.Content), shellQuote(f.Path), f.Mode, shellQuote(f.Path))
		if f.Path == "/etc/resolv.conf" {
			script.WriteString("fi\n")
		}
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
