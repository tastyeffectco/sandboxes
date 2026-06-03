// Package audit is the Phase 8 audit-log writer: a thin wrapper the
// API handlers, the wake handler, and the auth middleware call to
// record one append-only `audit_log` row per privileged action
// (roadmap/phase-8 §12). Query access in v1 is direct `sqlite3` from
// the host; there is no read API.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// Store is the slice of the SQLite store the writer needs. Declared as
// an interface so internal/audit depends only on internal/store's
// method, not the other way around. *store.Store satisfies it.
type Store interface {
	InsertAudit(ctx context.Context, at int64, actorKind, actorName, actorIP, externalUserID, action, target, detail string) error
}

// Logger persists audit rows. It is safe for concurrent use.
type Logger struct {
	store Store
	log   *slog.Logger

	mu   sync.Mutex
	last map[string]time.Time // sample-key -> last write, for WriteSampled
}

// New constructs a Logger. A nil store yields a no-op logger (nil-safe
// for tests / partial wiring).
func New(store Store, log *slog.Logger) *Logger {
	return &Logger{store: store, log: log, last: map[string]time.Time{}}
}

// Entry is one privileged-action audit record. Detail is JSON-encoded
// into the `detail` column.
type Entry struct {
	ActorKind      string
	ActorName      string
	ActorIP        string
	ExternalUserID string
	Action         string
	Target         string
	Detail         map[string]any
}

// Write persists one audit row, best-effort: a store failure is logged
// but never propagates to the caller's request. It uses a fresh
// background context with a short timeout so the row still lands even
// if the request context was already cancelled (client disconnect).
func (l *Logger) Write(_ context.Context, e Entry) {
	if l == nil || l.store == nil {
		return
	}
	detail := ""
	if len(e.Detail) > 0 {
		if b, err := json.Marshal(e.Detail); err == nil {
			detail = string(b)
		}
	}
	kind := e.ActorKind
	if kind == "" {
		kind = "system"
	}
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.store.InsertAudit(wctx, time.Now().Unix(),
		kind, e.ActorName, e.ActorIP, e.ExternalUserID,
		e.Action, e.Target, detail); err != nil && l.log != nil {
		l.log.Warn("audit: write failed", "action", e.Action, "err", err.Error())
	}
}

// WriteSampled is Write rate-limited to at most one row per minute per
// sampleKey. Used for `preview.access_allowed`, which would otherwise
// be one row per request (roadmap §12: "sampled 1/min per (sub,
// sandbox)").
func (l *Logger) WriteSampled(ctx context.Context, sampleKey string, e Entry) {
	if l == nil || l.store == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	if t, ok := l.last[sampleKey]; ok && now.Sub(t) < time.Minute {
		l.mu.Unlock()
		return
	}
	l.last[sampleKey] = now
	l.mu.Unlock()
	l.Write(ctx, e)
}

// TokenInvalid satisfies auth.AuditWriter — the auth middleware calls
// it on a failed bearer-token check (roadmap §12: the only token-
// related action written is the failure).
func (l *Logger) TokenInvalid(ctx context.Context, ip string) {
	l.Write(ctx, Entry{ActorKind: "unknown", ActorIP: ip, Action: "auth.token_invalid"})
}
