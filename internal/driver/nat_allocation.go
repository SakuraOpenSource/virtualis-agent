package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

var natAllocationMu sync.Mutex

type natAllocation struct {
	ID uint `json:"id"`
	Name string `json:"name"`
	Driver string `json:"driver"`
	Bridge string `json:"bridge"`
	IP string `json:"ip"`
}

func bridgeIPv4(ctx context.Context, bridge string) (net.IP, *net.IPNet, error) {
	raw,err:=output(ctx,"ip","-4","-json","addr","show","dev",bridge)
	if err!=nil {return nil,nil,fmt.Errorf("cannot inspect NAT bridge: %w",err)}
	var interfaces []struct{ Addresses []struct{Family string `json:"family"`;Local string `json:"local"`;Prefix int `json:"prefixlen"`} `json:"addr_info"` }
	if err=json.Unmarshal(raw,&interfaces);err!=nil {return nil,nil,err}
	for _,iface:=range interfaces {for _,addr:=range iface.Addresses {
		if addr.Family!="inet" {continue}
		ip,subnet,e:=net.ParseCIDR(addr.Local+"/"+strconv.Itoa(addr.Prefix))
		if e==nil && ip.To4()!=nil {return ip,subnet,nil}
	}}
	return nil,nil,fmt.Errorf("NAT bridge has no IPv4 subnet")
}

func loadNATAllocations(dataDir string) (map[string]natAllocation,error) {
	records:=map[string]natAllocation{}
	raw,err:=os.ReadFile(filepath.Join(dataDir,"nat-allocations.json"))
	if os.IsNotExist(err) {return records,nil}
	if err!=nil {return nil,err}
	if err=json.Unmarshal(raw,&records);err!=nil {return nil,fmt.Errorf("NAT allocation table is corrupt: %w",err)}
	seen:=map[string]bool{}
	for key,r:=range records {
		identity:=r.Bridge+"/"+r.IP
		if r.ID==0 || key!=r.Driver+"/"+strconv.FormatUint(uint64(r.ID),10) || net.ParseIP(r.IP)==nil || seen[identity] {return nil,fmt.Errorf("NAT allocation table contains conflicting identities")}
		seen[identity]=true
	}
	return records,nil
}

func saveNATAllocations(dataDir string, records map[string]natAllocation) error {
	if dataDir=="" {return fmt.Errorf("NAT allocation requires persistent agent storage")}
	if err:=os.MkdirAll(dataDir,0755);err!=nil {return err}
	raw,err:=json.Marshal(records);if err!=nil {return err}
	f,err:=os.CreateTemp(dataDir,"nat-allocations-*");if err!=nil {return err}
	defer os.Remove(f.Name())
	if _,err=f.Write(raw);err==nil {err=f.Sync()}
	closeErr:=f.Close();if err!=nil {return err};if closeErr!=nil {return closeErr}
	return os.Rename(f.Name(),filepath.Join(dataDir,"nat-allocations.json"))
}

func allocateNATIP(ctx context.Context, dataDir, driver, bridge string, inst *protocol.Instance) (string,error) {
	gateway,subnet,err:=bridgeIPv4(ctx,bridge);if err!=nil {return "",err}
	natAllocationMu.Lock();defer natAllocationMu.Unlock()
	records,err:=loadNATAllocations(dataDir);if err!=nil {return "",err}
	key:=driver+"/"+strconv.FormatUint(uint64(inst.ID),10)
	if current,ok:=records[key];ok {
		if current.Name!=inst.Name || current.Bridge!=bridge || !subnet.Contains(net.ParseIP(current.IP)) {return "",fmt.Errorf("NAT identity conflicts with persisted allocation")}
		return current.IP,nil
	}
	used:=map[string]bool{gateway.String():true}
	for _,r:=range records {if r.Bridge==bridge {used[r.IP]=true}}
	// Existing declared leases are claimed rather than silently overwriting another instance's address.
	requested:=strings.Split(inst.Network.IPv4,"/")[0]
	base:=gateway.To4().Mask(subnet.Mask)
	candidate:=""
	if requested!="" {
		ip:=net.ParseIP(requested)
		if ip==nil || !subnet.Contains(ip) || used[ip.String()] {return "",fmt.Errorf("requested NAT address conflicts with an existing allocation")}
		candidate=ip.String()
	} else {
		for slot:=100;slot<240;slot++ {
			ip:=net.IPv4(base[0],base[1],base[2],byte(slot))
			if subnet.Contains(ip) && !ip.Equal(subnet.IP) && !used[ip.String()] {candidate=ip.String();break}
		}
	}
	if candidate=="" {return "",fmt.Errorf("NAT address slots exhausted (140-slot managed range)")}
	records[key]=natAllocation{ID:inst.ID,Name:inst.Name,Driver:driver,Bridge:bridge,IP:candidate}
	if err=saveNATAllocations(dataDir,records);err!=nil {return "",err}
	return candidate,nil
}

func releaseNATAllocation(dataDir, driver string, id uint) error {
	natAllocationMu.Lock();defer natAllocationMu.Unlock()
	records,err:=loadNATAllocations(dataDir);if err!=nil {return err}
	key:=driver+"/"+strconv.FormatUint(uint64(id),10)
	if _,exists:=records[key];!exists {return nil}
	delete(records,key)
	return saveNATAllocations(dataDir,records)
}
