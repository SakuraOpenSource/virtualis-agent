package driver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func dedicatedPersistenceScript(t *testing.T) string {
	t.Helper()
	var calls []string
	base := dedicatedRunner(t, &calls)
	var script string
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if len(args) == 6 && args[0] == "exec" && args[3] == "sh" && args[4] == "-c" {
			script = args[5]
		}
		return base(ctx, command, args...)
	})
	if err := NewIncusWithDataDir(t.TempDir()).persistDedicatedGuest(ctx, dedicatedReadyInstance("vm")); err != nil {
		t.Fatal(err)
	}
	if script == "" {
		t.Fatal("未生成 guest 持久化脚本")
	}
	return script
}

func TestIncusDedicatedPersistenceResolvConfShell(t *testing.T) {
	script := dedicatedPersistenceScript(t)
	for _, fixture := range []struct {
		name, target, content string
		preserve, relative    bool
	}{
		{name: "active-resolved-plain-stale", content: "nameserver 192.0.2.53\n"},
		{name: "active-resolved-stub-symlink", target: "/run/systemd/resolve/stub-resolv.conf", content: "nameserver 127.0.0.53\n", preserve: true},
		{name: "active-resolved-generated-symlink", target: "/run/systemd/resolve/resolv.conf", content: "nameserver 203.0.113.53\n", preserve: true},
		{name: "active-resolved-library-stub-symlink", target: "/usr/lib/systemd/resolv.conf", content: "nameserver 127.0.0.53\n", preserve: true},
		{name: "active-resolved-relative-stub-symlink", target: "/run/systemd/resolve/stub-resolv.conf", content: "nameserver 127.0.0.53\n", preserve: true, relative: true},
		{name: "active-resolved-plain-stub", content: "nameserver 127.0.0.53\n", preserve: true},
		{name: "active-resolved-plain-proxy-stub", content: "nameserver 127.0.0.54\n", preserve: true},
		{name: "active-resolved-stub-comment-is-not-stub", content: "# nameserver 127.0.0.53\nnameserver 192.0.2.53\n"},
		{name: "active-resolved-other-symlink", target: "/etc/static-resolv.conf", content: "nameserver 192.0.2.53\n"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := t.TempDir()
			resolvPath := filepath.Join(root, "etc/resolv.conf")
			if err := os.MkdirAll(filepath.Dir(resolvPath), 0755); err != nil {
				t.Fatal(err)
			}
			linkTarget := ""
			if fixture.target == "" {
				if err := os.WriteFile(resolvPath, []byte(fixture.content), 0644); err != nil {
					t.Fatal(err)
				}
			} else {
				targetPath := root + fixture.target
				if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(targetPath, []byte(fixture.content), 0644); err != nil {
					t.Fatal(err)
				}
				linkTarget = targetPath
				if fixture.relative {
					var err error
					linkTarget, err = filepath.Rel(filepath.Dir(resolvPath), targetPath)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(linkTarget, resolvPath); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(root, "commands.log")
			for _, name := range []string{"ip", "systemctl", "networkctl", "resolvectl", "dhclient", "cat"} {
				stub := "#!/bin/sh\nprintf '%s\\n' \"" + name + " $*\" >> \"$BOOT_LOG\"\n"
				if name == "cat" {
					stub += "printf '%s\\n' '02:00:00:00:00:01'\n"
				}
				if name == "networkctl" {
					stub += "if [ \"$1\" = --help ]; then printf 'Commands:\\n  reload\\n  reconfigure DEVICES\\n'; fi\n"
				}
				if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0755); err != nil {
					t.Fatal(err)
				}
			}
			// 仅把 guest 绝对路径映射到临时根；执行真实的生成脚本、
			// shell 分支、文件写入和 symlink 判断，不触碰宿主网络或 /etc。
			sandboxed := strings.NewReplacer(
				"/etc", root+"/etc",
				"/usr/local/sbin", root+"/usr/local/sbin",
				"/run/systemd/resolve", root+"/run/systemd/resolve",
				"/usr/lib/systemd", root+"/usr/lib/systemd",
			).Replace(script)
			cmd := exec.Command("/bin/sh", "-c", sandboxed)
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "BOOT_LOG="+logPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("guest 持久化 shell 探针失败: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(resolvPath)
			if err != nil {
				t.Fatal(err)
			}
			want := "nameserver 1.1.1.1\n"
			if fixture.preserve {
				want = fixture.content
			}
			if string(raw) != want {
				t.Errorf("resolv.conf DNS 不符合实际接入模式: got %q want %q", raw, want)
			}
			if fixture.target != "" {
				if target, err := os.Readlink(resolvPath); err != nil || target != linkTarget {
					t.Errorf("破坏 resolved symlink: target=%q err=%v", target, err)
				}
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []string{"resolvectl dns eth0 1.1.1.1", "resolvectl domain eth0 ~."} {
				if !strings.Contains(string(log), call) {
					t.Errorf("active resolved 未获得 per-link DNS: 缺少 %s\n%s", call, log)
				}
			}
		})
	}
}
