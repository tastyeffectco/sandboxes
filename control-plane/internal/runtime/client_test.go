package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestClientStatus(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "sock")
	want := Status{
		Runtimed: RuntimedInfo{Version: "test", UptimeS: 5},
		Preview:  PreviewState{Status: PreviewReady, Pid: 42, LastHTTPStatus: 200, Restarts: 1},
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	got, err := NewClient(sock).Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Preview.Status != PreviewReady || got.Preview.Pid != 42 || got.Preview.Restarts != 1 {
		t.Fatalf("preview mismatch: %+v", got.Preview)
	}
	if got.Runtimed.Version != "test" {
		t.Fatalf("version: %q", got.Runtimed.Version)
	}
}

func TestClientStatusSocketAbsent(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "nonexistent"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("expected an error when the socket is absent")
	}
}
