package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// ─── Post-task health pipeline ──────────────────────────────────────
//
// After the agent finishes and the build check runs, runtimed executes
// an ordered pipeline of health checks against the LIVE dev server.
// Each check covers one failure mode that the agent's own build — a
// fresh, separate `pnpm build` process — structurally cannot see,
// because the failure lives in the long-running dev-server process, not
// in the source. build_ok can be true while the preview is blank.
//
// Two kinds of check, run in this order:
//
//	REMEDIATION — detects a bad dev-server condition and FIXES it
//	              (e.g. restart the dev server when the agent edited a
//	              config file Vite/Tailwind cached in-process). Runs
//	              first so later probes observe the repaired state.
//
//	PROBE       — detects a condition it canNOT fix and REPORTS it as a
//	              preview error (e.g. the dev server 500s on the CSS/TS
//	              entry while the HTML shell still serves 200 — a blank
//	              page). Surfaced so preview status is `error` with a
//	              message, not a silent `ready`.
//
// ── To handle a NEW failure mode: append one postTaskCheck to the
//    `postTaskChecks` registry and implement its run func. Nothing in
//    runTask changes. Keep remediations before probes.
//
// The same building blocks (devGet, entryAssetProbes, extractDevError)
// back the continuous probe in main.go, so the live preview status and
// the per-task result agree.

// healthCtx is the input handed to every check.
type healthCtx struct {
	app          *app
	appDir       string
	filesChanged []string // task's git-diff paths, relative to appDir
}

// finding is a check's outcome.
type finding struct {
	note         string // logged regardless of outcome (may be empty)
	previewError string // if set, surfaced as the task's preview error
	remediated   bool   // a remediation acted (for logging)
}

type checkKind string

const (
	remediation checkKind = "remediation"
	probe       checkKind = "probe"
)

// postTaskCheck is one named health check.
type postTaskCheck struct {
	name string
	kind checkKind
	run  func(ctx context.Context, h *healthCtx) finding
}

// postTaskChecks is the ordered pipeline. Remediations precede probes
// so a probe observes the repaired dev server.
//
// ──────────────── ADD FUTURE FAILURE-MODE CHECKS HERE ───────────────
var postTaskChecks = []postTaskCheck{
	{name: "stale-config-restart", kind: remediation, run: checkStaleConfigRestart},
	{name: "entry-asset-compile", kind: probe, run: checkEntryAssetCompile},
}

// runPostTaskChecks executes the pipeline and returns the first preview
// error reported by any probe ("" if the app is healthy).
func (a *app) runPostTaskChecks(ctx context.Context, filesChanged []string) string {
	h := &healthCtx{app: a, appDir: a.appDir, filesChanged: filesChanged}
	var previewErr string
	for _, c := range postTaskChecks {
		f := c.run(ctx, h)
		attrs := []any{"check", c.name, "kind", string(c.kind)}
		if f.note != "" {
			attrs = append(attrs, "note", f.note)
		}
		if f.remediated {
			attrs = append(attrs, "remediated", true)
		}
		if f.previewError != "" {
			attrs = append(attrs, "preview_error", true)
			if previewErr == "" {
				previewErr = f.previewError
			}
		}
		a.log.Info("post-task health check", attrs...)
	}
	return previewErr
}

// ─── check: stale-config-restart (REMEDIATION) ──────────────────────
//
// Vite + Tailwind/PostCSS load build config once into the running
// dev-server process and do not reliably reload it. If the agent edited
// any such file, the live dev server serves STALE config — e.g. it 500s
// on a Tailwind utility (`font-body`) the agent just defined. A fresh
// `pnpm build` reads config from scratch and passes, so build_ok hides
// it. The fix is a process restart; the dev supervisor respawns on
// stop().
var devConfigFiles = []string{
	"tailwind.config.js", "tailwind.config.ts", "tailwind.config.cjs", "tailwind.config.mjs",
	"postcss.config.js", "postcss.config.cjs", "postcss.config.mjs",
	"vite.config.ts", "vite.config.js",
	"tsconfig.json", "tsconfig.app.json", "tsconfig.node.json",
	"src/index.css", "src/App.css",
}

