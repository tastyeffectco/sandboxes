package egress

import (
	"context"
	"log/slog"
	"sync"
)

// SourceMap is the in-memory ip → sandbox_id table the egress
// collector uses to join kernel `SRC=...` lines back to a sandbox.
// SQLite is still source of truth (CLAUDE.md non-negotiable #6); this
// map is a hot-path lookup cache that the create / wake / stop flows
// keep coherent. Read at ~5000 lookups/sec on the journal tailer
// without doing a SQL query per lookup.
type SourceMap struct {
	mu sync.RWMutex
	m  map[string]string // ip -> id
}

// NewSourceMap constructs an empty map.
func NewSourceMap() *SourceMap {
	return &SourceMap{m: map[string]string{}}
}

// Set associates ip with id. Overwriting an existing entry is fine —
// docker can reassign bridge IPs after a daemon restart so the same
// id can appear under a new ip across host reboot.
func (s *SourceMap) Set(ip, id string) {
	if ip == "" || id == "" {
		return
	}
	s.mu.Lock()
	s.m[ip] = id
	s.mu.Unlock()
}

// Forget deletes the entry for ip. Safe on a missing key.
func (s *SourceMap) Forget(ip string) {
	if ip == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, ip)
	s.mu.Unlock()
}

// Lookup returns the sandbox_id for the given ip, or "" if no match.
func (s *SourceMap) Lookup(ip string) string {
	s.mu.RLock()
	id := s.m[ip]
	s.mu.RUnlock()
	return id
}

// Len returns the current map size. Exported for the metrics gauge.
func (s *SourceMap) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

// Manager bundles the Nft wrapper + SourceMap so callers reach for
// one type instead of two. It's the value plumbed into handleCreate,
// the wake handler, the reapers, and the reconciler.
type Manager struct {
	Nft     *Nft
	Sources *SourceMap
	Log     *slog.Logger
}

// NewManager constructs the bundle with Phase 6 defaults.
func NewManager(log *slog.Logger) *Manager {
	return &Manager{
		Nft:     New(),
		Sources: NewSourceMap(),
		Log:     log,
	}
}

// Add is the "create / wake succeeded" entry point: persist the
// ip→id binding in memory AND add to the kernel set. Order matters:
// add to nft FIRST so the kernel sees the policy before any packet
// reaches it. If nft fails, the caller aborts (does not update the
// in-memory map — leaves nothing inconsistent).
func (m *Manager) Add(ctx context.Context, id, ip string) error {
	if err := m.Nft.AddSource(ctx, ip); err != nil {
		return err
	}
	m.Sources.Set(ip, id)
	return nil
}

// Remove is the "stop / destroy" entry point: removes from kernel
// set AND in-memory map. We try nft first and fall through to the
// map removal even on nft errors, because a stopped container with
// an absent firewall rule is fine (no packets coming out anyway),
// but a stale in-memory entry would mis-route kernel-log lines.
func (m *Manager) Remove(ctx context.Context, id, ip string) error {
	err := m.Nft.RemoveSource(ctx, ip)
	m.Sources.Forget(ip)
	return err
}
