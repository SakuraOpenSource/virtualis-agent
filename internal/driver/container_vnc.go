package driver

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
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
	mu       sync.Mutex
	sessions map[string]*vncSession
}

var containerVNC = &containerVNCManager{sessions: make(map[string]*vncSession)}

func containerVNCKey(driverName string, inst *protocol.Instance) string {
	return driverName + "/" + strconv.FormatUint(uint64(inst.ID), 10)
}

// ensureContainerVNC 返回实例的本地 VNC 端口，按需拉起会话。
// attach 返回进入容器 shell 的完整命令（argv 形式）。
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
	m.mu.Lock()
	if s, ok := m.sessions[key]; ok && xvfbAlive(s.display) {
		port := s.port
		m.mu.Unlock()
		return port, nil
	}
	m.mu.Unlock()
	// agent 重启后的孤儿会话直接复用，端口保持稳定。
	if s := findOrphanSession(inst); s != nil {
		m.mu.Lock()
		m.sessions[key] = s
		m.mu.Unlock()
		log.Printf("容器 VNC 孤儿会话已接管: %s display=:%d port=%d", key, s.display, s.port)
		return s.port, nil
	}

	name := resourceName(driverName, inst)
	display, port := m.allocateSlot()
	argv := attach(name)
	displayStr := strconv.Itoa(display)
	portStr := strconv.Itoa(port)

	xvfb := exec.Command("Xvfb", ":"+displayStr, "-screen", "0", "1280x800x24", "-nolisten", "tcp")
	xvfb.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := xvfb.Start(); err != nil {
		return 0, fmt.Errorf("启动 Xvfb 失败: %w", err)
	}
	time.Sleep(800 * time.Millisecond) // 等 X socket 就绪

	env := append(os.Environ(), "DISPLAY=:"+displayStr)
	xterm := exec.Command("xterm", "-display", ":"+displayStr, "-title", name, "-geometry", "120x36")
	xterm.Args = append(xterm.Args, "-e")
	xterm.Args = append(xterm.Args, argv...)
	xterm.Env = env
	xterm.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := xterm.Start(); err != nil {
		stopContainerVNCProcesses(display, port)
		return 0, fmt.Errorf("启动 xterm 失败: %w", err)
	}

	x11vnc := exec.Command("x11vnc", "-display", ":"+displayStr,
		"-rfbport", portStr, "-nopw", "-shared", "-forever", "-quiet",
		"-noxrecord", "-noxdamage")
	x11vnc.Env = env
	x11vnc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := x11vnc.Start(); err != nil {
		stopContainerVNCProcesses(display, port)
		return 0, fmt.Errorf("启动 x11vnc 失败: %w", err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+portStr, time.Second)
		if err == nil {
			_ = conn.Close()
			m.mu.Lock()
			m.sessions[key] = &vncSession{key: key, display: display, port: port}
			m.mu.Unlock()
			log.Printf("容器 VNC 会话已建立: %s display=:%d port=%d", key, display, port)
			return port, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	stopContainerVNCProcesses(display, port)
	return 0, fmt.Errorf("x11vnc 端口 %d 未就绪", port)
}

func xvfbAlive(display int) bool {
	out, err := output(context.Background(), "pgrep", "-f", fmt.Sprintf("Xvfb :%d ", display))
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

func stopContainerVNCProcesses(display, port int) {
	_ = run(context.Background(), "pkill", "-f", fmt.Sprintf("Xvfb :%d ", display))
	_ = run(context.Background(), "pkill", "-f", fmt.Sprintf("rfbport %d", port))
	_ = run(context.Background(), "pkill", "-f", fmt.Sprintf("-display :%d", display))
}
