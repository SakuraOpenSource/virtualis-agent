package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestDedicatedGuestScriptSkipsSysfsNonInterfaces(t *testing.T) {
	for _, fixture := range []struct {
		name, mac, nic string
	}{{"default-nic-without-mac", "", "eth0"}, {"mac-selected-nic", "02:00:00:00:00:01", "ens3"}} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			netdir := filepath.Join(dir, "net")
			if err := os.MkdirAll(filepath.Join(netdir, fixture.nic), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(netdir, "bonding_masters"), nil, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(netdir, fixture.nic, "address"), []byte("02:00:00:00:00:01\n"), 0644); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			ip := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$IP_LOG\"\ncase \"$*\" in *bonding_masters*) exit 1;; esac\n"
			for name, contents := range map[string]string{"ip": ip, "systemctl": "#!/bin/sh\nexit 1\n", "dhclient": "#!/bin/sh\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(contents), 0755); err != nil {
					t.Fatal(err)
				}
			}
			network := protocol.NetworkConfig{IPv4: "192.0.2.20/32", MAC: fixture.mac}
			var script string
			for _, file := range dedicatedGuestFiles(network, dedicatedAttachment{Mode: "routed", IP: "192.0.2.20"}) {
				if file.Path == guestNetScriptPath {
					script = file.Content
				}
			}
			if script == "" {
				t.Fatal("缺少 guest 网络脚本")
			}
			script = strings.ReplaceAll(script, "/sys/class/net/", netdir+"/")
			log := filepath.Join(dir, "ip.log")
			command := exec.Command("sh", "-c", script)
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "IP_LOG="+log)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("sysfs 普通文件不应影响选择 guest 网卡: %v: %s", err, output)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), "bonding_masters") || !strings.Contains(string(calls), "link set "+fixture.nic+" up") {
				t.Fatalf("网卡选择错误: %s", calls)
			}
		})
	}
}
