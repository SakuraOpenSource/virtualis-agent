package driver

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 客户机引导注入（QEMU NAT 模式专用，尽力而为、失败不阻塞创建）：
//
// 模板镜像里的网络配置千差万别——接口名对不上（模板写 eth0，实际内核叫
// ens3）、静态地址属于上一个环境、root 密码无人知晓。创建实例时把系统盘
// 挂到 nbd 上做三件事：
//  1. 注入 systemd 开机服务，运行时自动探测真实网卡名并 DHCP（自动获取
//     网卡名，不假设 eth0/ens3）；
//  2. 把面板生成的 root 密码写进 /etc/shadow，SSH 直接可连；
//  3. 放行 root 密码登录（sshd_config.d）。
//
// 只对"盘里已有系统"的 qcow2 生效；ISO 空盘挂不出根分区，静默跳过，
// 密码交由装好系统后的 set-user-password 路径。

const (
	guestNetScriptPath  = "/usr/local/sbin/virtualis-net.sh"
	guestNetServicePath = "/etc/systemd/system/virtualis-net.service"
	guestNetSSHDropin   = "/etc/ssh/sshd_config.d/00-virtualis.conf"
)

const guestNetScript = `#!/bin/sh
# virtualis: 自动探测网卡（不假设命名）并 DHCP，幂等可重复执行。
for n in $(ls /sys/class/net 2>/dev/null); do
  [ "$n" = "lo" ] && continue
  ip link set "$n" up 2>/dev/null
  if command -v dhclient >/dev/null 2>&1; then
    dhclient -1 "$n" >/dev/null 2>&1 || dhclient "$n" >/dev/null 2>&1
  elif command -v udhcpc >/dev/null 2>&1; then
    udhcpc -i "$n" -n -q >/dev/null 2>&1
  fi
done
`

const guestNetUnit = `[Unit]
Description=Virtualis guest network (auto NIC + DHCP)
Wants=network-online.target
Before=network-online.target sshd.service ssh.service

[Service]
Type=oneshot
ExecStart=` + guestNetScriptPath + `
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`

// InjectGuestBootstrap 挂载 qcow2 系统盘并注入网络引导 + root 密码。
// rootPassword 为空时只注入网络。任何失败返回 error 由调用方记日志。
func InjectGuestBootstrap(ctx context.Context, diskPath, rootPassword string) error {
	if !hasCommand("qemu-nbd") {
		return fmt.Errorf("qemu-nbd 未安装")
	}
	if err := run(ctx, "modprobe", "nbd"); err != nil {
		return fmt.Errorf("加载 nbd 模块失败: %w", err)
	}
	device, err := freeNBDDevice()
	if err != nil {
		return err
	}
	if err := run(ctx, "qemu-nbd", "--format=qcow2", "-c", device, diskPath); err != nil {
		return fmt.Errorf("挂载 %s 到 %s 失败: %w", diskPath, device, err)
	}
	defer func() {
		_ = run(context.Background(), "qemu-nbd", "-d", device)
	}()
	time.Sleep(500 * time.Millisecond) // 等内核枚举分区

	root, err := mountGuestRoot(ctx, device)
	if err != nil {
		return err // ISO 空盘等场景：没有可识别的根分区
	}
	defer func() { _ = run(context.Background(), "umount", root) }()

	if _, err := os.Stat(filepath.Join(root, "etc")); err != nil {
		return fmt.Errorf("挂载点不含 /etc，不是系统盘根分区")
	}

	if err := writeGuestFile(root, guestNetScriptPath, guestNetScript, 0o755); err != nil {
		return fmt.Errorf("写入网卡探测脚本失败: %w", err)
	}
	if err := writeGuestFile(root, guestNetServicePath, guestNetUnit, 0o644); err != nil {
		return fmt.Errorf("写入 systemd 单元失败: %w", err)
	}
	// enable：直接放 wants 目录 symlink，不依赖 chroot systemctl。
	wants := filepath.Join(root, "etc/systemd/system/multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		return err
	}
	link := filepath.Join(wants, "virtualis-net.service")
	_ = os.Remove(link)
	if err := os.Symlink(guestNetServicePath, link); err != nil {
		return fmt.Errorf("启用开机服务失败: %w", err)
	}
	if err := writeGuestFile(root, guestNetSSHDropin, "PermitRootLogin yes\n", 0o644); err != nil {
		log.Printf("guest 引导: 写入 sshd drop-in 失败（旧版无 sshd_config.d 时可忽略）: %v", err)
	}
	if rootPassword != "" {
		if err := setGuestRootPassword(ctx, root, rootPassword); err != nil {
			return err
		}
	}
	log.Printf("guest 引导注入完成: %s（网卡自动探测 + DHCP%s）", filepath.Base(diskPath), map[bool]string{true: " + root 密码"}[rootPassword != ""])
	return nil
}