func checkStaleConfigRestart(ctx context.Context, h *healthCtx) finding {
	hits := matchConfigChanges(h.filesChanged, devConfigFiles)
	if len(hits) == 0 {
		return finding{}
	}
	h.app.dev.stop() // supervisor respawns with fresh config
	ready := h.app.waitDevReady(ctx, 25*time.Second)
	return finding{
		note:       fmt.Sprintf("dev config changed (%s); restarted dev server [ready=%v]", strings.Join(hits, ","), ready),
		remediated: true,
	}
}

// ─── check: entry-asset-compile (PROBE) ─────────────────────────────
//
// The dev server serves index.html (GET / → 200) even when it cannot
// transform the real entry modules — the user gets a blank page. The
// dev server returns 500 with an error body when a module fails to
// compile. We report it; we do not try to fix it (it may be a genuine
// code error). 4xx is ignored — a missing variant entry (.jsx vs .tsx)
// is not a failure.
var entryAssetProbes = []string{"/src/index.css", "/src/main.tsx"}

func checkEntryAssetCompile(ctx context.Context, h *healthCtx) finding {
	for _, p := range entryAssetProbes {
		code, body := h.app.devGet(ctx, p)
		if code < 500 {
			continue
		}
		return finding{
			note:         fmt.Sprintf("%s → HTTP %d", p, code),
			previewError: fmt.Sprintf("%s: %s", strings.TrimPrefix(p, "/"), extractDevError(body)),
		}
	}
	return finding{}
}

// ─── shared helpers ─────────────────────────────────────────────────

// devGet performs a GET against the dev server's loopback port and
// returns the status code (0 = no answer) and a capped body.
func (a *app) devGet(ctx context.Context, path string) (int, string) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", a.previewPort, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, ""
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	return resp.StatusCode, string(body)
}

// waitDevReady polls GET / until the dev server answers 200 or the
// budget expires; returns whether it became ready.
func (a *app) waitDevReady(ctx context.Context, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if code, _ := a.devGet(ctx, "/"); code == 200 {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// probeEntryAssets returns the first entry-asset compile error, or "".
// Used by the continuous probe (main.go) so live preview status reflects
// dev-server transform failures, not just the HTML shell's 200.
func (a *app) probeEntryAssets(ctx context.Context) string {
	for _, p := range entryAssetProbes {
		if code, body := a.devGet(ctx, p); code >= 500 {
			return extractDevError(body)
		}
	}
	return ""
}

// matchConfigChanges returns changed paths that are watched config
// files, matching on the exact relative path or the basename.
func matchConfigChanges(changed, watched []string) []string {
	want := make(map[string]bool, len(watched))
	for _, w := range watched {
		want[w] = true
	}
	var hits []string
	for _, f := range changed {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if want[f] || want[filepath.Base(f)] {
			hits = append(hits, f)
		}
	}
	return hits
}

// extractDevError pulls a human message out of a Vite 500 body. Vite
// reports transform errors two ways depending on the failing stage:
//
//	CSS/PostCSS — the body is itself the JSON error {"message": "..."}
//	              (this is the font-body / stale-Tailwind case).
//	JS/TS       — an HTML error page embedding `const error = {...}`.
//
// Handle both, then fall back to the first meaningful (non-HTML) line.
// Always capped.
func extractDevError(body string) string {
	if msg := jsonMessage(body); msg != "" {
		return capError(msg)
	}
	if i := strings.Index(body, "const error = "); i >= 0 {
		if msg := jsonMessage(body[i+len("const error = "):]); msg != "" {
			return capError(msg)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "<") {
			continue // skip HTML scaffolding
		}
		return capError(s)
	}
	return "dev server returned an error with no message"
}

// jsonMessage decodes the first JSON object at the start of s and
// returns its "message" field, or "" if s does not begin with one.
// Trailing data after the object (e.g. `;\n</script>`) is tolerated.
func jsonMessage(s string) string {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(s)))
	var j struct {
		Message string `json:"message"`
	}
	if err := dec.Decode(&j); err == nil {
		return j.Message
	}
	return ""
}

func capError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}
