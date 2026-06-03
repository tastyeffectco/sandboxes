// Package activity owns the signals that feed the idle reaper: the
// Traefik access-log tailer (HTTP requests), the open-connection
// poller (WebSocket / SSE lifetime, when a Traefik metric for it
// exists), and the in-flight `docker exec` counter.
//
// CLAUDE.md "Activity definition" lists four ways a sandbox is active:
// an open WS/SSE connection, an open exec session, a recent HTTP
// request, or an explicit keepalive_until flag. Phase 5 maps those to:
//   - WS/SSE          → open-connection poller (or wider grace window
//                       if no Traefik metric is verifiable; see
//                       poller.go).
//   - exec session    → InflightExec counter below.
//   - HTTP request    → access-log tailer.
//   - keepalive_until → SQLite column + idle-reaper skip rule.
package activity

import (
	"sync"

	"github.com/sandboxed/control-plane/internal/metrics"
)

// InflightExec is a tiny thread-safe counter map: how many exec calls
// are currently mid-flight per sandbox id. The idle reaper treats any
// non-zero entry here as "active" regardless of timestamps. The
// pressure reaper does NOT consult this in the < 5% emergency branch
// (per CLAUDE.md: "stop heaviest-RSS sandbox even if active").
type InflightExec struct {
	mu sync.Mutex
	n  map[string]int
}

// NewInflightExec constructs an empty counter.
func NewInflightExec() *InflightExec {
	return &InflightExec{n: map[string]int{}}
}

// Enter increments the counter for id.
func (i *InflightExec) Enter(id string) {
	i.mu.Lock()
	i.n[id]++
	i.mu.Unlock()
	metrics.InflightExec.Inc()
}

// Exit decrements the counter for id. Idempotent in the sense that
// reaching zero deletes the map entry to keep the map small.
func (i *InflightExec) Exit(id string) {
	i.mu.Lock()
	if i.n[id] > 0 {
		i.n[id]--
		if i.n[id] == 0 {
			delete(i.n, id)
		}
	}
	i.mu.Unlock()
	metrics.InflightExec.Dec()
}

// Active returns true if the sandbox has any in-flight exec call.
func (i *InflightExec) Active(id string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.n[id] > 0
}
