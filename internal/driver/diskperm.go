package driver

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// libvirt 默认以非 root 用户运行 QEMU 进程（Debian/Ubuntu 为
// libvirt-qemu，RHEL 系为 qemu），而被控以 root 落盘的 0700 目录与
// 0600 文件该进程既穿不透也读不了，域开机时报：
//
//	Cannot access storage file '...' (as uid:64055, gid:64055): Permission denied
//
// 这里把镜像目录与文件放宽到 QEMU 进程可访问。

// qemuOwnership 返回宿主机上 libvirt 运行 QEMU 的用户与组。
// 找不到已知用户时返回 false，由调用方退化处理。
func qemuOwnership() (int, int, bool) {
	for _, name := range []string{"libvirt-qemu", "qemu"} {
		u, err := user.Lookup(name)
		if err != nil {
			continue
		}
		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			continue
		}
		// 组优先取 qemu 惯例组（kvm/qemu），取不到用该用户的主组。
		gid := -1
		for _, group := range []string{"kvm", "qemu"} {
			if g, gerr := user.LookupGroup(group); gerr == nil {
				if v, aerr := strconv.Atoi(g.Gid); aerr == nil {
					gid = v
					break
				}
			}
		}
		if gid < 0 {
			gid, _ = strconv.Atoi(u.Gid)
		}
		return uid, gid, true
	}
	return 0, 0, false
}

// PrepareDiskDir 确保数据目录与镜像目录对 QEMU 进程可穿越。
// 对旧安装的 0700 目录也生效（Chmod 幂等）。
func PrepareDiskDir(dataDir string) {
	if dataDir == "" {
		return
	}
	_ = os.Chmod(dataDir, 0o755)
	images := filepath.Join(dataDir, "images")
	_ = os.MkdirAll(images, 0o755)
	_ = os.Chmod(images, 0o755)
}

// PrepareAllDiskFiles 对镜像目录里已有的文件批量放开权限，覆盖旧版本
// 落盘的 0600 文件 —— 被控升级重启后无需重建实例即可开机。
func PrepareAllDiskFiles(dataDir string) {
	if dataDir == "" {
		return
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "images"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		PrepareDiskFile(filepath.Join(dataDir, "images", entry.Name()))
	}
}

// PrepareDiskFile 让 root 落盘的磁盘/镜像文件可被 QEMU 进程读写：
// 优先 chown 给 QEMU 运行用户（060 组可写），宿主机没有该用户时
// 退化为 0666。对 cdrom 只读挂载同样适用。
func PrepareDiskFile(path string) {
	if path == "" {
		return
	}
	if uid, gid, ok := qemuOwnership(); ok {
		if err := os.Chown(path, uid, gid); err == nil {
			_ = os.Chmod(path, 0o660)
			return
		}
	}
	_ = os.Chmod(path, 0o666)
}
