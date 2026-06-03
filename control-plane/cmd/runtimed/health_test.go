package main

import (
	"strings"
	"testing"
)

func TestMatchConfigChanges(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		want    []string
	}{
		{"tailwind config edit", []string{"src/pages/Home.tsx", "tailwind.config.js"}, []string{"tailwind.config.js"}},
		{"index.css edit (the font-body bug)", []string{"src/index.css"}, []string{"src/index.css"}},
		{"vite + postcss", []string{"vite.config.ts", "postcss.config.js"}, []string{"vite.config.ts", "postcss.config.js"}},
		{"no config touched", []string{"src/App.tsx", "src/pages/Home.tsx", "package.json"}, nil},
		{"empty and whitespace ignored", []string{"", "  ", "tsconfig.json"}, []string{"tsconfig.json"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchConfigChanges(c.changed, devConfigFiles)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestExtractDevError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"vite json body (the real font-body CSS 500)",
			`{"message":"[postcss] The ` + "`font-body`" + ` class does not exist.","plugin":"vite:css"}`,
			"[postcss] The `font-body` class does not exist.",
		},
		{
			"vite html error page (JS/TS transform 500)",
			"\n  <!DOCTYPE html>\n  <html><head><title>Error</title>\n  <script type=\"module\">\n  const error = {\"message\":\"main.tsx: Unexpected token (23:14)\",\"stack\":\"at x\"};\n  </script></head></html>",
			"main.tsx: Unexpected token (23:14)",
		},
		{"plain text first line", "Error: Cannot find module './x'\n  at foo\n  at bar", "Error: Cannot find module './x'"},
		{"empty body", "", "dev server returned an error with no message"},
		{"html-only no message falls through", "<!DOCTYPE html>\n<html></html>", "dev server returned an error with no message"},
		{"leading blank lines", "\n\n  real message here", "real message here"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractDevError(c.body); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestExtractDevErrorCaps(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got := extractDevError(`{"message":"` + long + `"}`)
	if len([]rune(got)) > 401 { // 400 + the ellipsis rune
		t.Fatalf("not capped: len=%d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got tail %q", got[len(got)-6:])
	}
}

// The registry must keep remediations before probes so a probe observes
// the repaired dev server.
func TestPipelineOrderRemediationsFirst(t *testing.T) {
	seenProbe := false
	for _, c := range postTaskChecks {
		if c.kind == probe {
			seenProbe = true
		}
		if c.kind == remediation && seenProbe {
			t.Fatalf("remediation %q is ordered after a probe; remediations must run first", c.name)
		}
	}
}
