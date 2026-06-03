// Package idlock is a keyed mutex: one lock per sandbox id, created
// on demand. Phase 7 needs it so a workspace snapshot cannot read a
// loopback `.img` while a wake is starting the container and the
// container is writing to that same loopback.
//
// roadmap/phase-7-monitoring-snapshots-and-ops.md §9: "Acquire the
// per-sandbox mutex used by wake / destroy (avoid racing)." Phase 5's
// wake handler had its own wake-dedup map (a different concern —
// coalescing concurrent wakes into one outcome); this is the broader
// per-id critical-section lock that snapshot, restore, destroy, and
// wake all serialize on.
package idlock

import "sync"

// Registry hands out a *sync.Mutex per id. Locks are created lazily
// and never reclaimed — the id space is bounded by the sandbox count
// (target ≤ ~60 active, ≤ ~50 stopped), so the map stays tiny. A
// reclaim pass would add complexity for no measurable benefit.
type Registry struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New constructs an empty Registry.
func New() *Registry {
	return &Registry{locks: map[string]*sync.Mutex{}}
}

// get returns the mutex for id, creating it on first use.
func (r *Registry) get(id string) *sync.Mutex {
	r.mu.Lock()
	m, ok := r.locks[id]
	if !ok {
		m = &sync.Mutex{}
		r.locks[id] = m
	}
	r.mu.Unlock()
	return m
}

// Lock blocks until the per-id lock is held.
func (r *Registry) Lock(id string) { r.get(id).Lock() }

// Unlock releases the per-id lock. Panics (like sync.Mutex) if the
// lock was not held.
func (r *Registry) Unlock(id string) { r.get(id).Unlock() }

// TryLock acquires the per-id lock without blocking. Returns true if
// it was acquired. Used by the snapshotter goroutine, which skips a
// sandbox that is currently busy (waking / being destroyed) rather
// than queueing behind it.
func (r *Registry) TryLock(id string) bool { return r.get(id).TryLock() }
