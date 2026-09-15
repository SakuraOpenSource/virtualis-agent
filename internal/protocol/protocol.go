package protocol

import "time"

type InstanceSpec struct {
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
	Arch     string `json:"arch,omitempty"`
	// CPUMilli 是毫核表示的 CPU 配额（500 = 0.5 核）。非零时优先于 CPU：
	// CPU 字段此时存向上取整的整核数，供旧版本与 VM 使用。0 表示未设置
	// （纯整核场景），保持旧实例数据兼容。
	CPUMilli int `json:"cpu_milli,omitempty"`
}

type NetworkConfig struct {
	Mode          string   `json:"mode"`
	Bridge        string   `json:"bridge,omitempty"`
	MAC           string   `json:"mac,omitempty"`
	IPv4          string   `json:"ipv4,omitempty"`
	Gateway       string   `json:"gateway,omitempty"`
	DNS           []string `json:"dns,omitempty"`
	BandwidthMbps int      `json:"bandwidth_mbps,omitempty"`
	// TrafficGB 是累计流量配额（GB），0 表示不限。只做计量与断网，
	// 不翻译成 hypervisor 限速参数（限速仍由 BandwidthMbps 承担）。
	TrafficGB int `json:"traffic_gb,omitempty"`
}

type Image struct {
	ID           uint   `json:"id,omitempty"`
	Name         string `json:"name"`
	DisplayName  string `json:"display_name,omitempty"`
	Driver       string `json:"driver"`
	Type         string `json:"type"`
	OriginalName string `json:"original_name,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	Checksum     string `json:"checksum,omitempty"`
	Path         string `json:"path,omitempty"`
	// ExtraPath 是 Incus 分割镜像的 meta.tar.xz 本地路径。
	ExtraPath string `json:"extra_path,omitempty"`
}

// NATMapping 是一条 NAT 端口转发：宿主机的 HostPort 转发到实例的
// GuestPort。目标实例 IP 由被控在应用规则时解析（静态保留或动态查询）。
type NATMapping struct {
	Protocol  string `json:"protocol"` // tcp / udp
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
}

type Instance struct {
	ID          uint          `json:"id"`
	Name        string        `json:"name"`
	DisplayName string        `json:"display_name,omitempty"`
	Driver      string        `json:"driver"`
	Type        string        `json:"type"`
	Status      string        `json:"status,omitempty"`
	ImageID     *uint         `json:"image_id,omitempty"`
	Spec        InstanceSpec  `json:"spec"`
	Network     NetworkConfig `json:"network"`
	Image       *Image        `json:"image,omitempty"`
	// NATMappings 是主控落库的期望清单，被控开机时据此配置 DNAT，
	// 关机/删除时清除。整表下发，由被控幂等对账。
	NATMappings []NATMapping `json:"nat_mappings,omitempty"`
	// RootPassword 只在创建请求里出现：agent 把它写进系统盘（QEMU）或
	// 直接 chpasswd（容器），之后不再传输，也不出现在状态回包中。
	RootPassword string `json:"root_password,omitempty"`
	// SSHReady 是被控的回包字段：首次密码注入（含 sshd 可用性校验）完成后
	// 置 true，主控据此回写实例的 ssh_ready。创建/重装请求里传不传都忽略。
	SSHReady bool `json:"ssh_ready,omitempty"`
}

type Metrics struct {
	CPUPercent     float64   `json:"cpu_percent"`
	MemoryUsedMB   int64     `json:"memory_used_mb"`
	MemoryTotalMB  int64     `json:"memory_total_mb"`
	NetworkRxBytes uint64    `json:"network_rx_bytes"`
	NetworkTxBytes uint64    `json:"network_tx_bytes"`
	BandwidthRxBps float64   `json:"bandwidth_rx_bps"`
	BandwidthTxBps float64   `json:"bandwidth_tx_bps"`
	CollectedAt    time.Time `json:"collected_at"`
	// TrafficUsedBytes 是本实例累计使用的总流量（rx+tx，持久化累加，
	// 重启/计数器重置不丢失）。0 表示尚未累积。
	TrafficUsedBytes uint64 `json:"traffic_used_bytes,omitempty"`
	// TrafficQuotaExceeded 为 true 表示累计用量已达到配额（quota>0）。
	TrafficQuotaExceeded bool `json:"traffic_quota_exceeded,omitempty"`
}

type NetworkInterface struct {
	Name    string   `json:"name"`
	MAC     string   `json:"mac,omitempty"`
	State   string   `json:"state,omitempty"`
	IPv4    []string `json:"ipv4,omitempty"`
	IPv6    []string `json:"ipv6,omitempty"`
	RxBytes uint64   `json:"rx_bytes"`
	TxBytes uint64   `json:"tx_bytes"`
}

type NetworkStatus struct {
	Reachable  bool               `json:"reachable"`
	LatencyMS  float64            `json:"latency_ms"`
	Interfaces []NetworkInterface `json:"interfaces"`
	Error      string             `json:"error,omitempty"`
	CheckedAt  time.Time          `json:"checked_at"`
}

type VNCInfo struct {
	Available bool   `json:"available"`
	Protocol  string `json:"protocol,omitempty"`
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Display   string `json:"display,omitempty"`
	URL       string `json:"url,omitempty"`
	WebURL    string `json:"web_url,omitempty"`
	Message   string `json:"message,omitempty"`
}
