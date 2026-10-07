package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

var dedicatedResourcesMu sync.Mutex

type dedicatedRecord struct {
	ID         uint                   `json:"id"`
	Driver     string                 `json:"driver"`
	Attachment dedicatedAttachment    `json:"attachment"`
	Network    protocol.NetworkConfig `json:"network"`
}

func networkRecordPath(dir string, id uint) string {
	return filepath.Join(dir, "network", strconv.FormatUint(uint64(id), 10)+".json")
}

func loadDedicatedRecord(dir string, id uint) (*dedicatedRecord, error) {
	b, e := os.ReadFile(networkRecordPath(dir, id))
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var r dedicatedRecord
	if e = json.Unmarshal(b, &r); e != nil {
		return nil, e
	}
	if r.ID != id || !hostInterfaceName.MatchString(r.Attachment.Uplink) || (r.Attachment.Mode != "routed" && r.Attachment.Mode != "bridge") {
		return nil, fmt.Errorf("持久网络所有权记录损坏，拒绝清理")
	}
	return &r, nil
}

func saveDedicatedRecord(dir string, r dedicatedRecord) error {
	path := networkRecordPath(dir, r.ID)
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	entries, e := os.ReadDir(filepath.Dir(path))
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id, e := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if e != nil {
			return e
		}
		old, e := loadDedicatedRecord(dir, uint(id))
		if e != nil {
			return e
		}
		if old != nil && old.ID != r.ID && old.Attachment.IP == r.Attachment.IP {
			return fmt.Errorf("IPv4 %s 已由实例 %d 保留", r.Attachment.IP, old.ID)
		}
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".network-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}

func checkOwnedRoute(ctx context.Context, ip, bridge string) error {
	out, e := output(ctx, "ip", "-j", "route", "show", "exact", ip+"/32")
	if e != nil {
		return e
	}
	var routes []struct {
		Dev      string          `json:"dev"`
		Protocol json.RawMessage `json:"protocol"`
	}
	if e = json.Unmarshal(out, &routes); e != nil {
		return e
	}
	for _, r := range routes {
		if r.Dev != bridge || strings.Trim(string(r.Protocol), "\"") != ownedRouteProtocol {
			return fmt.Errorf("IPv4 %s 已存在非本实例的路由，拒绝覆盖", ip)
		}
	}
	return nil
}

func absentNetworkError(e error) bool {
	return e != nil && (contains(e.Error(), "Cannot find device") || contains(e.Error(), "does not exist") || contains(e.Error(), "No such process") || contains(e.Error(), "No such file or directory"))
}

func clearDedicatedProtection(ctx context.Context, id uint) error {
	if !commandAvailable(ctx, "iptables") || !commandAvailable(ctx, "iptables-restore") {
		return fmt.Errorf("无法验证防伪造规则清理：iptables 不可用")
	}
	chain := antiSpoofChain(id)
	firewallReplaceMu.Lock()
	defer firewallReplaceMu.Unlock()
	return restoreOwnedTable(ctx, "raw", "PREROUTING", []string{chain}, "-X "+chain+"\n")
}

func cleanupDedicated(ctx context.Context, dir string, id uint) error {
	dedicatedResourcesMu.Lock()
	defer dedicatedResourcesMu.Unlock()
	r, e := loadDedicatedRecord(dir, id)
	if e != nil || r == nil {
		return e
	}
	if r.Driver == "qemu" && r.Attachment.Mode == "routed" {
		bridge := ownedBridge(id)
		out, e := output(ctx, "ip", "-j", "link", "show", "dev", bridge)
		if e != nil && !absentNetworkError(e) {
			return e
		}
		var links []hostAddress
		if e == nil {
			if e = json.Unmarshal(out, &links); e != nil {
				return e
			}
		}
		if len(links) > 0 && (links[0].Alias != dedicatedOwner(id) || links[0].LinkInfo.Kind != "bridge") {
			return fmt.Errorf("清理被拒绝：接口 %s 不属于实例 %d", bridge, id)
		}
		// Exact dev + protocol deletion cannot remove a replacement foreign route.
		for _, args := range [][]string{{"route", "del", r.Attachment.IP + "/32", "dev", bridge, "proto", ownedRouteProtocol}, {"neigh", "del", "proxy", r.Attachment.IP, "dev", r.Attachment.Uplink}, {"link", "delete", "dev", bridge}} {
			if e = run(ctx, "ip", args...); e != nil && !absentNetworkError(e) {
				return fmt.Errorf("清理实例网络失败: %w", e)
			}
		}
	}
	// Incus owns its routed NIC, routes and proxy entries; its verified stop/delete
	// performs their teardown. Agent must not delete daemon-owned interfaces.
	if e = clearDedicatedProtection(ctx, id); e != nil {
		return e
	}
	return os.Remove(networkRecordPath(dir, id))
}
