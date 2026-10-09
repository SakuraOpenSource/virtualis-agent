package driver

import (
	"context"
	"os"
	"strings"
	"fmt"
	"path/filepath"
	"encoding/json"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestNATExhaustionReturnsExplicitErrorWithoutReservationMutation(t *testing.T) {
    d:=NewQEMUWithDataDir(t.TempDir())
    allocations:=map[string]natAllocation{}
    for id:=uint(1);id<=140;id++ {allocations[fmt.Sprintf("qemu/%d",id)]=natAllocation{ID:id,Name:fmt.Sprint(id),Driver:"qemu",Bridge:"virbr0",IP:fmt.Sprintf("192.168.122.%d",99+id)}}
    raw,_:=json.Marshal(allocations)
    if err:=os.WriteFile(filepath.Join(d.dataDir,"nat-allocations.json"),raw,0600);err!=nil {t.Fatal(err)}
    reconciler,ok:=any(d).(interface{EnsureNATIdentity(context.Context,*protocol.Instance)error})
    if !ok {t.Fatal("NAT allocator must return explicit errors instead of swallowing exhaustion")}
    mutated:=false
    ctx:=WithCommandRunner(context.Background(),func(_ context.Context,n string,a ...string)([]byte,error){
        if n=="ip" {return []byte(`[{"addr_info":[{"family":"inet","local":"192.168.122.1","prefixlen":24}]}]`),nil}
        if n=="virsh" && a[0]=="domiflist" {return []byte("Interface Type Source Model MAC\n----\nvnet0 network default virtio 52:54:00:01:00:00\n"),nil}
        if n=="virsh" && a[0]=="net-update" {mutated=true}
        return []byte(""),nil
    })
    err:=reconciler.EnsureNATIdentity(ctx,&protocol.Instance{ID:1000,Name:"overflow",Network:protocol.NetworkConfig{Mode:"nat"}})
    if err==nil || !strings.Contains(err.Error(),"exhausted") || mutated {t.Fatalf("exhaustion not fail-closed: %v mutation=%v",err,mutated)}
}

func TestNATAllocationPersistsDistinctSlotsForModuloCollisions(t *testing.T) {
	dir:=t.TempDir()
	ctx:=WithCommandRunner(context.Background(),func(_ context.Context,n string,a ...string)([]byte,error){
		if n=="ip" {return []byte(`[{"addr_info":[{"family":"inet","local":"192.168.122.1","prefixlen":24}]}]`),nil}
		if n=="virsh" && a[0]=="net-dumpxml" {return []byte(`<network><ip address='192.168.122.1' netmask='255.255.255.0'><dhcp/></ip></network>`),nil}
		if n=="virsh" && a[0]=="dominfo" {return []byte("Id: -\n"),nil}
		if n=="virsh" && a[0]=="domiflist" {return []byte("Interface Type Source Model MAC\n--------------------------------\nvnet0 network default virtio 52:54:00:01:00:00\n"),nil}
		return []byte(""),nil
	})
	first,second:=&protocol.Instance{ID:1,Name:"first",Network:protocol.NetworkConfig{Mode:"nat"}},&protocol.Instance{ID:141,Name:"second",Network:protocol.NetworkConfig{Mode:"nat"}}
	d:=NewQEMUWithDataDir(dir)
	d.EnsureNATIdentity(ctx,first)
	d.EnsureNATIdentity(ctx,second)
	if first.Network.IPv4=="" || second.Network.IPv4=="" || first.Network.IPv4==second.Network.IPv4 {t.Fatalf("colliding NAT slots: %q %q",first.Network.IPv4,second.Network.IPv4)}
	before:=second.Network.IPv4
	restarted:=NewQEMUWithDataDir(dir)
	second.Network.IPv4=""
	restarted.EnsureNATIdentity(ctx,second)
	if second.Network.IPv4!=before {t.Fatalf("slot changed on restart: %s -> %s",before,second.Network.IPv4)}
	entries,err:=os.ReadDir(dir)
	if err!=nil || len(entries)==0 {t.Fatal("allocation table was not persisted",err)}

}
