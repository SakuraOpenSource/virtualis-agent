package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func runDedicatedNetworkctlProbe(t *testing.T, help, helpStatus, fail string) (string, error, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "commands.log")
	for _, name := range []string{"ip", "systemctl", "networkctl", "resolvectl", "dhclient", "cat"} {
		stub := "#!/bin/sh\nprintf '%s\\n' \"" + name + " $*\" >> \"$BOOT_LOG\"\n"
		switch name {
		case "cat":
			stub += "printf '%s\\n' '02:00:00:00:00:01'\n"
		case "networkctl":
			stub += `if [ "$1" = --help ]; then
  printf '%s\n' "$NETWORKCTL_HELP"
  exit "$NETWORKCTL_HELP_STATUS"
fi
case "$NETWORKCTL_HELP" in
  *"$1"*) ;;
  *) echo "Unknown command verb '$1'" >&2; exit 64 ;;
esac
if [ "$1" = "$NETWORKCTL_FAIL" ]; then
  echo "networkctl $1 failed" >&2
  exit 42
fi
`
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	var script string
	for _, file := range dedicatedGuestFiles(protocol.NetworkConfig{IPv4: "192.0.2.20/32", DNS: []string{"1.1.1.1"}}, dedicatedAttachment{Mode: "routed", IP: "192.0.2.20"}) {
		if file.Path == guestNetScriptPath {
			script = file.Content
		}
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "BOOT_LOG="+logPath,
		"NETWORKCTL_HELP="+help, "NETWORKCTL_HELP_STATUS="+helpStatus, "NETWORKCTL_FAIL="+fail)
	out, err := cmd.CombinedOutput()
	calls, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(out), err, string(calls)
}

func TestDedicatedGuestScriptRejectsUnsupportedNetworkctl(t *testing.T) {
	for _, fixture := range []struct{ name, help, status string }{
		{"missing-reload", "Commands:\n  reconfigure DEVICES\n", "0"},
		{"missing-reconfigure", "Commands:\n  reload\n", "0"},
		{"capabilities-unavailable", "", "1"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			out, err, calls := runDedicatedNetworkctlProbe(t, fixture.help, fixture.status, "")
			if err == nil || !strings.Contains(out, "active systemd-networkd requires networkctl reload and reconfigure") {
				t.Errorf("不支持的 networkctl 必须明确拒绝: err=%v out=%q", err, out)
			}
			for _, forbidden := range []string{"networkctl reload\n", "networkctl reconfigure eth0\n", "ip -4 address flush", "dhclient -r"} {
				if strings.Contains(calls, forbidden) {
					t.Errorf("能力检查失败后不得尝试配置或释放租约: %s\n%s", forbidden, calls)
				}
			}
		})
	}
}

func TestDedicatedGuestScriptPreservesNetworkctlApplyErrors(t *testing.T) {
	for _, verb := range []string{"reload", "reconfigure"} {
		t.Run(verb, func(t *testing.T) {
			out, err, calls := runDedicatedNetworkctlProbe(t, "Commands:\n  reload\n  reconfigure DEVICES\n", "0", verb)
			if err == nil || !strings.Contains(out, "networkctl "+verb+" failed") {
				t.Fatalf("networkctl 执行错误被吞掉: err=%v out=%q", err, out)
			}
			if strings.Contains(calls, "ip -4 address flush") || strings.Contains(calls, "dhclient -r") {
				t.Fatalf("networkctl 执行失败后仍继续改网络: %s", calls)
			}
		})
	}
}
