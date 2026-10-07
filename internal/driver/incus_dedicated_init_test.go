package driver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isDedicatedInitProbe(args []string) bool {
	return len(args) == 6 && args[0] == "exec" && strings.Contains(args[5], "systemctl show-environment")
}

// Existing readiness tests model a fully booted init once the transport works.
func withInitializedGuestRunner(ctx context.Context, runner CommandRunner) context.Context {
	return WithCommandRunner(ctx, func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if isDedicatedInitProbe(args) {
			return nil, nil
		}
		return runner(ctx, command, args...)
	})
}

func TestIncusDedicatedGuestWaitsForInitAfterExecTransportWorks(t *testing.T) {
	var calls []string
	base := dedicatedRunner(t, &calls)
	initProbes, persists := 0, 0
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if isDedicatedInitProbe(args) {
			initProbes++
			if initProbes == 1 {
				return []byte("virtualis guest init not ready"), errors.New("exit status 75")
			}
			return nil, nil
		}
		if len(args) > 0 && args[0] == "exec" && !(len(args) == 4 && args[3] == "true") {
			persists++
			if initProbes < 2 {
				return []byte("Failed to connect to system scope bus; dangling resolv.conf"), errors.New("exit status 2")
			}
		}
		return base(ctx, command, args...)
	})
	if err := NewIncusWithDataDir(t.TempDir()).persistDedicatedGuest(ctx, dedicatedReadyInstance("container")); err != nil {
		t.Fatalf("exec transport ready 时仍应等待 init/DNS 路径: %v", err)
	}
	if initProbes != 2 || persists != 1 {
		t.Fatalf("应先等待 init，再只持久化一次: init_probes=%d persists=%d", initProbes, persists)
	}
}

func TestIncusDedicatedInitWaitDoesNotRetryPermanentErrorsWithEmptyOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{name: "permission-denied", failure: errors.New("permission denied")},
		{name: "missing-systemctl", failure: errors.New("exit status 127")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewIncusWithDataDir(t.TempDir())
			var calls []string
			base := dedicatedRunner(t, &calls)
			parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			transportProbes, initProbes, persists := 0, 0, 0
			ctx := WithCommandRunner(parent, func(ctx context.Context, command string, args ...string) ([]byte, error) {
				if len(args) == 4 && args[0] == "exec" && args[3] == "true" {
					transportProbes++
					return nil, nil
				}
				if isDedicatedInitProbe(args) {
					initProbes++
					return nil, tc.failure
				}
				if len(args) > 0 && args[0] == "exec" {
					persists++
				}
				return base(ctx, command, args...)
			})
			err := d.persistDedicatedGuest(ctx, dedicatedReadyInstance("container"))
			if !errors.Is(err, tc.failure) {
				t.Errorf("空输出的 init 永久错误必须保留原始错误链: got %v want %v", err, tc.failure)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("脚本参数中的暂态 marker 不得将永久错误替换为 deadline: %v", err)
			}
			if transportProbes != 1 || initProbes != 1 || persists != 0 {
				t.Errorf("永久 init 错误必须立即返回且不写配置: transport=%d init=%d persists=%d", transportProbes, initProbes, persists)
			}
		})
	}
}

func TestIncusDedicatedInitWaitHonorsCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	base := dedicatedRunner(t, &calls)
	initProbes, persists := 0, 0
	ctx := WithCommandRunner(parent, func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if isDedicatedInitProbe(args) {
			initProbes++
			time.AfterFunc(25*time.Millisecond, cancel)
			return []byte("virtualis guest init not ready"), errors.New("exit status 75")
		}
		if len(args) > 0 && args[0] == "exec" && !(len(args) == 4 && args[3] == "true") {
			persists++
		}
		return base(ctx, command, args...)
	})
	started := time.Now()
	err := NewIncusWithDataDir(t.TempDir()).persistDedicatedGuest(ctx, dedicatedReadyInstance("container"))
	if !errors.Is(err, context.Canceled) || initProbes != 1 || persists != 0 || time.Since(started) > 300*time.Millisecond {
		t.Fatalf("init 等待必须及时响应撤销且不写配置: err=%v init=%d persists=%d", err, initProbes, persists)
	}
}

func TestDedicatedInitProbeChecksBusAndResolverSymlink(t *testing.T) {
	var probe string
	ctx := WithCommandRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if isDedicatedInitProbe(args) {
			probe = args[5]
		}
		return nil, nil
	})
	if err := NewIncus().waitDedicatedGuestReady(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	if probe == "" {
		t.Fatal("没有检查 systemd bus 和 resolver 的 init 就绪探针")
	}
	for _, state := range []string{"no-bus", "dangling-resolver", "ready"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			stub := "#!/bin/sh\nexit 0\n"
			if state == "no-bus" {
				stub = "#!/bin/sh\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(stub), 0755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "stub-resolv.conf")
			if state != "dangling-resolver" {
				if err := os.WriteFile(target, []byte("nameserver 127.0.0.53\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			resolver := filepath.Join(dir, "resolv.conf")
			if err := os.Symlink(target, resolver); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("sh", "-c", strings.ReplaceAll(probe, "/etc/resolv.conf", resolver))
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := command.CombinedOutput()
			if state == "ready" {
				if err != nil {
					t.Fatalf("完整就绪应放行: %v: %s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), "virtualis guest init not ready") {
				t.Fatalf("bus/resolver 尚未就绪应报告暂态: err=%v out=%s", err, out)
			}
		})
	}
}
