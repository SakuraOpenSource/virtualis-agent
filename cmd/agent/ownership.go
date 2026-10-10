package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func (s *agentServer) ownershipPath(id uint) string {
	return filepath.Join(s.dataDir, "owned-instances", strconv.FormatUint(uint64(id), 10)+".json")
}

func (s *agentServer) ownedInstance(id uint) (protocol.Instance, error) {
	var inst protocol.Instance
	raw, err := os.ReadFile(s.ownershipPath(id))
	if err != nil {
		return inst, err
	}
	if err = json.Unmarshal(raw, &inst); err != nil {
		return inst, err
	}
	if inst.ID != id || inst.Name == "" || inst.Driver == "" {
		return inst, fmt.Errorf("invalid persisted instance identity")
	}
	return inst, nil
}

func (s *agentServer) saveOwnedInstance(inst protocol.Instance) error {
	// Ownership is created only by successful create/import, never status data from the wire.
	inst.RootPassword = ""
	if prior, err := s.ownedInstance(inst.ID); err == nil {
		if prior.Name != inst.Name || prior.Driver != inst.Driver {
			return fmt.Errorf("instance identity conflicts with persisted owner")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(s.ownershipPath(inst.ID))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "owner-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), s.ownershipPath(inst.ID))
}

func (s *agentServer) loadOwnedInstances() {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "owned-instances"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id, err := strconv.ParseUint(entry.Name()[:len(entry.Name())-5], 10, 64)
		if err != nil {
			continue
		}
		if inst, err := s.ownedInstance(uint(id)); err == nil {
			s.instances[inst.ID] = inst
		}
	}
}
