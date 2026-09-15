package driver

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// Package traffic 实现累计流量配额的计量与持久化。
//
// hypervisor 的网卡计数器（incus state / virsh domstats）在实例重启后会
// 清零，因此不能直接用单次 Metrics 的 rx+tx 判断配额。这里按实例维护一个
// 累计文件 traffic_<id>.json：每次 Metrics() 用“差分累加”更新 used_bytes，
// 计数器回退（current < last）时把 current 视为新基线、已累积值保留。

const bytesPerGB = uint64(1024) * 1024 * 1024

// TrafficQuotaBytes 把 GB 配额换算为字节。quota<=0 表示不限，返回 0。
func TrafficQuotaBytes(quotaGB int) uint64 {
	if quotaGB <= 0 {
		return 0
	}
	return uint64(quotaGB) * bytesPerGB
}

// TrafficQuotaExceeded 报告累计用量是否达到配额（quota<=0 永不超限）。
func TrafficQuotaExceeded(usedBytes uint64, quotaGB int) bool {
	quota := TrafficQuotaBytes(quotaGB)
	if quota == 0 {
		return false
	}
	return usedBytes >= quota
}

// AccumulateTrafficUsage 按差分累加流量，兼容计数器重置：
//
//	current >= last 时增量为 current-last；current < last 说明计数器
//	重置（重启/重建），把 current 本身作为新基线后的增量，已累积值保留。
func AccumulateTrafficUsage(prevUsed, prevRx, prevTx, curRx, curTx uint64) (newUsed, newLastRx, newLastTx uint64) {
	var deltaRx, deltaTx uint64
	if curRx >= prevRx {
		deltaRx = curRx - prevRx
	} else {
		deltaRx = curRx
	}
	if curTx >= prevTx {
		deltaTx = curTx - prevTx
	} else {
		deltaTx = curTx
	}
	return prevUsed + deltaRx + deltaTx, curRx, curTx
}

// trafficState 是 traffic_<id>.json 的落盘形态。
type trafficState struct {
	UsedBytes uint64 `json:"used_bytes"`
	LastRx    uint64 `json:"last_rx"`
	LastTx    uint64 `json:"last_tx"`
	QuotaGB   int    `json:"quota_gb"`
	// OriginalNetwork 保存断网前的原始网络配置，用于超限恢复后重连。
	OriginalNetwork *protocol.NetworkConfig `json:"original_network,omitempty"`
	// Disconnected 为 true 表示已因超限执行断网（incus mode=none / qemu link down）。
	Disconnected bool `json:"disconnected"`
}

func trafficStatePath(dataDir string, id uint) string {
	return filepath.Join(dataDir, "traffic_"+strconv.FormatUint(uint64(id), 10)+".json")
}

// errTrafficStateCorrupt 表示累计文件存在但 JSON 已损坏：调用方必须跳过
// 本次累加、不落盘，绝不能用零值覆盖（否则累计用量永久丢失）。
var errTrafficStateCorrupt = errors.New("流量累计文件损坏")

// trafficLocks 按实例串行化累计文件的“读→算→写”：Metrics 采样与执法标记
// 并发落同一文件会丢增量，enforcer 周期与 metrics 请求可能重叠。
var trafficLocks sync.Map // string -> *sync.Mutex

// trafficLockFor 返回某实例累计文件的互斥锁（per-ID，目录隔离）。
func trafficLockFor(dataDir string, id uint) *sync.Mutex {
	key := dataDir + "\x00" + strconv.FormatUint(uint64(id), 10)
	mu, _ := trafficLocks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func loadTrafficState(dataDir string, id uint) trafficState {
	st, _ := loadTrafficStateStrict(dataDir, id)
	return st
}

// loadTrafficStateStrict 是可辨错的累计文件读取：文件缺失/为空视为全新
// （返回零值 + nil）；存在但 JSON 损坏返回 errTrafficStateCorrupt。
func loadTrafficStateStrict(dataDir string, id uint) (trafficState, error) {
	var st trafficState
	if dataDir == "" {
		return st, nil
	}
	raw, err := os.ReadFile(trafficStatePath(dataDir, id))
	if err != nil || len(raw) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return trafficState{}, errTrafficStateCorrupt
	}
	return st, nil
}

func saveTrafficState(dataDir string, id uint, st trafficState) {
	if dataDir == "" {
		return
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.MkdirAll(dataDir, 0o755)
	tmp := trafficStatePath(dataDir, id) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, trafficStatePath(dataDir, id))
}

func removeTrafficState(dataDir string, id uint) {
	if dataDir == "" {
		return
	}
	_ = os.Remove(trafficStatePath(dataDir, id))
}

// observeTraffic 是 Metrics() 的公共尾巴：累加 rx+tx 并落盘，返回累计值与超限标记。
// quotaGB 取自实例当前网络配置（最新值覆盖落盘值，0 表示本次不限）。
// 读→算→写全程持有 per-ID 锁；文件损坏时跳过本次、不落盘（保旧不归零）。
func observeTraffic(dataDir string, id uint, quotaGB int, curRx, curTx uint64) (used uint64, exceeded bool) {
	mu := trafficLockFor(dataDir, id)
	mu.Lock()
	defer mu.Unlock()
	st, err := loadTrafficStateStrict(dataDir, id)
	if err != nil {
		return 0, false
	}
	used, lastRx, lastTx := AccumulateTrafficUsage(st.UsedBytes, st.LastRx, st.LastTx, curRx, curTx)
	st.UsedBytes = used
	st.LastRx = lastRx
	st.LastTx = lastTx
	st.QuotaGB = quotaGB
	saveTrafficState(dataDir, id, st)
	return used, TrafficQuotaExceeded(used, quotaGB)
}
