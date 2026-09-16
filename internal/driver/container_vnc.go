package driver

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// 容器（LXC/Incus）VNC：宿主侧 Xvfb + xterm + x11vnc 桥接。
//
// 容器没有图形控制台，VNC 在这里的意义是"实例控制台"：Xvfb 提供虚拟显示，
// xterm 在里面跑 `lxc-attach`/`incus exec` 进入容器 shell，x11vnc 把该显示
// 导出为 VNC 端口。主程序的 VNC WebSocket 代理按 vnc.Port 直连 127.0.0.1，
// 容器驱动返回真实端口即可零改动复用整条链路。
//
// 每实例一组后台进程（Xvfb / xterm / x11vnc，命令行都带可识别特征），
// 停止实例时按特征 pkill 回收；agent 重启后的孤儿会话靠特征找回复用，
// 避免 display/端口漂移。
//
// 会话复用必须做健康检查而不是只看 Xvfb 是否存活：x11vnc 死掉会让返回的
// 端口无法建连（"VNC 无法连接"），xterm 退出后画面只剩纯黑 root window
// （"VNC 无画面"）。两者都在 ensure 时按需自动补拉。
const (
	containerVNCBaseDisplay = 50 // 避开宿主桌面 :0 与常见残留
	containerVNCMaxDisplays = 100
)

type vncSession struct {
	key     string
	display int
	port    int
}

type containerVNCManager struct {
	mu sync.Mutex
	// sessions 保存本进程创建的会话；setupMu 串行化 ensure 的慢路径
	// （拉进程、等端口），避免并发请求对同一实例起两套 Xvfb。
	sessions map[string]*vncSession
	setupMu  sync.Mutex
}

var containerVNC = &containerVNCManager{sessions: make(map[string]*vncSession)}

func containerVNCKey(driverName string, inst *protocol.Instance) string {
	return driverName + "/" + strconv.FormatUint(uint64(inst.ID), 10)
}

// ensure 返回实例的本地 VNC 端口，按需拉起会话；attach 返回进入容器
// shell 的完整命令（argv 形式）。
func (m *containerVNCManager) ensure(ctx context.Context, driverName string, inst *protocol.Instance, running func() bool, attach func(string) []string) (int, error) {
	for _, bin := range []string{"Xvfb", "x11vnc", "xterm"} {
		if !hasCommand(bin) {
			return 0, fmt.Errorf("宿主缺少 %s（安装 xvfb x11vnc xterm 后可用容器 VNC）", bin)
		}
	}
	if !running() {
		return 0, fmt.Errorf("实例未运行，无法建立 VNC 会话")
	}
	key := containerVNCKey(driverName, inst)
	m.setupMu.Lock()
	defer m.setupMu.Unlock()

	name := resourceName(driverName, inst)

	// 已注册或孤儿会话：Xvfb 存活就接管，并把 x11vnc / xterm 补齐。
	if s := m.sessionOrOrphan(key, inst); s != nil && xvfbAlive(s.display) {
		if err := m.ensureProcesses(s, name, attach(name)); err == nil {
			m.mu.Lock()
			m.sessions[key] = s
			m.mu.Unlock()
			log.Printf("容器 VNC 会话已就绪: %s display=:%d port=%d", key, s.display, s.port)
			return s.port, nil
		} else {
			log.Printf("容器 VNC 会话修复失败，推倒重建: %s: %v", key, err)
			stopContainerVNCProcesses(s.display, s.port)
		}
	}
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()

	display, port := m.allocateSlot()
	displayStr := strconv.Itoa(display)

	xvfb := exec.Command("Xvfb", ":"+displayStr, "-screen", "0", "1280x800x24", "-nolisten", "tcp")
	xvfb.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := xvfb.Start(); err != nil {
		return 0, fmt.Errorf("启动 Xvfb 失败: %w", err)
	}
	time.Sleep(800 * time.Millisecond) // 等 X socket 就绪

	env := append(os.Environ(), "DISPLAY=:"+displayStr)
	if err := startXterm(displayStr, name, attach(name), env); err != nil {
		stopContainerVNCProcesses(display, port)
		return 0, err
	}
	if err := startX11VNC(displayStr, port, env); err != nil {
		stopContainerVNCProcesses(display, port)
		return 0, err
	}
	if !waitVNCListening(port, 6*time.Second) {
		stopContainerVNCProcesses(display, port)
		return 0, fmt.Errorf("x11vnc 端口 %d 未就绪", port)
	}
	m.mu.Lock()
	m.sessions[key] = &vncSession{key: key, display: display, port: port}
	m.mu.Unlock()
	log.Printf("容器 VNC 会话已建立: %s display=:%d port=%d", key, display, port)
	return port, nil
}

// sessionOrOrphan 返回已注册会话，或 agent 重启后的孤儿会话（端口保持稳定）。
func (m *containerVNCManager) sessionOrOrphan(key string, inst *protocol.Instance) *vncSession {
	m.mu.Lock()
	s := m.sessions[key]
	m.mu.Unlock()
	if s != nil {
		return s
	}
	return findOrphanSession(inst)
}

