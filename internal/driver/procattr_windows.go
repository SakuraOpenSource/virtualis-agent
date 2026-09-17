//go:build windows

package driver

import "syscall"

// setpgidAttr 在 Windows 上没有进程组概念，返回空属性；
// VNC 辅助进程只在 Linux 节点上启动，这里仅为让交叉编译通过。
func setpgidAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
