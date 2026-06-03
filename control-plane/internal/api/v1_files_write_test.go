package api

import (
	"strings"
	"testing"
)

func TestResolveWritePath(t *testing.T) {
	const mnt = "/var/lib/sandboxed/workspaces/01XYZ.mnt"

	cases := []struct {
		name     string
		raw      string
		wantErr  string // substring; "" means must succeed
		wantFull string // ignored if wantErr != ""
		wantRel  string
	}{
		// happy paths
		{"basic file", "AGENTS.md", "", mnt + "/AGENTS.md", "AGENTS.md"},
		{"nested file", "workspace/app/AGENTS.md", "", mnt + "/workspace/app/AGENTS.md", "workspace/app/AGENTS.md"},
		{"dotfile under home", ".config/opencode/opencode.json", "", mnt + "/.config/opencode/opencode.json", ".config/opencode/opencode.json"},
		{"trims a leading ./", "./AGENTS.md", "", mnt + "/AGENTS.md", "AGENTS.md"},
		{"clean-able redundancy", "workspace/./app/AGENTS.md", "", mnt + "/workspace/app/AGENTS.md", "workspace/app/AGENTS.md"},

		// rejections — caller errors
		{"empty", "", "required", "", ""},
		{"NUL byte", "AGENTS\x00.md", "NUL", "", ""},
		{"absolute path", "/etc/passwd", "relative", "", ""},
		{"absolute under mount", mnt + "/AGENTS.md", "relative", "", ""},
		{"traversal segment", "../etc/passwd", "traversal", "", ""},
		{"traversal mid-path", "workspace/../../etc/passwd", "traversal", "", ""},
		{"root", ".", "must name a file", "", ""},
		{"slash only", "/", "relative", "", ""},
		{"trailing slash (dir)", "workspace/", "directory", "", ""},

		// reserved subtrees — platform-owned
		{"reserved .runtimed", ".runtimed/sock", "reserved", "", ""},
		{"reserved .runtimed exact", ".runtimed", "reserved", "", ""},
		{"reserved lost+found", "lost+found/something", "reserved", "", ""},

		// the .config dir itself is NOT reserved — agents need it
		{"under .config OK", ".config/claude/config.json", "", mnt + "/.config/claude/config.json", ".config/claude/config.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, rel, err := resolveWritePath(mnt, tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (full=%q)", tc.wantErr, full)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if full != tc.wantFull {
				t.Errorf("full = %q, want %q", full, tc.wantFull)
			}
			if rel != tc.wantRel {
				t.Errorf("rel = %q, want %q", rel, tc.wantRel)
			}
			// Invariant: the resolved path MUST be under the mount.
			if full != mnt && !strings.HasPrefix(full, mnt+"/") {
				t.Errorf("invariant violated: %q escapes %q", full, mnt)
			}
		})
	}
}

// Property check: no string passed as `raw` can resolve to a path that
// escapes the mount, no matter how mean.
func TestResolveWritePath_NeverEscapes(t *testing.T) {
	const mnt = "/var/lib/sandboxed/workspaces/01XYZ.mnt"
	probes := []string{
		"..", "../", "../..", "../../../etc/passwd",
		"workspace/../..",
		"workspace/../../../etc/passwd",
		"./..",
		"/..",
		"/etc/passwd",
		"workspace/AGENTS.md/..",
		"a/b/c/../../../..",
	}
	for _, p := range probes {
		full, _, err := resolveWritePath(mnt, p)
		if err == nil {
			if full != mnt && !strings.HasPrefix(full, mnt+"/") {
				t.Fatalf("escape via %q resolved to %q", p, full)
			}
			// resolved inside is fine; rejection is also fine
		}
	}
}
