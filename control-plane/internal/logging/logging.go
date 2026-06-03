// Package logging configures the slog JSON logger used everywhere
// in sandboxd, and ships an HTTP middleware that decorates every
// request with a request-id derived from a ULID.
//
// systemd captures stderr into journald (see systemd/sandboxd.service).
// No file rotation; journald owns retention.
package logging

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewLogger returns a JSON slog.Logger writing to stderr. If
// SANDBOXD_DEBUG is set, the level is debug; otherwise info.
func NewLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("SANDBOXD_DEBUG") != "" {
		level = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}

type ctxKey int

const requestIDKey ctxKey = 1

// RequestID returns the request id stored in ctx by Middleware,
// or "" if none.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// Middleware adds an X-Request-Id header to every response and
// threads a request-id into the context. Used by the API handlers
// when emitting log lines.
func Middleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r.WithContext(ctx))
		log.Info("http",
			"request_id", rid,
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// newRequestID generates a ULID using crypto/rand so multiple goroutines
// don't share a single entropy source (and so we don't panic on the
// nil source the original Phase 4 draft passed — see
// ops/implementation/phase-4-report.md Issue #5).
func newRequestID() string {
	t := time.Now()
	id, err := ulid.New(ulid.Timestamp(t), rand.Reader)
	if err != nil {
		// Fallback — extremely unlikely. Return a timestamp-only id.
		return time.Now().UTC().Format("20060102T150405.000000000Z07:00")
	}
	return id.String()
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped writer's flusher, so this logging
// wrapper does not hide http.Flusher from streaming handlers (SSE
// task events). Without it Go buffers the whole response.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
