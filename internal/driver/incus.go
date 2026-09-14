package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// Incus 驱动：离线导入镜像后 launch，网络经 device 配置。
type Incus struct {
	mu      sync.Mutex
	samples map[uint]incusSample
}

type incusSample struct {
	cpuTime uint64
	rx, tx  uint64
	at      time.Time
}

func NewIncus() *Incus {
	return &Incus{samples: make(map[uint]incusSample)}
}
func (d *Incus) Name() string { return "incus" }

func (d *Incus) Probe(ctx context.Context) error {
	if !hasCommand("incus") {
		return fmt.Errorf("Incus 未安装")
	}
	return run(ctx, "incus", "version")
}

func (d *Incus) cli() string {
	if hasCommand("incus") {
		return "incus"
	}
	return "lxc"
}

func (d *Incus) Create(ctx context.Context, inst *protocol.Instance) error {
	if inst.Image == nil || inst.Image.Path == "" {
		return fmt.Errorf("Incus 离线模式需要先上传镜像到 data/images，请在镜像管理上传镜像后重试")
	}
	name := resourceName("incus", inst)
	alias := fmt.Sprintf("virtualis-img-%d", inst.ID)
	// 上次失败可能残留同名 alias 或同 fingerprint 镜像：先幂等清理。
	_ = run(ctx, d.cli(), "image", "delete", alias)
	// 分割镜像（meta.tar.xz + rootfs.tar.xz / disk.qcow2）传两个文件；
	// 统一镜像（单 tar）只传 Path。
	importArgs := []string{"image", "import", inst.Image.Path, "--alias", alias}
	if inst.Image.ExtraPath != "" {
		importArgs = []string{"image", "import", inst.Image.ExtraPath, inst.Image.Path, "--alias", alias}
	}
	if err := run(ctx, d.cli(), importArgs...); err != nil {
		if !contains(err.Error(), "already exists") {
			return fmt.Errorf("导入离线镜像失败: %w", err)
		}
		// 同 fingerprint 镜像已在本地库（上次失败残留）：把挂在该镜像上的
		// virtualis alias 指回本次 alias，launch 才有入口。
		out, listErr := output(ctx, d.cli(), "image", "list", "--format", "csv")
		if listErr != nil {
			return fmt.Errorf("导入离线镜像失败: %w", err)
		}
		fingerprint := ""
		for _, line := range strings.Split(string(out), "\n") {
			// CSV 列：alias,fingerprint,public,description,...
			cols := strings.Split(line, ",")
			if len(cols) < 2 || !strings.Contains(cols[0], "virtualis-img-") {
				continue
			}
			fingerprint = cols[1]
			break
		}
		if fingerprint == "" || run(ctx, d.cli(), "image", "alias", "create", alias, fingerprint) != nil {
			return fmt.Errorf("导入离线镜像失败: %w", err)
		}
	}
	// NAT 模式保留静态地址（dnsmasq 静态租约），NAT 映射目标才稳定。
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat {
		if err := d.ensureIncusBridge(ctx); err != nil {
			return err
		}
		if inst.Network.IPv4 == "" {
			// Incus 容器挂 incusbr0，保留地址必须落在它的子网内。
			// 网桥不存在时直接失败，不再用硬编码 10.10.10.x 盲 launch。
			reserved, _ := natSlotIPOn("incusbr0", inst)
			if reserved == "" {
				return fmt.Errorf("NAT 网桥 incusbr0 不存在或无 IPv4 地址，无法分配保留 IP")
			}
			inst.Network.IPv4 = reserved
		}
		// NAT 未指定 MAC 时派生确定性 MAC（与 QEMU 路径一致）：dnsmasq
		// 静态租约按 MAC 绑定，固定 MAC 可避免重装后新旧随机 MAC 的
		// 陈旧租约干扰首次 DHCP。
		if strings.TrimSpace(inst.Network.MAC) == "" {
			inst.Network.MAC = natMAC(inst)
		}
	}
	// 每实例一个专用 profile 承载网络/资源限制：launch 的 -d 简写在
	// Incus 6 上解析不可靠，profile device add 的位置参数语法最稳。
	profile := fmt.Sprintf("virtualis-p-%d", inst.ID)
	if err := d.ensureProfile(ctx, profile, inst.Network, inst); err != nil {
		return err
	}
	args := []string{"launch", alias, name, "-p", profile}
	if inst.Type == "vm" {
		args = append(args, "--vm")
	}
	if inst.Spec.CPU > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu=%d", inst.Spec.CPU))
	}
	if inst.Spec.MemoryMB > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMiB", inst.Spec.MemoryMB))
	}
	if err := run(ctx, d.cli(), args...); err != nil {
		return err
	}
	d.ensureImageAliasCleaned(ctx, alias)
	// 创建时就地确认网络已注册（静态 IPv4 真的拿到手），不留到首次使用。
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat && inst.Network.IPv4 != "" {
		if err := d.ensureContainerIPv4(ctx, name, inst.Network.IPv4); err != nil {
			log.Printf("实例 %d 容器 IPv4 确认失败: %v", inst.ID, err)
		}
	}
	return nil
}

