//go:build !windows

package driver

import "syscall"

// setpgidAttr 返回把子进程放进独立进程组的属性：Xvfb/x11vnc/xterm 的
// 生命周期由实例驱动，停止实例时按进程组整体回收。Unix 专属字段。
func setpgidAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
