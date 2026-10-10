package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func (s *agentServer) snapshotInstance(w http.ResponseWriter, r *http.Request, id uint) {
	var payload struct {
		Instance protocol.Instance `json:"instance"`
		Name     string            `json:"name"`
		Action   string            `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil || payload.Instance.ID != id || !driver.ValidSnapshotName(payload.Name) {
		writeError(w, 400, "invalid snapshot request")
		return
	}
	d, err := s.registry.Resolve(r.Context(), payload.Instance.Driver)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	recovery, ok := d.(driver.RecoveryDriver)
	if !ok {
		writeError(w, 400, "driver does not support recovery")
		return
	}
	var size int64
	switch payload.Action {
	case "create":
		size, err = recovery.CreateSnapshot(r.Context(), &payload.Instance, payload.Name)
	case "restore":
		err = recovery.RestoreSnapshot(r.Context(), &payload.Instance, payload.Name)
	case "delete":
		err = recovery.DeleteSnapshot(r.Context(), &payload.Instance, payload.Name)
	default:
		writeError(w, 400, "invalid snapshot action")
		return
	}
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	if payload.Action == "restore" {
		payload.Instance.Driver, payload.Instance.Status, payload.Instance.ObservedIP = d.Name(), driver.StatusStopped, ""
		s.storeRecovered(payload.Instance, false)
	}
	writeJSON(w, 200, map[string]any{"size_bytes": size})
}

func (s *agentServer) exportInstance(w http.ResponseWriter, r *http.Request, id uint) {
	inst, err := s.requestInstance(r, id)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	d, err := s.registry.Resolve(r.Context(), inst.Driver)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	recovery, ok := d.(driver.RecoveryDriver)
	if !ok {
		writeError(w, 400, "driver does not support export")
		return
	}
	dir, err := os.MkdirTemp(s.dataDir, "export-*")
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	name := "backup.tar"
	if d.Name() == "incus" {
		name = "backup.tar.gz"
	}
	path := filepath.Join(dir, name)
	if err := recovery.Export(r.Context(), &inst, path); err != nil {
		writeError(w, 502, err.Error())
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxImageSize {
		writeError(w, 400, "backup exceeds maximum size")
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

func (s *agentServer) importInstance(w http.ResponseWriter, r *http.Request, id uint) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImageSize+(1<<20))
	inst, file, name, _, _, cleanup, err := parseInstance(r)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || inst.ID != id || file == nil {
		writeError(w, 400, "invalid import request")
		return
	}
	d, err := s.registry.Resolve(r.Context(), inst.Driver)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	recovery, ok := d.(driver.RecoveryDriver)
	if !ok {
		writeError(w, 400, "driver does not support import")
		return
	}
	path, err := s.saveRecoveryUpload(file, name)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	defer os.Remove(path)
	inst.Driver, inst.RootPassword, inst.SSHReady = d.Name(), "", false
	validator, ok := d.(driver.ImportValidator)
	if !ok {
		writeError(w, 400, "driver does not support safe import validation")
		return
	}
	if err := validator.ValidateImport(r.Context(), &inst, path); err != nil {
		writeError(w, 400, "invalid recovery archive: "+err.Error())
		return
	}
	if r.URL.Query().Get("replace") == "true" {
		original := inst
		if stored, err := s.storedInstance(id); err == nil {
			original = stored
		}
		if original.Name != inst.Name || original.Driver != inst.Driver || original.Type != inst.Type {
			writeError(w, 409, "replace identity must match the original runtime")
			return
		}
		original.RootPassword = ""
		originalReady := s.bootReadyOf(id)
		if status, err := d.Status(r.Context(), &original); err != nil || status != driver.StatusStopped {
			writeError(w, 409, "stop instance before restoring backup")
			return
		}
		staged, ok := d.(driver.ReplacementImporter)
		if !ok {
			writeError(w, 400, "driver does not support non-destructive staged replacement")
			return
		}
		if err := staged.ReplaceImport(r.Context(), &original, &inst, path); err != nil {
			original.Status = driver.StatusStopped
			ready := originalReady
			var uncertain *driver.ReplacementError
			if errors.As(err, &uncertain) && uncertain.Uncertain {
				original.Status, ready = "error", false
			}
			s.storeRecovered(original, ready)
			// Driver errors can contain host paths; recovery details stay in the node log.
			log.Printf("instance %d staged replacement failed: %v", id, err)
			writeError(w, 502, "staged restore failed; original data retained on agent; operator recovery may be required")
			return
		}
	} else if err := importVerified(r.Context(), d, recovery, &inst, path); err != nil {
		writeError(w, 502, err.Error())
		return
	}
	inst.Status = driver.StatusStopped
	inst.RootPassword = ""
	if err := s.saveOwnedInstance(inst); err != nil {
		writeError(w, 500, "imported runtime retained; ownership persistence failed")
		return
	}
	s.storeRecovered(inst, false)
	writeJSON(w, 200, map[string]any{"instance": inst})
}

func importVerified(ctx context.Context, d driver.Driver, recovery driver.RecoveryDriver, inst *protocol.Instance, path string) error {
	if err := recovery.Import(ctx, inst, path); err != nil {
		return err
	}
	status, err := d.Status(ctx, inst)
	if err != nil {
		return fmt.Errorf("import stopped verification: %w", err)
	}
	if status != driver.StatusStopped {
		return fmt.Errorf("imported runtime is not stopped: %s", status)
	}
	return nil
}

func (s *agentServer) storeRecovered(inst protocol.Instance, ready bool) {
	inst.RootPassword, inst.SSHReady, inst.ObservedIP = "", ready, ""
	s.mu.Lock()
	s.instances[inst.ID], s.bootReady[inst.ID] = inst, ready
	s.mu.Unlock()
}

// Recovery archives are private data, not runnable disk images. Do not apply
// PrepareDiskFile's hypervisor permissions to uploads or retained rollback files.
func (s *agentServer) saveRecoveryUpload(src io.Reader, _ string) (path string, err error) {
	f, err := os.CreateTemp(s.dataDir, "recovery-upload-*")
	if err != nil {
		return "", err
	}
	path = f.Name()
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	n, copyErr := io.Copy(f, io.LimitReader(src, maxImageSize+1))
	closeErr := f.Close()
	if copyErr != nil {
		return path, copyErr
	}
	if closeErr != nil {
		return path, closeErr
	}
	if n == 0 || n > maxImageSize {
		return path, fmt.Errorf("invalid archive size")
	}
	return path, nil
}
