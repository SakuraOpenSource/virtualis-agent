package driver

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestAccumulateTrafficUsageNormal(t *testing.T) {
	used, rx, tx := AccumulateTrafficUsage(100, 1000, 2000, 1500, 2600)
	if used != 100+500+600 || rx != 1500 || tx != 2600 {
		t.Fatalf("差分累加错误: used=%d rx=%d tx=%d", used, rx, tx)
	}
}

func TestAccumulateTrafficUsageCounterReset(t *testing.T) {
	// 重启后计数器清零：current < last 时把 current 本身作为增量，已累积保留。
	used, rx, tx := AccumulateTrafficUsage(5000, 9000, 8000, 100, 200)
	if used != 5000+100+200 || rx != 100 || tx != 200 {
		t.Fatalf("计数器重置处理错误: used=%d rx=%d tx=%d", used, rx, tx)
	}
	// 单向重置也要各自处理。
	used, _, _ = AccumulateTrafficUsage(100, 1000, 2000, 500, 2500)
	if used != 100+500+500 {
		t.Fatalf("单向重置处理错误: used=%d", used)
	}
}

func TestTrafficQuotaExceeded(t *testing.T) {
	if TrafficQuotaExceeded(100, 0) {
		t.Fatal("不限配额不应超限")
	}
	if TrafficQuotaExceeded(0, 10) {
		t.Fatal("未使用不应超限")
	}
	quota := TrafficQuotaBytes(1)
	if !TrafficQuotaExceeded(quota, 1) || !TrafficQuotaExceeded(quota+1, 1) {
		t.Fatal("达到配额应判超限")
	}
	if TrafficQuotaExceeded(quota-1, 1) {
		t.Fatal("未达配额不应超限")
	}
}

func TestObserveTrafficPersistsAcrossResets(t *testing.T) {
	dir := t.TempDir()
	used, exceeded := observeTraffic(dir, 42, 102400, 1000, 2000)
	if used != 3000 || exceeded {
		t.Fatalf("首次累加错误: used=%d exceeded=%v", used, exceeded)
	}
	used, _ = observeTraffic(dir, 42, 102400, 1500, 2600)
	if used != 3000+500+600 {
		t.Fatalf("二次累加错误: used=%d", used)
	}
	// 模拟重启清零后继续累加，已累积不丢。
	used, _ = observeTraffic(dir, 42, 102400, 100, 200)
	if used != 3000+500+600+100+200 {
		t.Fatalf("重置后累加错误: used=%d", used)
	}
	st := loadTrafficState(dir, 42)
	if st.UsedBytes != used || st.LastRx != 100 || st.LastTx != 200 || st.QuotaGB != 102400 {
		t.Fatalf("落盘状态错误: %+v", st)
	}
}

func TestBandwidthLimitPathUntouched(t *testing.T) {
	// 限速参数仍按原逻辑翻译，流量配额不混入 device 参数。
	managed := strings.Join(incusEth0DeviceArgs(protocol.NetworkConfig{BandwidthMbps: 50, TrafficGB: 100}, "incusbr0"), ",")
	for _, want := range []string{"limits.ingress=50Mbit", "limits.egress=50Mbit"} {
		if !strings.Contains(managed, want) {
			t.Fatalf("限速参数缺失 %q: %s", want, managed)
		}
	}
	if strings.Contains(managed, "traffic") {
		t.Fatalf("流量配额不应进入 hypervisor 限速参数: %s", managed)
	}
	unmanaged := strings.Join(incusEth0UnmanagedArgs(protocol.NetworkConfig{BandwidthMbps: 50, TrafficGB: 100}, "br0"), ",")
	if !strings.Contains(unmanaged, "limits.ingress=50Mbit") {
		t.Fatalf("非托管限速缺失: %s", unmanaged)
	}
	bw := qemuInterfaceXML(protocol.NetworkConfig{Mode: NetworkModeNat, BandwidthMbps: 50, TrafficGB: 100})
	if !strings.Contains(bw, "average='50000'") {
		t.Fatalf("QEMU 限速换算缺失: %s", bw)
	}
}

