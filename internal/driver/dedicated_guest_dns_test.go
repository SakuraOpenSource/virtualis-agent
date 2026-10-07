package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestDedicatedGuestScriptReconfiguresActiveNetworkManagers(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "commands.log")
	for _, name := range []string{"ip", "systemctl", "networkctl", "resolvectl", "dhclient", "cat"} {
		stub := "#!/bin/sh\nprintf '%s\\n' \"" + name + " $*\" >> \"$BOOT_LOG\"\n"
		if name == "cat" {
			stub += "printf '%s\\n' '02:00:00:00:00:01'\n"
		}
		if name == "networkctl" {
			stub += "if [ \"$1\" = --help ]; then printf 'Commands:\\n  reload\\n  reconfigure DEVICES\\n'; fi\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	network := protocol.NetworkConfig{IPv4: "192.0.2.20/32", DNS: []string{"1.1.1.1", "8.8.8.8"}}
	var script string
	for _, file := range dedicatedGuestFiles(network, dedicatedAttachment{Mode: "routed", IP: "192.0.2.20"}) {
		if file.Path == guestNetScriptPath {
			script = file.Content
		}
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "BOOT_LOG="+logPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guest 网络脚本执行失败：%v，%s", err, out)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(raw)
	for _, want := range []string{"networkctl reload", "networkctl reconfigure eth0", "resolvectl dns eth0 1.1.1.1 8.8.8.8", "resolvectl domain eth0 ~."} {
		if !strings.Contains(calls, want) {
			t.Errorf("静态网络配置未交给正在运行的网络管理器：缺少 %s\n%s", want, calls)
		}
	}
	if reload := strings.Index(calls, "networkctl reload"); reload >= 0 && reload > strings.Index(calls, "networkctl reconfigure eth0") {
		t.Fatal("必须先 reload 新配置，再 reconfigure guest NIC")
	}
}