// ConfigureNetwork rebuilds the instance profile and reapplies it to an
// existing container. Incus profiles are live for most device changes; a
// restart makes the new DHCP/static address deterministic.
func (d *Incus) ConfigureNetwork(ctx context.Context, inst *protocol.Instance) error {
	name := resourceName("incus", inst)
	profile := fmt.Sprintf("virtualis-p-%d", inst.ID)
	if err := d.ensureProfile(ctx, profile, inst.Network, inst); err != nil {
		return err
	}
	if err := run(ctx, d.cli(), "profile", "assign", name, profile); err != nil && !contains(err.Error(), "already assigned") {
		return fmt.Errorf("应用实例 profile 失败: %w", err)
	}
	status, err := d.Status(ctx, inst)
	if err != nil {
		return err
	}
	if status == StatusRunning {
		if err := d.Restart(ctx, inst); err != nil {
			return fmt.Errorf("重启实例使网络配置生效失败: %w", err)
		}
	}
	if NormalizeNetworkMode(inst.Network.Mode) == NetworkModeNat && inst.Network.IPv4 != "" {
		if err := d.ensureContainerIPv4(ctx, name, inst.Network.IPv4); err != nil {
			return fmt.Errorf("确认容器 IPv4 失败: %w", err)
		}
	}
	return nil
}

// 幂等：profile 已存在时仅重建设备定义。
func (d *Incus) ensureProfile(ctx context.Context, profile string, network protocol.NetworkConfig, inst *protocol.Instance) error {
	if err := run(ctx, d.cli(), "profile", "create", profile); err != nil && !contains(err.Error(), "already exists") {
		return fmt.Errorf("创建实例 profile 失败: %w", err)
	}
	// Incus 创建的空 profile 不会继承 default，root 可能根本不存在。
	// 先读取设备清单：存在才 set，不存在就 add；不能把任意 remove 失败
	// 误判为“设备已存在”（这正是 profile device doesn't exist 的根因）。
	// 存储池不再硬编码 default：部分节点（如 HK 43.255.159.9）没有 default 池，
	// 自动选择现存池，否则 launch 会报 Pool not found。
	pool, err := d.ensureStoragePool(ctx)
	if err != nil {
		return err
	}
	rootExists, err := profileDeviceExists(ctx, d.cli(), profile, "root")
	if err != nil {
		return fmt.Errorf("读取 profile root 设备失败: %w", err)
	}
	if rootExists {
		// 旧 profile 可能挂在已不存在的池上（如 pool=default 但节点只有 s1）：
		// 此时仅 set size 无法自愈，launch 仍会失败，必须 remove 后按正确池重建。
		if cur := profileDeviceValue(ctx, d.cli(), profile, "root", "pool"); cur != "" && cur != pool {
			_ = run(ctx, d.cli(), "profile", "device", "remove", profile, "root")
			rootExists = false
		}
	}
	if rootExists {
		if args := profileRootDeviceArgs(profile, true, inst.Spec.DiskGB, pool); len(args) > 0 {
			if err := run(ctx, d.cli(), args...); err != nil {
				return fmt.Errorf("更新 root 磁盘配额失败: %w", err)
			}
		}
	} else {
		rootArgs := profileRootDeviceArgs(profile, false, inst.Spec.DiskGB, pool)
		if err := run(ctx, d.cli(), rootArgs...); err != nil {
			return fmt.Errorf("配置 root 磁盘失败: %w", err)
		}
	}
	eth0Exists, err := profileDeviceExists(ctx, d.cli(), profile, "eth0")
	if err != nil {
		return fmt.Errorf("读取 profile eth0 设备失败: %w", err)
	}
	if eth0Exists {
		if err := run(ctx, d.cli(), "profile", "device", "remove", profile, "eth0"); err != nil {
			return fmt.Errorf("移除旧网络设备失败: %w", err)
		}
	}
	mode := NormalizeNetworkMode(network.Mode)
	if mode == NetworkModeNone {
		if err := run(ctx, d.cli(), "profile", "device", "add", profile, "eth0", "nic", "nictype=none"); err != nil {
			return fmt.Errorf("配置网络设备失败: %w", err)
		}
		return nil
	}
	parent := "incusbr0"
	managed := true
	if mode == NetworkModeDedicated {
		target, _, err := dedicatedTarget(network)
		if err == nil && target != "" {
			parent = target
		} else if v := strings.TrimSpace(network.Bridge); v != "" {
			parent = v
		}
		// 独立网卡的目标可能是宿主机物理口等非托管桥：只有托管网络才走 network=。
		managed = d.isManagedNetwork(ctx, parent)
	}
	var spec []string
	if managed {
		spec = append([]string{"profile", "device", "add", profile, "eth0", "nic"}, incusEth0DeviceArgs(network, parent)...)
	} else {
		spec = append([]string{"profile", "device", "add", profile, "eth0", "nic"}, incusEth0UnmanagedArgs(network, parent)...)
	}
	if err := run(ctx, d.cli(), spec...); err != nil {
		return fmt.Errorf("配置网络设备失败: %w", err)
	}
	return nil
}

