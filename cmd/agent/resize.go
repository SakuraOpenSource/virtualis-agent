package main

import (
	"encoding/json"
	"github.com/SakuraOpenSource/virtualis-agent/internal/driver"
	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
	"net/http"
)

func (s *agentServer) resizeInstance(w http.ResponseWriter, r *http.Request, id uint) {
	var p struct {
		Instance protocol.Instance       `json:"instance"`
		Spec     protocol.InstanceSpec   `json:"spec"`
		Network  *protocol.NetworkConfig `json:"network"`
	}
	if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); e != nil || p.Instance.ID != id || p.Instance.Name == "" {
		writeError(w, 400, "invalid resize request")
		return
	}
	inst := p.Instance
	inst.RootPassword = ""
	if e := driver.ValidateResize(&inst, p.Spec, p.Network); e != nil {
		writeError(w, 400, e.Error())
		return
	}
	d, e := s.registry.Resolve(r.Context(), inst.Driver)
	if e != nil {
		writeError(w, 400, e.Error())
		return
	}
	resize, ok := d.(driver.ResizeDriver)
	if !ok {
		writeError(w, 400, "driver does not support resize")
		return
	}
	status, e := d.Status(r.Context(), &inst)
	if e != nil {
		writeError(w, 502, e.Error())
		return
	}
	if status != driver.StatusStopped {
		writeError(w, 409, "stop instance before resizing")
		return
	}
	if e = resize.Resize(r.Context(), &inst, p.Spec, p.Network); e != nil {
		writeError(w, 502, e.Error())
		return
	}
	inst.Driver, inst.Status, inst.RootPassword = d.Name(), driver.StatusStopped, ""
	s.applyBootReady(&inst)
	s.mu.Lock()
	s.instances[id] = inst
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"instance": inst})
}