// freeNBDDevice 找一个未被占用的 /dev/nbdX。
func freeNBDDevice() (string, error) {
	for i := 0; i < 16; i++ {
		device := fmt.Sprintf("/dev/nbd%d", i)
		if _, err := os.Stat(device); err != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/sys/block/nbd%d/backing_file", i)); err != nil {
			return device, nil
		}
	}
	return "", fmt.Errorf("没有空闲的 nbd 设备")
}

// mountGuestRoot 挂载盘上第一个可用文件系统分区并返回挂载点；
// 无分区表的整盘文件系统也支持。
func mountGuestRoot(ctx context.Context, device string) (string, error) {
	candidates := []string{}
	out, err := output(ctx, "lsblk", "-rno", "NAME,TYPE,FSTYPE", device)
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[1] == "part" && fields[2] != "" {
				candidates = append(candidates, "/dev/"+fields[0])
			}
		}
	}
	if len(candidates) == 0 {
		// 无分区表：探测整盘。
		if blkOut, blkErr := output(ctx, "blkid", "-o", "value", "-s", "TYPE", device); blkErr == nil && strings.TrimSpace(string(blkOut)) != "" {
			candidates = append(candidates, device)
		}
	}
	root := "/mnt/virtualis-guest"
	_ = run(ctx, "mkdir", "-p", root)
	var lastErr error
	for _, part := range candidates {
		if err := run(ctx, "mount", "-o", "rw", part, root); err != nil {
			lastErr = err
			continue
		}
		return root, nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("没有可挂载的根分区（ISO 空盘属正常）: %w", lastErr)
	}
	return "", fmt.Errorf("盘上没有文件系统分区（ISO 空盘属正常）")
}

// writeGuestFile 在挂载的 guest 根下写文件（绝对路径以 / 开头）。
func writeGuestFile(root, path, content string, mode os.FileMode) error {
	full := filepath.Join(root, strings.TrimPrefix(path, "/"))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), mode)
}

// setGuestRootPassword 用 openssl 生成 SHA-512 crypt 并替换 shadow 里的 root 行。
func setGuestRootPassword(ctx context.Context, root, password string) error {
	out, err := output(ctx, "openssl", "passwd", "-6", password)
	if err != nil {
		return fmt.Errorf("生成密码哈希失败: %w", err)
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return fmt.Errorf("openssl passwd 输出为空")
	}
	shadowPath := filepath.Join(root, "etc/shadow")
	raw, err := os.ReadFile(shadowPath)
	if err != nil {
		return fmt.Errorf("读取 shadow 失败: %w", err)
	}
	lines := strings.Split(string(raw), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "root:") {
			fields := strings.SplitN(line, ":", 3)
			if len(fields) < 3 {
				fields = []string{"root", hash, ""}
			} else {
				fields[1] = hash
			}
			lines[i] = strings.Join(fields, ":")
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append([]string{"root:" + hash + ":19000:0:99999:7:::"}, lines...)
	}
	if err := os.WriteFile(shadowPath, []byte(strings.Join(lines, "\n")), 0o640); err != nil {
		return fmt.Errorf("写回 shadow 失败: %w", err)
	}
	return nil
}
