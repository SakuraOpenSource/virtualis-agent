package driver

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func dedicatedReadyInstance(typ string) *protocol.Instance {
	return &protocol.Instance{
		ID: 8, Name: "routed", Type: typ,
		Image:   &protocol.Image{Path: "/fixture/rootfs.tar"},
		Network: protocol.NetworkConfig{Mode: "dedicated", Bridge: "eth0", IPv4: "192.0.2.20/32", DNS: []string{"1.1.1.1"}},
	}
}

func TestIncusDedicatedBootGuestWaitHonorsCancellation(t *testing.T) {
	for _, action := range []string{"create", "restart"} {
		for _, stage := range []string{"before-probe", "during-probe", "retry-delay"} {
			t.Run(action+"/"+stage, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				if stage == "before-probe" {
					cancel()
				}
				var calls []string
				base := dedicatedRunner(t, &calls)
				probes, persists := 0, 0
				ctx := withInitializedGuestRunner(parent, func(ctx context.Context, command string, args ...string) ([]byte, error) {
					if len(args) > 1 && args[0] == "query" && strings.Contains(args[1], "/profiles/") {
						return []byte(`{"devices":{"root":{"type":"disk"}}}`), nil
					}
					if len(args) > 0 && args[0] == "exec" {
						if len(args) == 4 && args[3] == "true" {
							probes++
						} else {
							persists++
						}
						switch stage {
						case "during-probe":
							cancel()
							<-ctx.Done()
							return nil, ctx.Err()
						case "retry-delay":
							timer := time.AfterFunc(25*time.Millisecond, cancel)
							t.Cleanup(func() { timer.Stop() })
							return []byte("VM agent isn't currently running"), errors.New("exit status 1")
						}
					}
					return base(ctx, command, args...)
				})
				d := NewIncusWithDataDir(t.TempDir())
				inst := dedicatedReadyInstance("vm")
				start := time.Now()
				var err error
				if action == "create" {
					err = d.Create(ctx, inst)
				} else {
					err = d.Restart(ctx, inst)
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("guest 等待必须返回 ctx 撤销: got %v", err)
				}
				if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
					t.Errorf("guest 等待未及时响应 ctx 撤销: %s", elapsed)
				}
				if persists != 0 || probes > 1 || (stage == "before-probe" && probes != 0) {
					t.Errorf("撤销后不得持久化或继续探测: probes=%d persists=%d", probes, persists)
				}
			})
		}
	}
}

func TestIncusDedicatedGuestWaitHasBoundedProbeContext(t *testing.T) {
	var calls []string
	base := dedicatedRunner(t, &calls)
	probes, persists := 0, 0
	ctx := withInitializedGuestRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if len(args) == 4 && args[0] == "exec" && args[3] == "true" {
			probes++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Minute {
				t.Fatalf("guest 就绪 exec 必须受最多一分钟的 deadline 约束: deadline=%v ok=%t", deadline, ok)
			}
		} else if len(args) > 0 && args[0] == "exec" {
			persists++
			if _, ok := ctx.Deadline(); ok {
				t.Fatal("就绪探针的局部 deadline 不应泄漏到持久化阶段")
			}
		}
		return base(ctx, command, args...)
	})
	if err := NewIncusWithDataDir(t.TempDir()).persistDedicatedGuest(ctx, dedicatedReadyInstance("vm")); err != nil {
		t.Fatal(err)
	}
	if probes != 1 || persists != 1 {
		t.Fatalf("应完成有界探针后持久化: probes=%d persists=%d", probes, persists)
	}
}