// incusEth0DeviceArgs 把 NAT/托管网络配置翻译成 profile device add 的设备参数。
// 托管网络必须用 network= 挂载：nictype=bridged parent= 会被视为非托管桥，
// 再带 ipv4.address 会在 Incus 6 上直接报错
// `Cannot use manually specified ipv4.address when using unmanaged parent bridge`
// (HK 43.255.159.9 实例 47 的失败根因)。抽成纯函数方便单测，ensureProfile
// 与 ConfigureNetwork 共用，保证创建与重配行为一致。
func incusEth0DeviceArgs(network protocol.NetworkConfig, parent string) []string {
	if strings.TrimSpace(parent) == "" {
		parent = "incusbr0"
	}
	args := []string{"network=" + parent}
	if network.IPv4 != "" {
		args = append(args, "ipv4.address="+strings.Split(network.IPv4, "/")[0])
	}
	if network.MAC != "" {
		args = append(args, "hwaddr="+network.MAC)
	}
	if network.BandwidthMbps > 0 {
		limit := fmt.Sprintf("%dMbit", network.BandwidthMbps)
		args = append(args, "limits.ingress="+limit, "limits.egress="+limit)
	}
	return args
}

// incusEth0UnmanagedArgs 用于非托管父桥（宿主机物理口、自建 linux bridge 等）：
// 非托管桥不支持在设备上指定 ipv4.address，IP 由客内静态配置完成，
// 这里只带 hwaddr 与限速，避免触发与托管桥相同的校验错误。
func incusEth0UnmanagedArgs(network protocol.NetworkConfig, parent string) []string {
	if strings.TrimSpace(parent) == "" {
		parent = "incusbr0"
	}
	args := []string{"nictype=bridged", "parent=" + parent}
	if network.MAC != "" {
		args = append(args, "hwaddr="+network.MAC)
	}
	if network.BandwidthMbps > 0 {
		limit := fmt.Sprintf("%dMbit", network.BandwidthMbps)
		args = append(args, "limits.ingress="+limit, "limits.egress="+limit)
	}
	return args
}

// ensureIncusBridge 保证 NAT 默认桥 incusbr0 存在：缺失时按
// 10.10.10.1/24 创建并开 NAT。不存在且创建失败则返回错误，
// 上层不再用 10.10.10.x 回退地址盲 launch。
func (d *Incus) ensureIncusBridge(ctx context.Context) error {
	if d.isManagedNetwork(ctx, "incusbr0") {
		return nil
	}
	// 已存在但非托管的同名 linux bridge 会阻止托管网络创建：
	// 此时不再盲目 network create，而是直接报错让管理员处理，
	// 避免把“桥名被占用”掩盖成后续 device add 的校验失败。
	if err := run(ctx, d.cli(), "network", "create", "incusbr0",
		"ipv4.address=10.10.10.1/24", "ipv4.nat=true",
		"ipv6.address=auto", "ipv6.nat=true"); err != nil {
		if contains(err.Error(), "already exists") && d.isManagedNetwork(ctx, "incusbr0") {
			return nil
		}
		return fmt.Errorf("创建默认 NAT 网络 incusbr0 失败: %w", err)
	}
	return nil
}

