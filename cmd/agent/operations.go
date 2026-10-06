package main

import (
	"context"
	"sync"
)

// A lease spans the HTTP operation and any bootstrap continuation it starts.
// The map contains only active operations, so deleted IDs do not leak locks.
type operationLease struct {
	mu      sync.Mutex
	refs    int
	release func()
}
type leaseKey struct{}

func (s *agentServer) tryOperation(id uint) (*operationLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy == nil {
		s.busy = make(map[uint]bool)
	}
	if s.busy[id] {
		return nil, false
	}
	s.busy[id] = true
	return &operationLease{refs: 1, release: func() { s.mu.Lock(); delete(s.busy, id); s.mu.Unlock() }}, true
}
func (l *operationLease) retain() { l.mu.Lock(); l.refs++; l.mu.Unlock() }
func (l *operationLease) done() {
	l.mu.Lock()
	l.refs--
	last := l.refs == 0
	l.mu.Unlock()
	if last {
		l.release()
	}
}
func retainOperation(ctx context.Context) func() {
	if l, ok := ctx.Value(leaseKey{}).(*operationLease); ok {
		l.retain()
		return l.done
	}
	return func() {}
}
