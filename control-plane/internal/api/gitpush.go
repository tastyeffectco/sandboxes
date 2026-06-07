// gitpush.go — auto-git-push. After each task finishes, sandboxd pushes
// the sandbox's app workspace to its assigned git remote, HOST-SIDE,
// with a master token injected inline. The sandbox / runtimed / agent
// never run git and never see the token.
//
// Dead-simple + reliable by construction:
//   - runs in the existing watchTask goroutine, after the result is durable;
//   - best-effort: a push failure NEVER affects the task result, and is
//     retried automatically on the next task;
//   - serialized by the per-sandbox idlock (no race with wake/snapshot/next task);
//   - git runs as the workspace owner uid, so .git ownership stays correct
//     for runtimed's next checkpoint (and git's safe.directory check passes);
//   - the token is composed into the push URL in-memory only — never written
//     to .git/config — and scrubbed from any logged output.
package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sandboxd/control-plane/internal/metrics"
)

const gitPushTimeout = 90 * time.Second

// pushOnTaskFinish pushes the sandbox's app workspace to its configured
// git remote. No-op (silent) when no remote is set or no token is on the
// host. Safe to call from any goroutine; recovers from panics.
func (s *Server) pushOnTaskFinish(sandboxID, taskID string) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Warn("git push: recovered from panic", "sandbox_id", sandboxID, "panic", rec)
		}
	}()

	if s.GitTokenPath == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitPushTimeout)
	defer cancel()
	log := s.Log.With("component", "gitpush", "sandbox_id", sandboxID, "task", taskID)

	remote, err := s.Store.GitRemote(ctx, sandboxID)
	if err != nil || remote == "" {
		return // not configured for this sandbox → feature off (not a failure)
	}
	// From here a push is EXPECTED for this sandbox, so every non-success
	// path is counted as a failure → it surfaces on the Grafana alert.
	if !strings.HasPrefix(remote, "https://") {
		log.Warn("git push: remote is not https", "remote", remote)
		metrics.GitPush.WithLabelValues("failed").Inc()
		return
	}

	tokenBytes, err := os.ReadFile(s.GitTokenPath)
	if err != nil {
		log.Warn("git push: master token missing while a remote is configured", "path", s.GitTokenPath)
		metrics.GitPush.WithLabelValues("failed").Inc()
		return
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		log.Warn("git push: master token file is empty while a remote is configured")
		metrics.GitPush.WithLabelValues("failed").Inc()
		return
	}

	appDir := s.appDirFor(sandboxID)
	fi, err := os.Stat(filepath.Join(appDir, ".git"))
	if err != nil {
		return // no repo yet (no checkpoint has run) → nothing to push (not a failure)
	}
	uid, gid := ownerIDs(fi)

	// Inline-credentialed URL: https://x-access-token:<TOKEN>@host/path.
	// Composed in memory for the push arg only — never persisted.
	authedURL := "https://x-access-token:" + token + "@" + strings.TrimPrefix(remote, "https://")

	s.Locks.Lock(sandboxID)
	defer s.Locks.Unlock(sandboxID)

	git := func(args ...string) (string, error) {
		full := append([]string{"-C", appDir}, args...)
		cmd := exec.CommandContext(ctx, "git", full...)
		// Run as the workspace owner so .git objects keep the right
		// ownership for runtimed's next checkpoint.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uid, Gid: gid, NoSetGroups: true},
		}
		cmd.Env = []string{
			"HOME=" + appDir,
			"PATH=/usr/bin:/bin:/usr/local/bin",
			"GIT_TERMINAL_PROMPT=0", // never block waiting for credentials
			"GIT_CONFIG_NOSYSTEM=1", // ignore host /etc/gitconfig
		}
		out, err := cmd.CombinedOutput()
		return scrub(string(out), token), err
	}

	// 1. stage everything (.gitignore already excludes node_modules/dist/.vite).
	if out, err := git("add", "-A"); err != nil {
		log.Warn("git push: add failed", "out", out)
		metrics.GitPush.WithLabelValues("failed").Inc()
		return
	}
	// 2. commit only if there are staged changes.
	if _, err := git("diff", "--cached", "--quiet"); err != nil {
		if out, cerr := git(
			"-c", "user.email=sandboxd@sandboxd.local",
			"-c", "user.name=sandboxd",
			"commit", "-m", "task "+taskID); cerr != nil {
			log.Warn("git push: commit failed", "out", out)
			// fall through — still attempt the push (a prior task's commit
			// may be unpushed).
		}
	}
	// 3. push to the assigned branch (no force). A divergent remote is
	//    logged and left for the next task / operator — never clobbered.
	if out, err := git("push", authedURL, "HEAD:refs/heads/main"); err != nil {
		log.Warn("git push: push failed", "remote", remote, "out", out)
		metrics.GitPush.WithLabelValues("failed").Inc()
		return
	}
	metrics.GitPush.WithLabelValues("ok").Inc()
	log.Info("git push: ok", "remote", remote)
}

// ownerIDs returns the uid/gid that own a FileInfo, falling back to
// 0 only if the platform stat is unavailable (never on Linux here).
func ownerIDs(fi os.FileInfo) (uint32, uint32) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid, st.Gid
	}
	return 0, 0
}

// scrub removes the token from text destined for logs.
func scrub(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}