// isManagedNetwork 判断指定名称是否为 Incus 托管网络。
// 托管网络必须用 network= 挂载；非托管父桥必须用 nictype=bridged parent= 且不能带 ipv4.address。
func (d *Incus) isManagedNetwork(ctx context.Context, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	out, err := output(ctx, d.cli(), "network", "show", name)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(strings.ToLower(line))
		line = strings.ReplaceAll(line, "\"", "")
		if strings.HasPrefix(line, "managed:") && strings.Contains(line, "true") {
			return true
		}
	}
	// 没有 managed 字段时按非托管处理（fail-closed）：错误走 network= 分支会在 device add 阶段明确报错，而不是静默用错语法。
	return false
}

// ensureStoragePool 返回可用的存储池名：优先 default，否则取现存第一个。
// HK 等节点没有 default 池（只有 s1/incusbr01），硬编码会导致
// profile root pool=default 残留、launch 报 Pool not found。
func (d *Incus) ensureStoragePool(ctx context.Context) (string, error) {
	out, err := output(ctx, d.cli(), "storage", "list", "--format", "csv", "-c", "n")
	if err == nil {
		var pools []string
		for _, line := range strings.Split(string(out), "\n") {
			name := strings.TrimSpace(strings.Trim(line, "\" "))
			if name == "" || strings.EqualFold(name, "NAME") {
				continue
			}
			if idx := strings.Index(name, ","); idx >= 0 {
				name = strings.TrimSpace(strings.Trim(name[:idx], "\" "))
			}
			if name != "" {
				pools = append(pools, name)
			}
		}
		for _, p := range pools {
			if p == "default" {
				return "default", nil
			}
		}
		if len(pools) > 0 {
			return pools[0], nil
		}
	}
	if out2, err2 := output(ctx, d.cli(), "storage", "list"); err2 == nil {
		if pool := parseStoragePoolTable(string(out2)); pool != "" {
			return pool, nil
		}
	}
	if err := run(ctx, d.cli(), "storage", "create", "default", "dir"); err == nil {
		return "default", nil
	} else if contains(err.Error(), "already exists") {
		return "default", nil
	}
	return "", fmt.Errorf("未找到可用存储池，且自动创建 default 失败: %w", err)
}

// parseStoragePoolTable 从 `incus storage list` 表格中提取首选池名。
func parseStoragePoolTable(table string) string {
	var pools []string
	for _, line := range strings.Split(table, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 2 {
			continue
		}
		name := strings.TrimSpace(cols[1])
		if name == "" || strings.EqualFold(name, "NAME") {
			continue
		}
		pools = append(pools, name)
	}
	for _, p := range pools {
		if p == "default" {
			return "default"
		}
	}
	if len(pools) > 0 {
		return pools[0]
	}
	return ""
}

