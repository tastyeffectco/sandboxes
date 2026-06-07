// Package traefik generates the Docker label set that Traefik's
// docker provider reads to attach a router + service to a sandbox
// container.
//
// Phase 3 freeze contract (ops/implementation/phase-3-report.md §6
// row "Per-host certs"): the label set ships PER-HOST certs, not a
// SAN-wildcard. The literal label set emitted here is byte-for-byte
// what Phase 3's sandbox-up shell helper emitted; Phase 4 inherits
// it wholesale so any downstream sandbox routed via the API matches
// the Phase-3-validated routing path exactly.
//
// TLS: routers enable TLS but carry NO per-router certresolver.
// Traefik serves every preview host from one shared wildcard
// certificate — *.preview.<domain> — held in the default TLS store
// (defaultGeneratedCert; see ops/traefik/dynamic/tls.yml). One cert
// covers all sandboxes, so no per-host ACME order is ever made and
// Let's Encrypt's 50-certs-per-registered-domain-per-week limit is
// never hit.
package traefik

import "fmt"

// Labels returns the slice of "key=value" label strings for a
// sandbox with the given ports. If ports is empty, Labels returns
// nil — the sandbox starts with no Traefik exposure (Phase 3
// `exposedByDefault: false` makes this safe).
//
// Each port produces one router and one service whose names are the
// same string `s-<id>-<port>`. The Host() rule covers the literal
// preview URL `s-<id>-<port>.preview.<domain>`.
//
// Phase 8 extension (roadmap §10): when visibility == "private", each
// router additionally references the `sandbox-preview-auth@file`
// forward-auth middleware (defined in ops/traefik/dynamic/auth.yml)
// so Traefik gates every request through sandboxd's /forward-auth.
// A "public" sandbox keeps the exact Phase 3/4/5 label set.
// entrypoint and tls are configurable for the OSS build: the default
// distribution serves plain HTTP on the `web` entrypoint (preview hosts
// like s-<id>-<port>.preview.localhost resolve to 127.0.0.1 in the
// browser, no certificates required). An operator deploying on a real
// domain sets PREVIEW_ENTRYPOINT=websecure and PREVIEW_TLS=true, supplies
// a wildcard cert to Traefik's default TLS store, and gets the original
// HTTPS behaviour with no per-host ACME.
func Labels(id string, ports []int, domain, visibility, entrypoint string, tls bool) []string {
	if len(ports) == 0 {
		return nil
	}
	if entrypoint == "" {
		entrypoint = "web"
	}
	// `sandboxd.managed=true` lets this distribution's Traefik scope its
	// docker provider with a constraint so it only ever routes sandboxes
	// it owns — important when sandboxd shares a Docker daemon with
	// other Traefik-labelled containers. See traefik/traefik.yml.
	out := []string{"traefik.enable=true", "sandboxd.managed=true"}
	for _, p := range ports {
		router := fmt.Sprintf("s-%s-%d", id, p)
		host := fmt.Sprintf("s-%s-%d.preview.%s", id, p, domain)
		out = append(out,
			fmt.Sprintf("traefik.http.routers.%s.rule=Host(`%s`)", router, host),
			fmt.Sprintf("traefik.http.routers.%s.entrypoints=%s", router, entrypoint),
			// Explicit router priority. The file-provider wake catch-all
			// is at priority 1, so any dynamic router at priority >=2
			// wins. We pick 100 for headroom against any future
			// implicit-priority changes.
			fmt.Sprintf("traefik.http.routers.%s.priority=100", router),
			fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port=%d", router, p),
		)
		if tls {
			// TLS on, but no per-router certresolver: Traefik serves the
			// shared *.preview.<domain> wildcard from the default TLS
			// store. One cert for every preview host — no per-host ACME.
			out = append(out, fmt.Sprintf("traefik.http.routers.%s.tls=true", router))
		}
		if visibility == "private" {
			out = append(out,
				fmt.Sprintf("traefik.http.routers.%s.middlewares=sandbox-preview-auth@file", router))
		}
	}
	return out
}