// ensureProcesses 把存活 Xvfb 之上的 x11vnc / xterm 补齐，返回是否全部健康。
func (m *containerVNCManager) ensureProcesses(s *vncSession, name string, argv []string) error {
	displayStr := strconv.Itoa(s.display)
	env := append(os.Environ(), "DISPLAY=:"+displayStr)
	if !vncListening(s.port) {
		if err := startX11VNC(displayStr, s.port, env); err != nil {
			return err
		}
		if !waitVNCListening(s.port, 6*time.Second) {
			return fmt.Errorf("x11vnc 端口 %d 未就绪", s.port)
		}
		log.Printf("容器 VNC x11vnc 已重拉: %s port=%d", s.key, s.port)
	}
	if xtermAlive(s.display, name) {
		return nil
	}
	if err := startXterm(displayStr, name, argv, env); err != nil {
		return err
	}
	log.Printf("容器 VNC xterm 已重开: %s display=:%d", s.key, s.display)
	return nil
}

func startXterm(display, name string, argv []string, env []string) error {
	xterm := exec.Command("xterm", "-display", ":"+display, "-title", name, "-geometry", "120x36")
	xterm.Args = append(xterm.Args, "-e")
	xterm.Args = append(xterm.Args, argv...)
	xterm.Env = env
	xterm.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := xterm.Start(); err != nil {
		return fmt.Errorf("启动 xterm 失败: %w", err)
	}
	return nil
}

func startX11VNC(display string, port int, env []string) error {
	x11vnc := exec.Command("x11vnc", "-display", ":"+display,
		"-rfbport", strconv.Itoa(port), "-nopw", "-shared", "-forever", "-quiet",
		"-noxrecord", "-noxdamage")
	x11vnc.Env = env
	x11vnc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := x11vnc.Start(); err != nil {
		return fmt.Errorf("启动 x11vnc 失败: %w", err)
	}
	return nil
}

// vncListening 报告 x11vnc 端口是否可建连（x11vnc 进程是否还活着的最可靠信号）。
func vncListening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// stopContainerVNCProcesses 按命令行特征回收实例的 VNC 三件套。
// 注意模式不能以 "-" 开头：pkill 会把它当成自己的选项，导致 xterm/x11vnc
// 从来杀不掉（实例停了进程还在，端口与 display 槽位持续泄漏）。
func stopContainerVNCProcesses(display, port int) {
	_ = run(context.Background(), "pkill", "-f", fmt.Sprintf("Xvfb :%d ", display))
	_ = run(context.Background(), "pkill", "-f", fmt.Sprintf("x11vnc.*rfbport %d", port))
	_ = run(context.Background(), "pkill", "-f", fmt.Sprintf("xterm.*-display :%d", display))
}

func waitVNCListening(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if vncListening(port) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return vncListening(port)
}

func xvfbAlive(display int) bool {
	out, err := output(context.Background(), "pgrep", "-f", fmt.Sprintf("Xvfb :%d ", display))
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// xtermAlive 报告该 display 上标题为 name 的 xterm 是否仍在运行。
// xterm 退出意味着控制台 shell 已结束（用户 exit 或 attach 命令失败，
// 例如镜像没有 bash），只剩没有任何窗口的纯黑画面，必须重开。
func xtermAlive(display int, name string) bool {
	pattern := regexp.QuoteMeta(fmt.Sprintf("xterm -display :%d -title %s", display, name))
	out, err := output(context.Background(), "pgrep", "-f", pattern)
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// findOrphanSession 按 display 首选槽位（id 确定性）查找存活的 Xvfb。
func findOrphanSession(inst *protocol.Instance) *vncSession {
	display := containerVNCBaseDisplay + int(inst.ID%containerVNCMaxDisplays)
	if xvfbAlive(display) {
		return &vncSession{display: display, port: 5900 + display}
	}
	return nil
}

// allocateSlot 首选 50+id%100 的确定性槽位；被占时向后找空位。
func (m *containerVNCManager) allocateSlot() (int, int) {
	for display := containerVNCBaseDisplay; display < containerVNCBaseDisplay+containerVNCMaxDisplays; display++ {
		if !xvfbAlive(display) {
			return display, 5900 + display
		}
	}
	return containerVNCBaseDisplay, 5900 + containerVNCBaseDisplay
}

func (m *containerVNCManager) release(key string) {
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()
}

// stopContainerVNC 回收实例的 VNC 会话。
func (m *containerVNCManager) stop(driverName string, inst *protocol.Instance) {
	key := containerVNCKey(driverName, inst)
	m.mu.Lock()
	session, ok := m.sessions[key]
	m.mu.Unlock()
	if !ok {
		return
	}
	stopContainerVNCProcesses(session.display, session.port)
	m.release(key)
	log.Printf("容器 VNC 会话已回收: %s", key)
}

