package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestQEMUReplacementKeepsOriginalUntilStagedImportVerified(t *testing.T) {
	for _, failStage := range []bool{false,true} {
		t.Run(map[bool]string{false:"success",true:"stage_failure"}[failStage],func(t *testing.T){
			d:=NewQEMUWithDataDir(t.TempDir())
			replacer,ok:=any(d).(ReplacementImporter)
			if !ok { t.Fatal("QEMU lacks non-destructive staged replacement") }
			original:=&protocol.Instance{ID:1,Name:"old",Driver:"qemu",Type:"vm",Spec:protocol.InstanceSpec{DiskGB:20},Network:protocol.NetworkConfig{Mode:"none"}}
			incoming:=*original
			os.MkdirAll(d.imagesDir(),0755)
			disk:=filepath.Join(d.imagesDir(),resourceName("qemu",original)+".qcow2")
			os.WriteFile(disk,standaloneQCOW(),0600)
			defined:=false
			ctx:=qemuTestContext(t,func(_ context.Context,n string,a []string)([]byte,error){
				if n=="virsh" && a[0]=="list" { return []byte(resourceName("qemu",original)+"\n"),nil }
				if n=="virsh" && a[0]=="dumpxml" { return []byte(domainXML(resourceName("qemu",original),original,disk,"")),nil }
				if n=="virsh" && a[0]=="define" { defined=true; if failStage {return nil,errors.New("stage failed")} }
				if n=="virsh" && (a[0]=="domrename" || a[0]=="undefine") && strings.Contains(a[1],"virtualis-1-") {
					if !defined {t.Fatal("original touched before stage define")}
					if failStage {t.Fatal("stage failure touched original")}
				}
				if n=="virsh" && a[0]=="undefine" && strings.Contains(a[1],"retained-") { if _,e:=os.Stat(disk);e!=nil {t.Fatal("source data deleted before old domain retired")} }
				return nil,nil
			})
			err:=replacer.ReplaceImport(ctx,original,&incoming,qemuBackup(t,true))
			if failStage { if err==nil {t.Fatal("stage failure reported success")};if _,e:=os.Stat(disk);e!=nil {t.Fatal("source disk lost",e)} } else { if err!=nil {t.Fatal(err)};if incoming.Image==nil || incoming.Image.Path==disk {t.Fatal("new disk not published")} }
		})
	}
}

func TestIncusReplacementHasNonDestructiveCapability(t *testing.T) {
	if _,ok:=any(NewIncusWithDataDir(t.TempDir())).(ReplacementImporter);!ok { t.Fatal("Incus lacks non-destructive staged replacement") }
}
