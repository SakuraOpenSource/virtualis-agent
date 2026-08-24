package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func defaultMetrics(inst *protocol.Instance) protocol.Metrics {
	total := int64(inst.Spec.MemoryMB)
	if total < 0 {
		total = 0
	}
	return protocol.Metrics{MemoryTotalMB: total, CollectedAt: time.Now().UTC()}
}

// collectHostMetrics is the portable fallback for LXC/Incus installations
// whose cgroup layout differs by distribution. QEMU uses domain-specific
// counters instead. The fallback is explicitly host-side, never fabricated.
func collectHostMetrics(inst *protocol.Instance) protocol.Metrics {
	metrics := defaultMetrics(inst)
	metrics.CollectedAt = time.Now().UTC()
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) > 0 {
			if load, parseErr := strconv.ParseFloat(fields[0], 64); parseErr == nil && runtime.NumCPU() > 0 {
				metrics.CPUPercent = load / float64(runtime.NumCPU()) * 100
			}
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, available int64
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			value, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if parseErr != nil {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				total = value / 1024
			case "MemAvailable:":
				available = value / 1024
			}
		}
		if total > 0 {
			metrics.MemoryTotalMB = total
			metrics.MemoryUsedMB = total - available
			if metrics.MemoryUsedMB < 0 {
				metrics.MemoryUsedMB = 0
			}
		}
	}
	for _, iface := range listNetworkInterfaces() {
		metrics.NetworkRxBytes += iface.RxBytes
		metrics.NetworkTxBytes += iface.TxBytes
	}
	if metrics.CPUPercent > 100 {
		metrics.CPUPercent = 100
	}
	return metrics
}

func listNetworkInterfaces() []protocol.NetworkInterface {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	result := make([]protocol.NetworkInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		result = append(result, protocol.NetworkInterface{Name: iface.Name, MAC: iface.HardwareAddr.String(), State: interfaceState(iface), RxBytes: readInterfaceCounter(iface.Name, "rx_bytes"), TxBytes: readInterfaceCounter(iface.Name, "tx_bytes")})
	}
	return result
}

func interfaceState(iface net.Interface) string {
	if iface.Flags&net.FlagUp != 0 {
		return "up"
	}
	return "down"
}

func collectHostNetwork(ctx context.Context, configured protocol.NetworkConfig) protocol.NetworkStatus {
	checked := time.Now().UTC()
	status := protocol.NetworkStatus{Reachable: false, CheckedAt: checked}
	interfaces, err := net.Interfaces()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		item := protocol.NetworkInterface{Name: iface.Name, MAC: iface.HardwareAddr.String(), State: "down"}
		if iface.Flags&net.FlagUp != 0 {
			item.State = "up"
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			value := strings.Split(address.String(), "/")[0]
			if ip := net.ParseIP(value); ip != nil {
				if ip.To4() != nil {
					item.IPv4 = append(item.IPv4, value)
				} else {
					item.IPv6 = append(item.IPv6, value)
				}
			}
		}
		item.RxBytes = readInterfaceCounter(iface.Name, "rx_bytes")
		item.TxBytes = readInterfaceCounter(iface.Name, "tx_bytes")
		status.Interfaces = append(status.Interfaces, item)
	}
	if configured.Mode == "none" {
		status.Error = "实例网络已关闭"
		return status
	}
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "1.1.1.1:53")
	if err == nil {
		status.Reachable = true
		status.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		_ = conn.Close()
	} else {
		status.Error = "无法连接外部网络"
	}
	return status
}

func parseIPCommand(text string) []protocol.NetworkInterface {
	byName := make(map[string]int)
	var result []protocol.NetworkInterface
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		if name == "" || name == "lo" {
			continue
		}
		index, exists := byName[name]
		if !exists {
			index = len(result)
			byName[name] = index
			result = append(result, protocol.NetworkInterface{Name: name, State: "up"})
		}
		family := fields[2]
		address := strings.Split(fields[3], "/")[0]
		if net.ParseIP(address) == nil {
			continue
		}
		if family == "inet" {
			result[index].IPv4 = append(result[index].IPv4, address)
		} else if family == "inet6" {
			result[index].IPv6 = append(result[index].IPv6, address)
		}
	}
	return result
}

func readInterfaceCounter(name, counter string) uint64 {
	path := filepath.Join("/sys/class/net", name, "statistics", counter)
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, _ := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return value
}

func unsupportedVNC(driverName string) (protocol.VNCInfo, error) {
	return protocol.VNCInfo{Available: false, Message: fmt.Sprintf("%s 不提供 VNC", driverName)}, nil
}
