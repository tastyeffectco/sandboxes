package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

// serve binds the control Unix domain socket and serves the runtimed
// RPC surface until ctx is cancelled.
func serve(ctx context.Context, socketPath string, a *app) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return err
	}
	// Clear a stale socket left by a previous boot before binding.
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, a.status())
	})
	mux.HandleFunc("POST /tasks", a.handleStartTask)
	mux.HandleFunc("GET /tasks/{id}/events", a.handleTaskEvents)
	mux.HandleFunc("POST /tasks/{id}/cancel", a.handleCancelTask)

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	a.log.Info("runtimed control socket listening", "socket", socketPath)
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		err = nil
	}
	_ = os.Remove(socketPath)
	return err
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
