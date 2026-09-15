package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// trafficEnforcer 是累计流量配额的后台执法器：每 60s 对内存中的实例做一次
// Metrics 采样（采样内部已做差分累加与落盘），超限则断网不断电，未超限且
// 之前已断网则用断网前保存的原始网络重连。
//
// 断网动作按驱动区分：
//   - incus：ConfigureNetwork(mode=none)，只换 profile（eth0→nictype=none）
//     并挂载，不重启容器，实例不关机，只是不再产生外网流量；
//   - qemu：ConfigureNetwork(mode=none)，内部走 virsh domif-setlink down，
//     不重建 domain、不重启，链路 down 即停流量。
//
// 原始网络保存在 <dataDir>/traffic_<id>.json 的 original_network 字段，
// agent 重启不丢失；重连成功后清除标记，累计用量保留（不清零）。
func (s *agentServer) snapshotInstances() []protocol.Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]protocol.Instance, 0, len(s.instances))
	for _, inst := range s.instances {
		out = append(out, inst)
	}
	return out
}

// trafficStore 是驱动的流量状态读写口，incus 与 qemu 均已实现。
type trafficStore interface {
	TrafficStateOf(id uint) (disconnected bool, original *protocol.NetworkConfig, used uint64, quota int)
	MarkTrafficDisconnected(id uint, original protocol.NetworkConfig, used uint64, quota int)
	MarkTrafficReconnected(id uint)
}

func (s *agentServer) enforceTrafficQuotas(ctx context.Context) {
	// 重叠 guard：上一轮清扫仍在跑（hypervisor 查询慢）时跳过本轮，
	// 避免多轮叠加打爆被控。
	if !s.enforcing.CompareAndSwap(false, true) {
		return
	}
	defer s.enforcing.Store(false)
	for _, stored := range s.snapshotInstances() {
		if stored.ID == 0 {
			continue
		}
		if stored.Network.TrafficGB <= 0 && !isTrafficDisconnected(s, stored.ID) {
			// 不限流量且未断网：无需采样，避免无意义的 hypervisor 查询。
			continue
		}
		d, ok := s.registry.Get(stored.Driver)
		if !ok {
			continue
		}
		sampled := stored
		mctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		metrics, err := d.Metrics(mctx, &sampled)
		cancel()
		if err != nil {
			continue
		}
		ts, hasStore := d.(trafficStore)
		var disconnected bool
		var original *protocol.NetworkConfig
		quota := stored.Network.TrafficGB
		if hasStore {
			var fileQuota int
			disconnected, original, _, fileQuota = ts.TrafficStateOf(stored.ID)
			// 配额取内存与落盘的最大值：主控侧扩容经任意请求进入内存，
			// 断网副本未带配额时回落文件值，扩容后采样不再超限即触发重连。
			if fileQuota > quota {
				quota = fileQuota
			}
			// Metrics 采样时若配额为 0（断网副本未带配额），回落文件中的配额。
			if quota <= 0 && original != nil && original.TrafficGB > 0 {
				quota = original.TrafficGB
			}
		}
		if !metrics.TrafficQuotaExceeded {
			if hasStore && disconnected {
				mode := strings.ToLower(strings.TrimSpace(stored.Network.Mode))
				switch {
				case original != nil && mode == "none":
					// 配额已恢复：用最新配额重连（断网时保存的原始网络可能还是旧配额）。
					restored := *original
					if quota > restored.TrafficGB {
						restored.TrafficGB = quota
					}
					s.reconnectAfterQuota(ctx, d, ts, stored, restored)
				default:
					// 网络已由主控侧恢复（内存已不是 none），或原始网络丢失：
					// 只清标记，不动网络，避免断网标记永久残留导致后续超限不再执法。
					ts.MarkTrafficReconnected(stored.ID)
				}
			}
			continue
		}
		// 已超限：mode=none 说明已经断网，幂等跳过。
		if strings.ToLower(strings.TrimSpace(stored.Network.Mode)) == "none" {
			continue
		}
		if hasStore && disconnected {
			continue
		}
		s.disconnectOnQuota(ctx, d, ts, stored, metrics.TrafficUsedBytes, quota)
	}
}

func isTrafficDisconnected(s *agentServer, id uint) bool {
	s.mu.RLock()
	stored, ok := s.instances[id]
	s.mu.RUnlock()
	if ok && strings.ToLower(strings.TrimSpace(stored.Network.Mode)) == "none" {
		return true
	}
	return false
}

func (s *agentServer) disconnectOnQuota(ctx context.Context, d driver.Driver, ts trafficStore, stored protocol.Instance, used uint64, quota int) {
	original := stored.Network
	disconnected := stored
	disconnected.Network.Mode = "none"
	dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := d.ConfigureNetwork(dctx, &disconnected); err != nil {
		// 失败不落标记：下个周期仍判超限，重试断网。先标记后失败会造成
		// “已标记、未断网”的不一致（超限流量继续跑，还不再重试）。
		log.Printf("实例 %d 流量超限（已用 %d 字节，配额 %d GB），断网失败，下周期重试: %v", stored.ID, used, quota, err)
		return
	}
	// 断网成功后再落标记，保证标记与真实网络状态一致。
	if ts != nil {
		ts.MarkTrafficDisconnected(stored.ID, original, used, quota)
	}
	s.mu.Lock()
	s.instances[stored.ID] = disconnected
	s.mu.Unlock()
	log.Printf("实例 %d 流量超限（已用 %d 字节，配额 %d GB），已断网不断电，扩容配额后自动重连", stored.ID, used, quota)
}

func (s *agentServer) reconnectAfterQuota(ctx context.Context, d driver.Driver, ts trafficStore, stored protocol.Instance, original protocol.NetworkConfig) {
	restored := stored
	restored.Network = original
	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := d.ConfigureNetwork(rctx, &restored); err != nil {
		log.Printf("实例 %d 流量配额已恢复，重连失败: %v", stored.ID, err)
		return
	}
	if ts != nil {
		ts.MarkTrafficReconnected(stored.ID)
	}
	s.mu.Lock()
	s.instances[stored.ID] = restored
	s.mu.Unlock()
	log.Printf("实例 %d 流量配额已恢复，网络已重连（mode=%s）", stored.ID, original.Mode)
}

// startTrafficEnforcer 启动 60s 周期的流量执法循环，随 ctx 结束。
func (s *agentServer) startTrafficEnforcer(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	// 启动后先做一次，60s 内超限也能尽快断网，不必等首个周期。
	done := make(chan struct{}, 1)
	go func() {
		s.enforceTrafficQuotas(ctx)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return
	}
	for {
		select {
		case <-ticker.C:
			s.enforceTrafficQuotas(ctx)
		case <-ctx.Done():
			return
		}
	}
}