// profileDeviceValue 读取 profile 上某设备的单个键（如 root.pool）。
// 设备或键不存在返回空串（调用方可按“未知”处理，不报错）。
func profileDeviceValue(ctx context.Context, cli, profile, device, key string) string {
	out, err := output(ctx, cli, "profile", "device", "get", profile, device, key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// profileRootDeviceArgs chooses add for a missing root device and set only
// when an existing root has a new disk quota.
func profileRootDeviceArgs(profile string, exists bool, diskGB int, pool string) []string {
	if strings.TrimSpace(pool) == "" {
		pool = "default"
	}
	if exists {
		if diskGB <= 0 {
			return nil
		}
		return []string{"profile", "device", "set", profile, "root", fmt.Sprintf("size=%dGiB", diskGB)}
	}
	args := []string{"profile", "device", "add", profile, "root", "disk", "path=/", "pool=" + pool}
	if diskGB > 0 {
		args = append(args, fmt.Sprintf("size=%dGiB", diskGB))
	}
	return args
}

// profileHasDevice parses a profile JSON document. A missing devices map or
// device is a valid "not found" result; malformed JSON is a real error.
func profileHasDevice(data []byte, device string) (bool, error) {
	// `incus query /1.0/profiles/<name>` 可能直出对象，也可能带 metadata
	// 信封；`profile show` 在部分 Incus 版本不支持 --format，因此统一走
	// query。两种形态都兼容。
	var direct struct {
		Devices map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(data, &direct); err != nil {
		return false, fmt.Errorf("解析 profile 设备失败: %w", err)
	}
	if direct.Devices != nil {
		_, ok := direct.Devices[device]
		return ok, nil
	}
	var envelope struct {
		Metadata struct {
			Devices map[string]json.RawMessage `json:"devices"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false, fmt.Errorf("解析 profile 设备失败: %w", err)
	}
	_, ok := envelope.Metadata.Devices[device]
	return ok, nil
}

// profileDeviceExists checks a profile device without relying on error text
// from a mutating remove command.
func profileDeviceExists(ctx context.Context, cli, profile, device string) (bool, error) {
	out, err := output(ctx, cli, "query", "/1.0/profiles/"+profile)
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message != "" {
			return false, fmt.Errorf("读取 profile 失败: %w: %s", err, message)
		}
		return false, fmt.Errorf("读取 profile 失败: %w", err)
	}
	return profileHasDevice(out, device)
}

func (d *Incus) ensureImageAliasCleaned(ctx context.Context, alias string) {
	// 镜像已随实例 launch 挂载，别名只是导入时的临时名字，删掉防堆积。
	_ = run(context.WithoutCancel(ctx), d.cli(), "image", "delete", alias)
}

func (d *Incus) Delete(ctx context.Context, inst *protocol.Instance) error {
	containerVNC.stop(d.Name(), inst)
	err := run(ctx, d.cli(), "delete", resourceName("incus", inst), "--force")
	if err != nil && !contains(err.Error(), "not found") {
		return err
	}
	// 实例级 profile 用完即删：否则每次开通都会留下一个 virtualis-p-N
	// 残留（profile 不随实例删除自动回收）。profile 不存在时忽略报错。
	_ = run(ctx, d.cli(), "profile", "delete", fmt.Sprintf("virtualis-p-%d", inst.ID))
	return nil
}

func (d *Incus) Start(ctx context.Context, inst *protocol.Instance) error {
	err := run(ctx, d.cli(), "start", resourceName("incus", inst))
	// launch 创建的实例一落地就在运行，"already running" 视为成功。
	if err != nil && !contains(err.Error(), "already running") {
		return err
	}
	return nil
}
func (d *Incus) Stop(ctx context.Context, inst *protocol.Instance) error {
	containerVNC.stop(d.Name(), inst)
	err := run(ctx, d.cli(), "stop", resourceName("incus", inst))
	if err != nil && !contains(err.Error(), "not running") {
		return err
	}
	return nil
}
func (d *Incus) Restart(ctx context.Context, inst *protocol.Instance) error {
	return run(ctx, d.cli(), "restart", resourceName("incus", inst))
}
func (d *Incus) HardStart(ctx context.Context, inst *protocol.Instance) error {
	return d.Start(ctx, inst)
}
func (d *Incus) HardStop(ctx context.Context, inst *protocol.Instance) error {
	containerVNC.stop(d.Name(), inst)
	return run(ctx, d.cli(), "stop", resourceName("incus", inst), "--force")
}
func (d *Incus) HardRestart(ctx context.Context, inst *protocol.Instance) error {
	if err := d.HardStop(ctx, inst); err != nil {
		return err
	}
	return d.HardStart(ctx, inst)
}
func (d *Incus) Reinstall(ctx context.Context, inst *protocol.Instance) error {
	if err := d.Delete(ctx, inst); err != nil {
		return err
	}
	return d.Create(ctx, inst)
}

func (d *Incus) Status(ctx context.Context, inst *protocol.Instance) (string, error) {
	out, err := output(ctx, d.cli(), "list", resourceName("incus", inst), "--format", "csv", "-c", "ns")
	if err != nil {
		return "", fmt.Errorf("读取 Incus 实例状态失败: %w", err)
	}
	if strings.Contains(strings.ToLower(string(out)), "running") {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

// incusState 是 incus query .../state 里采集需要字段的子集。
type incusState struct {
	CPU struct {
		Usage uint64 `json:"usage"`
	} `json:"cpu"`
	Memory struct {
		Usage uint64 `json:"usage"`
		Total uint64 `json:"total"`
	} `json:"memory"`
	Network map[string]struct {
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
			Scope   string `json:"scope"`
		} `json:"addresses"`
		Counters struct {
			BytesReceived uint64 `json:"bytes_received"`
			BytesSent     uint64 `json:"bytes_sent"`
		} `json:"counters"`
		HWAddr string `json:"hwaddr"`
		State  string `json:"state"`
	} `json:"network"`
}

// queryState 拉取实例的运行状态 JSON（incus query 的权威数据源）。
func (d *Incus) queryState(ctx context.Context, name string) (*incusState, error) {
	out, err := output(ctx, d.cli(), "query", "/1.0/instances/"+name+"/state")
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Metadata *incusState `json:"metadata"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil || envelope.Metadata == nil {
		// 某些版本直接返回状态对象。
		var direct incusState
		if err2 := json.Unmarshal(out, &direct); err2 != nil {
			return nil, fmt.Errorf("解析实例状态失败: %w", err)
		}
		return &direct, nil
	}
	return envelope.Metadata, nil
}

func (d *Incus) Metrics(ctx context.Context, inst *protocol.Instance) (protocol.Metrics, error) {
	name := resourceName("incus", inst)
	state, err := d.queryState(ctx, name)
	if err != nil {
		return protocol.Metrics{}, err
	}
	metrics := defaultMetrics(inst)
	metrics.CollectedAt = time.Now().UTC()
	if state.Memory.Total > 0 {
		metrics.MemoryTotalMB = int64(state.Memory.Total / 1024 / 1024)
	}
	metrics.MemoryUsedMB = int64(state.Memory.Usage / 1024 / 1024)
	var rxBytes, txBytes uint64
	for _, net := range state.Network {
		rxBytes += net.Counters.BytesReceived
		txBytes += net.Counters.BytesSent
	}
	metrics.NetworkRxBytes = rxBytes
	metrics.NetworkTxBytes = txBytes

	// CPU 与带宽都是累计值：与上次采样差分出速率。
	d.mu.Lock()
	previous, ok := d.samples[inst.ID]
	d.samples[inst.ID] = incusSample{cpuTime: state.CPU.Usage, rx: rxBytes, tx: txBytes, at: metrics.CollectedAt}
	d.mu.Unlock()
	if ok {
		seconds := metrics.CollectedAt.Sub(previous.at).Seconds()
		if seconds > 0 && state.CPU.Usage >= previous.cpuTime {
			cores := inst.Spec.CPU
			if cores < 1 {
				cores = 1
			}
			metrics.CPUPercent = float64(state.CPU.Usage-previous.cpuTime) / (seconds * 1e9 * float64(cores)) * 100
		}
		if seconds > 0 && rxBytes >= previous.rx {
			metrics.BandwidthRxBps = float64(rxBytes-previous.rx) / seconds
		}
		if seconds > 0 && txBytes >= previous.tx {
			metrics.BandwidthTxBps = float64(txBytes-previous.tx) / seconds
		}
	}
	if metrics.CPUPercent < 0 {
		metrics.CPUPercent = 0
	}
	if metrics.CPUPercent > 100 {
		metrics.CPUPercent = 100
	}
	return metrics, nil
}

func (d *Incus) Network(ctx context.Context, inst *protocol.Instance) (protocol.NetworkStatus, error) {
	name := resourceName("incus", inst)
	status := protocol.NetworkStatus{CheckedAt: time.Now().UTC()}
	state, err := d.queryState(ctx, name)
	if err != nil {
		status.Error = "实例未运行或状态不可读"
		return status, nil
	}
	reachable := false
	for ifaceName, net := range state.Network {
		item := protocol.NetworkInterface{Name: ifaceName, MAC: net.HWAddr, State: net.State}
		if item.State == "" {
			item.State = "up"
		}
		for _, addr := range net.Addresses {
			// 只显示全局地址：local/link scope 是环回与链路本地。
			if addr.Scope != "global" {
				continue
			}
			if addr.Family == "inet" {
				item.IPv4 = append(item.IPv4, addr.Address)
			} else {
				item.IPv6 = append(item.IPv6, addr.Address)
			}
		}
		item.RxBytes = net.Counters.BytesReceived
		item.TxBytes = net.Counters.BytesSent
		if len(item.IPv4) > 0 || len(item.IPv6) > 0 {
			reachable = true
		}
		status.Interfaces = append(status.Interfaces, item)
	}
	status.Reachable = reachable
	if !reachable {
		status.Error = "实例网卡已连接但未获取到全局 IP"
	}
	if inst.Network.IPv4 != "" && len(status.Interfaces) > 0 {
		// Static NAT reservation is authoritative even if Incus reports the
		// address with a non-global scope during early boot.
		for i := range status.Interfaces {
			if status.Interfaces[i].Name == "eth0" {
				found := false
				for _, ip := range status.Interfaces[i].IPv4 {
					if strings.Split(ip, "/")[0] == strings.Split(inst.Network.IPv4, "/")[0] {
						found = true
						break
					}
				}
				if !found {
					status.Interfaces[i].IPv4 = append(status.Interfaces[i].IPv4, strings.Split(inst.Network.IPv4, "/")[0])
				}
				status.Reachable = true
				status.Error = ""
				break
			}
		}
	}
	return status, nil
}

// ensureContainerIPv4 确保容器的 eth0 真的拿到了期望的静态 IPv4。
//
// Incus 的 dnsmasq 静态租约偶发竞态：profile 配好保留地址，容器首次
// DHCP 请求却没拿到（lease 表有 STATIC 条目、容器内只有 IPv6 链路本地）。
// 创建时就地修复而不是留到"第一次启动才发现"：轮询 → 强制重启再轮询 →
// 最终兜底在容器内直接静态加地址+网关。全部失败才返回错误。
func (d *Incus) ensureContainerIPv4(ctx context.Context, name, expectIP string) error {
	if expectIP == "" {
		return nil
	}
	hasIPv4 := func() bool {
		state, err := d.queryState(ctx, name)
		if err != nil {
			return false
		}
		net, ok := state.Network["eth0"]
		if !ok {
			return false
		}
		for _, addr := range net.Addresses {
			if addr.Family == "inet" && addr.Scope == "global" {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if hasIPv4() {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	// DHCP 竞态：重启容器重新走一遍 DHCP。
	_ = run(ctx, d.cli(), "restart", name, "--force")
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if hasIPv4() {
			log.Printf("容器 %s 重启后已获取 IPv4", name)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	// 最终兜底：容器内直接静态配置（网关取 incusbr0 地址）。
	// natSlotIPOn 返回 (guestIP, gatewayIP)，这里必须取第二个值；
	// 旧代码误用 guestIP（如 10.10.10.101）作网关，会让容器失去外网，
	// 随后的 openssh-server 安装必然失败。
	_, gateway := natSlotIPOn("incusbr0", &protocol.Instance{ID: 1})
	gw := gateway
	ip := strings.Split(expectIP, "/")[0]
	script := "ip addr add " + ip + "/24 dev eth0 2>/dev/null; ip link set eth0 up"
	if gw != "" {
		script += "; ip route replace default via " + gw + " dev eth0"
	}
	// 静态路径没有 DHCP 下发的 DNS：不配置会导致 apt 解析域名失败
	// （openssh 安装必然失败）。优先交给 systemd-resolved 管理，
	// 同时直接写 resolv.conf 兜底精简镜像。
	if gw != "" {
		script += "; resolvectl dns eth0 " + gw + " 2>/dev/null || true"
	}
	script += "; printf 'nameserver " + mapNonEmpty(gw, "223.5.5.5") + "\nnameserver 114.114.114.114\n' > /etc/resolv.conf 2>/dev/null || true"
	if err := run(ctx, d.cli(), "exec", name, "--", "sh", "-c", script); err != nil {
		return fmt.Errorf("静态兜底配置失败: %w", err)
	}
	log.Printf("容器 %s DHCP 未就绪，已在容器内静态配置 %s", name, ip)
	return nil
}

// mapNonEmpty 空值兜底。
func mapNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// SetRootPassword 注入 root 密码并确保 SSH 可用：
// 1) 容器就绪前 exec 会失败，做有限重试；
// 2) 精简镜像不带 sshd，按发行版包管理器安装；
// 3) 放行 root 密码登录、校验配置并启动 sshd；
// 4) 任一步失败都返回错误，绝不能把“仅 chpasswd 成功”冒充为 SSH 可用。
func (d *Incus) SetRootPassword(ctx context.Context, inst *protocol.Instance, password string) error {
	name := resourceName("incus", inst)
	exec := func(timeout context.Context, args ...string) error {
		full := append([]string{"exec", name, "--"}, args...)
		return run(timeout, d.cli(), full...)
	}
	// 容器 init 未完成时 exec 报错：最多等 30 秒。
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(8 * time.Second):
			}
		}
		lastErr = exec(ctx, "true")
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return fmt.Errorf("等待容器初始化失败: %w", lastErr)
	}

	if exec(ctx, "sh", "-c", "command -v sshd >/dev/null 2>&1 || test -x /usr/sbin/sshd") != nil {
		// 安装包之前先确保容器有正确的 IPv4 和默认路由。
		var ipv4 string
		if state, err := d.queryState(ctx, name); err == nil {
			for _, addr := range state.Network["eth0"].Addresses {
				if addr.Family == "inet" && addr.Scope == "global" {
					ipv4 = addr.Address
					break
				}
			}
		}
		if ipv4 == "" && inst.Network.IPv4 != "" {
			if err := d.ensureContainerIPv4(ctx, name, inst.Network.IPv4); err != nil {
				return fmt.Errorf("安装 sshd 前配置 IPv4 失败: %w", err)
			}
		}
		install := "if command -v apt-get >/dev/null 2>&1; then " +
			"export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq openssh-server; " +
			"elif command -v apk >/dev/null 2>&1; then apk add --no-cache openssh; " +
			"elif command -v dnf >/dev/null 2>&1; then dnf install -y openssh-server; " +
			"elif command -v yum >/dev/null 2>&1; then yum install -y openssh-server; " +
			"elif command -v pacman >/dev/null 2>&1; then pacman -Sy --noconfirm openssh; " +
			"else echo 'unsupported package manager' >&2; exit 127; fi"
		if err := exec(ctx, "sh", "-c", install); err != nil {
			return fmt.Errorf("安装 SSH 服务失败: %w", err)
		}
	}
	if err := exec(ctx, "sh", "-c", "command -v sshd >/dev/null 2>&1 || test -x /usr/sbin/sshd"); err != nil {
		return fmt.Errorf("安装后仍找不到 sshd: %w", err)
	}
	if err := exec(ctx, "sh", "-c", "echo root:"+shellQuote(password)+" | chpasswd"); err != nil {
		return fmt.Errorf("设置 root 密码失败: %w", err)
	}
	config := "mkdir -p /etc/ssh/sshd_config.d && printf '%s\\n' 'PermitRootLogin yes' 'PasswordAuthentication yes' > /etc/ssh/sshd_config.d/00-virtualis.conf"
	if err := exec(ctx, "sh", "-c", config); err != nil {
		return fmt.Errorf("写入 SSH 登录配置失败: %w", err)
	}
	if err := exec(ctx, "sh", "-c", "sshd -t"); err != nil {
		return fmt.Errorf("SSH 配置校验失败: %w", err)
	}
	// ssh 必须在 chpasswd + drop-in 全部就位后（重）启动：实测镜像自带的
	// ssh.service 若在容器刚启动、shadow/配置尚未就绪时被拉起，会一直以
	// PAM authentication failure 拒绝 root 密码（非 root 不受影响），只有
	// 重启 sshd 进程才能恢复。restart 对未启动的 ssh 同样生效。
	start := "systemctl enable ssh 2>/dev/null; systemctl restart ssh 2>/dev/null || systemctl restart sshd 2>/dev/null || service ssh restart 2>/dev/null || service sshd restart 2>/dev/null || /usr/sbin/sshd"
	if err := exec(ctx, "sh", "-c", start); err != nil {
		return fmt.Errorf("启动 SSH 服务失败: %w", err)
	}
	if err := exec(ctx, "sh", "-c", "pgrep -x sshd >/dev/null 2>&1 || ss -lnt 2>/dev/null | grep -q ':22 '"); err != nil {
		return fmt.Errorf("SSH 服务启动后未监听 22 端口: %w", err)
	}
	return nil
}

// shellQuote 返回可安全嵌入 POSIX shell 单引号字符串的内容。
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// VNC 返回容器控制台的本地 VNC 端口（Xvfb + xterm + x11vnc 桥接）。
func (d *Incus) VNC(ctx context.Context, inst *protocol.Instance, _ string) (protocol.VNCInfo, error) {
	port, err := containerVNC.ensure(ctx, d.Name(), inst,
		func() bool { s, _ := d.Status(ctx, inst); return s == StatusRunning },
		func(name string) []string { return []string{d.cli(), "exec", name, "--", "/bin/bash", "-l"} })
	if err != nil {
		return protocol.VNCInfo{Available: false, Message: err.Error()}, nil
	}
	return protocol.VNCInfo{
		Available: true, Protocol: "vnc", Host: "127.0.0.1", Port: port,
		Display: ":" + strconv.Itoa(port-5900), URL: fmt.Sprintf("vnc://127.0.0.1:%d", port),
	}, nil
}

func parseMiB(line string) int64 {
	fields := strings.Fields(line)
	for _, field := range fields {
		field = strings.TrimSuffix(strings.TrimSuffix(field, "MiB"), "MB")
		if value, err := strconv.ParseInt(field, 10, 64); err == nil {
			return value
		}
	}
	return 0
}
