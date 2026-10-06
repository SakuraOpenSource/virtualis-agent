package main

import (
	"context"
	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"log"
	"strings"
	"time"
)

func (s *agentServer) snapshotInstances() []protocol.Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]protocol.Instance, 0, len(s.instances))
	for _, inst := range s.instances {
		out = append(out, inst)
	}
	return out
}

type trafficStore interface {
	TrafficStateOf(uint) (bool, *protocol.NetworkConfig, uint64, int)
	MarkTrafficDisconnected(uint, protocol.NetworkConfig, uint64, int)
	MarkTrafficReconnected(uint)
}

func (s *agentServer) enforceTrafficQuotas(ctx context.Context) {
	if !s.enforcing.CompareAndSwap(false, true) {
		return
	}
	defer s.enforcing.Store(false)
	for _, snapshot := range s.snapshotInstances() {
		if ctx.Err() != nil {
			return
		}
		s.enforceInstanceQuota(ctx, snapshot.ID)
	}
}
func (s *agentServer) enforceInstanceQuota(ctx context.Context, id uint) {
	if id == 0 {
		return
	}
	lease, ok := s.tryOperation(id)
	if !ok {
		return
	}
	defer lease.done()
	// Re-read after acquiring the lease: the snapshot may predate resize/delete.
	stored, e := s.storedInstance(id)
	if e != nil {
		return
	}
	if stored.Network.TrafficGB <= 0 && !isTrafficDisconnected(s, id) {
		return
	}
	d, ok := s.registry.Get(stored.Driver)
	if !ok {
		return
	}
	sampled := stored
	mctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	metrics, e := d.Metrics(mctx, &sampled)
	cancel()
	if e != nil {
		return
	}
	ts, hasStore := d.(trafficStore)
	var disconnected bool
	var original *protocol.NetworkConfig
	quota := stored.Network.TrafficGB
	if hasStore {
		var fileQuota int
		disconnected, original, _, fileQuota = ts.TrafficStateOf(id)
		if fileQuota > quota {
			quota = fileQuota
		}
		if quota <= 0 && original != nil && original.TrafficGB > 0 {
			quota = original.TrafficGB
		}
	}
	if !metrics.TrafficQuotaExceeded {
		if hasStore && disconnected {
			mode := strings.ToLower(strings.TrimSpace(stored.Network.Mode))
			if original != nil && mode == "none" {
				restored := *original
				if quota > restored.TrafficGB {
					restored.TrafficGB = quota
				}
				s.reconnectAfterQuota(ctx, d, ts, stored, restored)
			} else {
				ts.MarkTrafficReconnected(id)
			}
		}
		return
	}
	if strings.ToLower(strings.TrimSpace(stored.Network.Mode)) == "none" || (hasStore && disconnected) {
		return
	}
	s.disconnectOnQuota(ctx, d, ts, stored, metrics.TrafficUsedBytes, quota)
}
func isTrafficDisconnected(s *agentServer, id uint) bool {
	s.mu.RLock()
	stored, ok := s.instances[id]
	s.mu.RUnlock()
	return ok && strings.ToLower(strings.TrimSpace(stored.Network.Mode)) == "none"
}
func (s *agentServer) disconnectOnQuota(ctx context.Context, d driver.Driver, ts trafficStore, stored protocol.Instance, used uint64, quota int) {
	original := stored.Network
	disconnected := stored
	disconnected.Network.Mode = "none"
	disconnected.ObservedIP = ""
	dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if e := d.ConfigureNetwork(dctx, &disconnected); e != nil {
		log.Printf("实例 %d 流量超限断网失败，下周期重试: %v", stored.ID, e)
		return
	}
	if ts != nil {
		ts.MarkTrafficDisconnected(stored.ID, original, used, quota)
	}
	s.mu.Lock()
	s.instances[stored.ID] = disconnected
	s.mu.Unlock()
	log.Printf("实例 %d 流量超限，已断网不断电", stored.ID)
}
func (s *agentServer) reconnectAfterQuota(ctx context.Context, d driver.Driver, ts trafficStore, stored protocol.Instance, original protocol.NetworkConfig) {
	restored := stored
	restored.Network = original
	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if e := d.ConfigureNetwork(rctx, &restored); e != nil {
		log.Printf("实例 %d 流量配额已恢复，重连失败: %v", stored.ID, e)
		return
	}
	applyFirewallIfRunning(rctx, d, &restored)
	if ts != nil {
		ts.MarkTrafficReconnected(stored.ID)
	}
	s.mu.Lock()
	s.instances[stored.ID] = restored
	s.mu.Unlock()
	log.Printf("实例 %d 流量配额已恢复，网络已重连", stored.ID)
}
func (s *agentServer) startTrafficEnforcer(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	s.enforceTrafficQuotas(ctx)
	for {
		select {
		case <-ticker.C:
			s.enforceTrafficQuotas(ctx)
		case <-ctx.Done():
			return
		}
	}
}
