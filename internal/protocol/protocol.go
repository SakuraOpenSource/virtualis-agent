package protocol

import "time"

type InstanceSpec struct {
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
	Arch     string `json:"arch,omitempty"`
}

type NetworkConfig struct {
	Mode          string   `json:"mode"`
	Bridge        string   `json:"bridge,omitempty"`
	MAC           string   `json:"mac,omitempty"`
	IPv4          string   `json:"ipv4,omitempty"`
	Gateway       string   `json:"gateway,omitempty"`
	DNS           []string `json:"dns,omitempty"`
	BandwidthMbps int      `json:"bandwidth_mbps,omitempty"`
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
