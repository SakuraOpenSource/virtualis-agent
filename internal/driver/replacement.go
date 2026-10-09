package driver

import (
    "context"
    "crypto/rand"
    "encoding/binary"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "strconv"
    "time"
    "github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

// ReplacementImporter verifies a separate runtime before switching names; failures retain original storage.
type ReplacementImporter interface {
    ReplaceImport(context.Context, *protocol.Instance, *protocol.Instance, string) error
}

type ReplacementError struct {
    Err error
    Uncertain bool
}
func (e *ReplacementError) Error() string { return fmt.Sprintf("replacement requires recovery: %v", e.Err) }
func (e *ReplacementError) Unwrap() error { return e.Err }

func replacementStage(inst *protocol.Instance) (protocol.Instance, string, error) {
	var random [8]byte
	if _,err:=rand.Read(random[:]);err!=nil { return protocol.Instance{},"",err }
	id:=uint(binary.BigEndian.Uint64(random[:]) & 0x7fffffff)
	if id==0 || id==inst.ID { return replacementStage(inst) }
	nonce:=hex.EncodeToString(random[:])
	stage:=*inst
	stage.ID,stage.Name=id,"restore-"+nonce
	stage.RootPassword,stage.SSHReady="",false
	stage.Image=nil
	return stage,nonce,nil
}

func replacementPlan(dataDir, nonce, phase string, original, incoming *protocol.Instance) (string,error) {
	dir:=filepath.Join(dataDir,"retained-"+nonce)
	if err:=os.MkdirAll(dir,0700);err!=nil {return "",err}
	old,newInstance:=*original,*incoming
	old.RootPassword,newInstance.RootPassword="",""
	data,err:=json.Marshal(map[string]any{"phase":phase,"original":old,"incoming":newInstance})
	if err!=nil {return "",err}
	file,err:=os.CreateTemp(dir,"plan-*")
	if err!=nil {return "",err}
	path:=file.Name()
	defer os.Remove(path)
	if _,err=file.Write(data);err==nil {err=file.Sync()}
	closeErr:=file.Close()
	if err!=nil {return "",err};if closeErr!=nil {return "",closeErr}
	if err=os.Rename(path,filepath.Join(dir,"recovery.json"));err!=nil {return "",err}
	return dir,nil
}

func (d *QEMU) ReplaceImport(ctx context.Context, original, incoming *protocol.Instance, archive string) error {
	if err:=requireStopped(ctx,d,original);err!=nil {return err}
	oldDisk,err:=d.systemDisk(ctx,original)
	if err!=nil {return err}
	if err=ManagedImagePath(d.dataDir,oldDisk);err!=nil {return err}
	stage,nonce,err:=replacementStage(incoming)
	if err!=nil {return err}
	// Import verifies every disk and a separate stopped domain without touching the source.
	if err=d.Import(ctx,&stage,archive);err!=nil {return err}
	stageName,target:=resourceName(d.Name(),&stage),resourceName(d.Name(),original)
	retired:=*original
	retired.Name="retained-"+nonce
	retainedName:=resourceName(d.Name(),&retired)
	dir,err:=replacementPlan(d.dataDir,nonce,"staged",&retired,&stage)
	if err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	cleanup,cancel:=context.WithTimeout(context.WithoutCancel(ctx),2*time.Minute)
	defer cancel()
	if err=requireStopped(ctx,d,&stage);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=requireStopped(ctx,d,original);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=run(ctx,"virsh","domrename",target,retainedName);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=run(ctx,"virsh","domrename",stageName,target);err!=nil {
		rollback:=run(cleanup,"virsh","domrename",retainedName,target)
		return &ReplacementError{Err:fmt.Errorf("cutover failed: %w; name rollback: %v",err,rollback),Uncertain:true}
	}
	oldSnapshots:=filepath.Join(dir,"snapshots")
	if _,statErr:=os.Lstat(d.snapshotDir(original.ID));statErr==nil {
		if err=os.Rename(d.snapshotDir(original.ID),oldSnapshots);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	} else if !os.IsNotExist(statErr) {return &ReplacementError{Err:statErr,Uncertain:true}}
	if _,statErr:=os.Lstat(d.snapshotDir(stage.ID));statErr==nil {
		if err=os.Rename(d.snapshotDir(stage.ID),d.snapshotDir(original.ID));err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	} else if !os.IsNotExist(statErr) {return &ReplacementError{Err:statErr,Uncertain:true}}
	incoming.Image=stage.Image
	if err=requireStopped(ctx,d,incoming);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if _,err=replacementPlan(d.dataDir,nonce,"committed",&retired,incoming);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	// Only a verified published replacement authorizes retiring the old definition and its owned disk.
	if err=run(cleanup,"virsh","undefine",retainedName,"--nvram");err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=ManagedImagePath(d.dataDir,oldDisk);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=os.Remove(oldDisk);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=os.RemoveAll(dir);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	return nil
}

func (d *Incus) ReplaceImport(ctx context.Context, original, incoming *protocol.Instance, archive string) error {
	if err:=requireStopped(ctx,d,original);err!=nil {return err}
	stage,nonce,err:=replacementStage(incoming)
	if err!=nil {return err}
	if err=d.Import(ctx,&stage,archive);err!=nil {return err}
	stageName,target:=resourceName(d.Name(),&stage),resourceName(d.Name(),original)
	retired:=*original
	retired.Name="retained-"+nonce
	retainedName:=resourceName(d.Name(),&retired)
	dir,err:=replacementPlan(d.dataDir,nonce,"staged",&retired,&stage)
	if err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	oldProfile:="virtualis-p-"+strconv.FormatUint(uint64(original.ID),10)
	newProfile:="virtualis-p-"+strconv.FormatUint(uint64(stage.ID),10)
	retainedProfile:="virtualis-retained-p-"+nonce
	cleanup,cancel:=context.WithTimeout(context.WithoutCancel(ctx),2*time.Minute)
	defer cancel()
	if err=requireStopped(ctx,d,&stage);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=requireStopped(ctx,d,original);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=run(ctx,d.cli(),"move",target,retainedName);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=run(ctx,d.cli(),"move",stageName,target);err!=nil {
		rollback:=run(cleanup,d.cli(),"move",retainedName,target)
		return &ReplacementError{Err:fmt.Errorf("cutover failed: %w; name rollback: %v",err,rollback),Uncertain:true}
	}
	if err=run(ctx,d.cli(),"profile","rename",oldProfile,retainedProfile);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=run(ctx,d.cli(),"profile","rename",newProfile,oldProfile);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=requireStopped(ctx,d,incoming);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if _,err=replacementPlan(d.dataDir,nonce,"committed",&retired,incoming);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	// Delete the retained source only after import, policy restoration and stopped verification succeeded.
	if err=run(cleanup,d.cli(),"delete",retainedName);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=run(cleanup,d.cli(),"profile","delete",retainedProfile);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	if err=os.RemoveAll(dir);err!=nil {return &ReplacementError{Err:err,Uncertain:true}}
	return nil
}
