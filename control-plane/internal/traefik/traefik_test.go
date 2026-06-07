package traefik

import (
	"reflect"
	"testing"
)

func TestLabels_NoPorts(t *testing.T) {
	if got := Labels("01HXANYZ", nil, "localhost", "public", "web", false); got != nil {
		t.Fatalf("want nil for empty ports, got %v", got)
	}
	if got := Labels("01HXANYZ", []int{}, "localhost", "private", "web", false); got != nil {
		t.Fatalf("want nil for empty ports, got %v", got)
	}
}

// OSS default: plain HTTP on the `web` entrypoint, no TLS label.
func TestLabels_SinglePort_HTTP(t *testing.T) {
	got := Labels("nx", []int{3000}, "localhost", "public", "web", false)
	want := []string{
		"traefik.enable=true",
		"sandboxd.managed=true",
		"traefik.http.routers.s-nx-3000.rule=Host(`s-nx-3000.preview.localhost`)",
		"traefik.http.routers.s-nx-3000.entrypoints=web",
		"traefik.http.routers.s-nx-3000.priority=100",
		"traefik.http.services.s-nx-3000.loadbalancer.server.port=3000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected label set\ngot:  %#v\nwant: %#v", got, want)
	}
}

// Production-style: websecure entrypoint + tls=true.
func TestLabels_SinglePort_TLS(t *testing.T) {
	got := Labels("nx", []int{3000}, "example.com", "public", "websecure", true)
	want := []string{
		"traefik.enable=true",
		"sandboxd.managed=true",
		"traefik.http.routers.s-nx-3000.rule=Host(`s-nx-3000.preview.example.com`)",
		"traefik.http.routers.s-nx-3000.entrypoints=websecure",
		"traefik.http.routers.s-nx-3000.priority=100",
		"traefik.http.services.s-nx-3000.loadbalancer.server.port=3000",
		"traefik.http.routers.s-nx-3000.tls=true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected label set\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestLabels_MultiPort(t *testing.T) {
	got := Labels("01HX", []int{3000, 3001}, "example.com", "public", "web", false)
	if got[0] != "traefik.enable=true" {
		t.Fatalf("first label must be enable; got %q", got[0])
	}
	// Two fixed lines (enable + managed), then 4 lines per port (rule,
	// entrypoints, priority, service) when TLS is off.
	if len(got) != 2+4*2 {
		t.Fatalf("want 10 labels for 2 ports (no TLS); got %d (%v)", len(got), got)
	}
	gotMap := map[string]bool{}
	for _, l := range got {
		gotMap[l] = true
	}
	for _, must := range []string{
		"traefik.http.routers.s-01HX-3000.rule=Host(`s-01HX-3000.preview.example.com`)",
		"traefik.http.routers.s-01HX-3001.rule=Host(`s-01HX-3001.preview.example.com`)",
		"traefik.http.services.s-01HX-3000.loadbalancer.server.port=3000",
		"traefik.http.services.s-01HX-3001.loadbalancer.server.port=3001",
	} {
		if !gotMap[must] {
			t.Errorf("missing expected label: %s", must)
		}
	}
}

// A private sandbox additionally references the sandbox-preview-auth@file
// forward-auth middleware on every router; a public sandbox must NOT.
func TestLabels_Private(t *testing.T) {
	priv := Labels("nx", []int{3000}, "localhost", "private", "web", false)
	wantMW := "traefik.http.routers.s-nx-3000.middlewares=sandbox-preview-auth@file"
	found := false
	for _, l := range priv {
		if l == wantMW {
			found = true
		}
	}
	if !found {
		t.Fatalf("private sandbox must carry the forward-auth middleware label; got %#v", priv)
	}

	pub := Labels("nx", []int{3000}, "localhost", "public", "web", false)
	for _, l := range pub {
		if l == wantMW {
			t.Fatalf("public sandbox must NOT carry the forward-auth middleware label; got %#v", pub)
		}
	}
}
