package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const buildCheckTimeout = 120 * time.Second

func runGit(appDir string, args ...string) (string, error) {
	full := append([]string{"-C", appDir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func gitCommit(appDir, msg string) error {
	_, err := runGit(appDir,
		"-c", "user.email=runtimed@sandbox.local", "-c", "user.name=runtimed",
		"commit", "--allow-empty", "-q", "-m", msg)
	return err
}

// ensureRepo makes the app dir a git repo on first use, with one
// baseline commit. node_modules / dist / .vite are excluded by the
// app's committed .gitignore.
func ensureRepo(appDir string) error {
	if _, err := os.Stat(filepath.Join(appDir, ".git")); err == nil {
		return nil
	}
	if _, err := runGit(appDir, "init", "-q"); err != nil {
		return err
	}
	if _, err := runGit(appDir, "add", "-A"); err != nil {
		return err
	}
	return gitCommit(appDir, "runtimed: golden snapshot baseline")
}

// checkpoint commits the current workspace state before a task and
// returns the commit SHA — the pre-task revert seam (checkpoint_id).
func checkpoint(appDir, taskID string) (string, error) {
	if err := ensureRepo(appDir); err != nil {
		return "", err
	}
	if _, err := runGit(appDir, "add", "-A"); err != nil {
		return "", err
	}
	if err := gitCommit(appDir, "runtimed: checkpoint before task "+taskID); err != nil {
		return "", err
	}
	return runGit(appDir, "rev-parse", "HEAD")
}

// filesChanged lists workspace paths the task changed, relative to the
// app dir, by diffing the working tree against the checkpoint commit.
// It is the authoritative files_changed source (provider-agnostic).
func filesChanged(appDir, checkpointID string) ([]string, error) {
	if checkpointID == "" {
		return nil, nil
	}
	if _, err := runGit(appDir, "add", "-A"); err != nil {
		return nil, err
	}
	out, err := runGit(appDir, "diff", "--cached", "--name-only", checkpointID)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(out, "\n"), nil
}

// buildCheck runs the project build and reports whether the app
// compiles — this is what makes a task's build_ok honest.
func buildCheck(appDir string, log *slog.Logger) (ok bool, errMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), buildCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", "pnpm build")
	cmd.Dir = appDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, ""
	}
	log.Warn("post-task build check failed", "err", err.Error())
	msg := strings.TrimSpace(string(out))
	if len(msg) > 2000 {
		msg = "...(truncated)...\n" + msg[len(msg)-2000:]
	}
	return false, msg
}