func TestIncusDedicatedGuestWaitHonorsCallerDeadline(t *testing.T) {
	for _, stage := range []string{"during-probe", "retry-delay"} {
		t.Run(stage, func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			var calls []string
			base := dedicatedRunner(t, &calls)
			probes, persists := 0, 0
			ctx := withInitializedGuestRunner(parent, func(ctx context.Context, command string, args ...string) ([]byte, error) {
				if len(args) == 4 && args[0] == "exec" && args[3] == "true" {
					probes++
					if stage == "during-probe" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return []byte("VM agent isn't currently running"), errors.New("exit status 1")
				}
				if len(args) > 0 && args[0] == "exec" {
					persists++
				}
				return base(ctx, command, args...)
			})
			start := time.Now()
			err := NewIncusWithDataDir(t.TempDir()).persistDedicatedGuest(ctx, dedicatedReadyInstance("vm"))
			if !errors.Is(err, context.DeadlineExceeded) || probes != 1 || persists != 0 {
				t.Fatalf("调用方 deadline 未阻止持久化: err=%v probes=%d persists=%d", err, probes, persists)
			}
			if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
				t.Fatalf("未及时响应调用方 deadline: %s", elapsed)
			}
		})
	}
}

func TestIncusDedicatedGuestWaitDoesNotRetryPermanentProbeError(t *testing.T) {
	var calls []string
	base := dedicatedRunner(t, &calls)
	failure := errors.New("permission denied")
	guestCalls := 0
	ctx := withInitializedGuestRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "exec" {
			guestCalls++
			return nil, failure
		}
		return base(ctx, command, args...)
	})
	err := NewIncusWithDataDir(t.TempDir()).persistDedicatedGuest(ctx, dedicatedReadyInstance("vm"))
	if !errors.Is(err, failure) || guestCalls != 1 {
		t.Fatalf("永久探针错误不得重试或吞掉: err=%v calls=%d", err, guestCalls)
	}
}

func TestIncusDedicatedGuestPersistenceDoesNotRetryConfigurationError(t *testing.T) {
	var calls []string
	base := dedicatedRunner(t, &calls)
	// 即使配置阶段输出碰巧等于暂态就绪错误，也不得重新执行写入脚本。
	failure := errors.New("VM agent isn't currently running")
	probes, persists := 0, 0
	ctx := withInitializedGuestRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if len(args) == 4 && args[0] == "exec" && args[3] == "true" {
			probes++
			return nil, nil
		}
		if len(args) > 0 && args[0] == "exec" {
			persists++
			return nil, failure
		}
		return base(ctx, command, args...)
	})
	err := NewIncusWithDataDir(t.TempDir()).persistDedicatedGuest(ctx, dedicatedReadyInstance("vm"))
	if !errors.Is(err, failure) || probes != 1 || persists != 1 {
		t.Fatalf("配置错误不得重试或吞掉: err=%v probes=%d persists=%d", err, probes, persists)
	}
}

func TestIncusDedicatedBootWaitsForGuestBeforePersistence(t *testing.T) {
	for _, action := range []string{"create", "restart"} {
		for _, typ := range []string{"vm", "container"} {
			t.Run(action+"/"+typ, func(t *testing.T) {
				var calls []string
				base := dedicatedRunner(t, &calls)
				var guestCalls []string
				ctx := withInitializedGuestRunner(context.Background(), func(ctx context.Context, command string, args ...string) ([]byte, error) {
					if len(args) > 1 && args[0] == "query" && strings.Contains(args[1], "/profiles/") {
						return []byte(`{"devices":{"root":{"type":"disk"}}}`), nil
					}
					if len(args) > 0 && args[0] == "exec" {
						if len(args) == 4 && args[2] == "--" && args[3] == "true" {
							guestCalls = append(guestCalls, "probe")
						} else {
							guestCalls = append(guestCalls, "persist")
						}
						if len(guestCalls) == 1 {
							message := "Error: VM agent isn't currently running"
							if typ == "container" {
								message = "Error: Instance is not running"
							}
							return []byte(message), errors.New("exit status 1")
						}
					}
					return base(ctx, command, args...)
				})
				d := NewIncusWithDataDir(t.TempDir())
				inst := dedicatedReadyInstance(typ)
				var err error
				if action == "create" {
					err = d.Create(ctx, inst)
				} else {
					err = d.Restart(ctx, inst)
				}
				if err != nil {
					t.Fatalf("guest 暂未就绪后恢复，不应使 %s 失败: %v", action, err)
				}
				if want := []string{"probe", "probe", "persist"}; !reflect.DeepEqual(guestCalls, want) {
					t.Fatalf("应先等待 guest 就绪，再只执行一次持久化: got %v want %v", guestCalls, want)
				}
			})
		}
	}
}