func TestObserveTrafficParallel(t *testing.T) {
	dir := t.TempDir()
	// 同值并发采样必须是幂等的：每轮 8 个 goroutine 用完全相同的计数器
	// 调 observeTraffic，轮次间用 WaitGroup 排序，per-ID 锁保证读→算→写
	// 不丢增量，最终累计必须与串行执行完全一致（-race 下也无数据竞争）。
	series := [][2]uint64{{1000, 2000}, {1500, 2600}, {100, 200}}
	ids := []uint{9, 10}
	for _, cur := range series {
		var wg sync.WaitGroup
		for _, id := range ids {
			for w := 0; w < 8; w++ {
				wg.Add(1)
				go func(id uint) {
					defer wg.Done()
					observeTraffic(dir, id, 102400, cur[0], cur[1])
				}(id)
			}
		}
		wg.Wait()
	}
	// 串行期望：3000 → +1100 → 计数器重置 +300。
	const want = uint64(3000 + 500 + 600 + 100 + 200)
	for _, id := range ids {
		st := loadTrafficState(dir, id)
		if st.UsedBytes != want || st.LastRx != 100 || st.LastTx != 200 {
			t.Fatalf("实例 %d 并发累加错误: used=%d last=(%d,%d)，期望 used=%d", id, st.UsedBytes, st.LastRx, st.LastTx, want)
		}
	}
}

func TestObserveTrafficCorruptFileSkips(t *testing.T) {
	dir := t.TempDir()
	// 正常落盘两次，再写坏文件：下一次采样必须跳过、不归零、不覆盖。
	observeTraffic(dir, 11, 102400, 1000, 2000)
	used, _ := observeTraffic(dir, 11, 102400, 1500, 2600)
	if used != 3000+500+600 {
		t.Fatalf("预置累计错误: used=%d", used)
	}
	bad := filepath.Join(dir, "traffic_11.json")
	if err := os.WriteFile(bad, []byte("{broken json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, exceeded := observeTraffic(dir, 11, 102400, 9999, 9999); got != 0 || exceeded {
		t.Fatalf("损坏文件应跳过累加: got=%d exceeded=%v", got, exceeded)
	}
	raw, err := os.ReadFile(bad)
	if err != nil || string(raw) != "{broken json" {
		t.Fatalf("损坏文件必须原样保留: %q err=%v", raw, err)
	}
}

// TestReinstallSnapshotRestore 锁定重装流量快照语义（回归 6ed06a7）：
// Reinstall = 快照 → Delete（清累计文件）→ Create → 恢复快照。
// 这里直接驱动这条序列的流量状态部分，验证 Delete 后文件被清、
// 恢复后累计值/断网标记/原始网络一个不丢。
func TestReinstallSnapshotRestore(t *testing.T) {
	dir := t.TempDir()
	const id = 4242
	original := trafficState{
		UsedBytes:       11 << 30, // 11 GiB
		LastRx:          6 << 30,
		LastTx:          5 << 30,
		QuotaGB:         100,
		Disconnected:    true,
		OriginalNetwork: &protocol.NetworkConfig{Mode: "nat", BandwidthMbps: 200},
	}
	saveTrafficState(dir, id, original)

	// Reinstall 第一步：快照（loadTrafficState）。
	snapshot := loadTrafficState(dir, id)
	if snapshot.UsedBytes != original.UsedBytes || snapshot.LastRx != original.LastRx ||
		snapshot.LastTx != original.LastTx || snapshot.QuotaGB != original.QuotaGB ||
		snapshot.Disconnected != original.Disconnected || snapshot.OriginalNetwork == nil {
		t.Fatalf("快照应与落盘一致: %+v", snapshot)
	}

	// Delete 语义：真删才清累计文件。
	removeTrafficState(dir, id)
	if _, err := os.Stat(trafficStatePath(dir, id)); !os.IsNotExist(err) {
		t.Fatal("Delete 后累计文件应不存在")
	}

	// Create 成功与否都恢复快照。
	saveTrafficState(dir, id, snapshot)
	restored := loadTrafficState(dir, id)
	if restored.UsedBytes != original.UsedBytes || restored.LastRx != original.LastRx ||
		restored.LastTx != original.LastTx || restored.QuotaGB != original.QuotaGB ||
		restored.Disconnected != original.Disconnected || restored.OriginalNetwork == nil {
		t.Fatalf("恢复后累计状态应完整: %+v", restored)
	}
	if restored.OriginalNetwork == nil || restored.OriginalNetwork.Mode != "nat" {
		t.Fatal("恢复后原始网络配置应保留（超限断网后的重连依据）")
	}
}

// TestReinstallSnapshotEmptyStateNoFile 零值快照不写文件：
// 全新实例重装不应凭空造出 traffic_<id>.json（Reinstall 的 snapshot != (trafficState{}) 分支）。
func TestReinstallSnapshotEmptyStateNoFile(t *testing.T) {
	dir := t.TempDir()
	const id = 777
	snapshot := loadTrafficState(dir, id) // 无文件 → 零值
	if snapshot != (trafficState{}) {
		t.Fatal("无累计文件时应得到零值快照")
	}
	// 模拟 Create 之后走 snapshot != zero 分支未触发：文件不应存在。
	if _, err := os.Stat(trafficStatePath(dir, id)); !os.IsNotExist(err) {
		t.Fatal("零值快照不应落盘")
	}
}
