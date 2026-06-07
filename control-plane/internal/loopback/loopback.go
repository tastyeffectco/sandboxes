// Package loopback manages per-sandbox workspace storage.
//
// HISTORY / OSS NOTE: historically this package
// created an 8 GB sparse ext4 loopback file per sandbox (truncate +
// mkfs.ext4 + losetup mount) so the kernel enforced a hard per-workspace
// quota. That requires privileged host access (loop devices, mount,
// userns-remap subuid math) which is hostile to a portable "runs fully
// on Docker, one-click install" distribution.
//
// The OSS "sandboxd" build replaces the loopback with a plain
// bind-mounted directory per sandbox under a single data root. The
// trade-off is explicit and documented in the README: NO hard
// per-workspace disk quota (the host filesystem is shared). Everything
// else — workspace persistence across container destroy, host reboot
// survival, seeding from the image skeleton, template/snapshot clones —
// is preserved, and the public method surface is unchanged so the rest
// of the control plane is untouched.
//
// The directory is bind-mounted into the sandbox container at
// /home/sandbox. Because the control plane shells out to the host Docker
// daemon (docker.sock), the data root MUST be visible at the SAME
// absolute path both inside the control-plane container and on the host
// (the compose file bind-mounts it host:container symmetric). The path
// the control plane writes is therefore also a valid host path for the
// sibling `docker run -v <path>:/home/sandbox`.
package loopback

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Manager owns the workspace data root and the seeding image.
type Manager struct {
	Root      string // data root, e.g. /var/lib/sandboxed/workspaces
	SeedImage string // base image used for the one-shot seed container
	DockerBin string // "docker"; injectable for tests
	Userns    string // --userns for the seed container ("host" by default)
}

// New constructs a Manager. The data root and seed image are normally
// overridden from the environment by the caller (sandboxd main).
func New() *Manager {
	return &Manager{
		Root:      "/var/lib/sandboxed/workspaces",
		SeedImage: "sandboxd-base:1.0.0",
		DockerBin: "docker",
		Userns:    "host",
	}
}

// Paths returns the workspace path for a sandbox id. With directory
// storage there is no separate image file, so img and mnt are the same
// directory — callers that historically read `img` as "the thing on
// disk to delete/back up" and `mnt` as "the thing to bind-mount" both
// resolve to the workspace directory.
func (m *Manager) Paths(id string) (img, mnt string) {
	dir := filepath.Join(m.Root, id)
	return dir, dir
}

// Provision is idempotent. It creates the workspace directory if absent
// and seeds it from the image's /opt/sandbox-skel ONLY when the
// directory is empty. Re-provisioning an existing, populated workspace
// (id-reuse, or the reconciler's boot pass) is a no-op.
func (m *Manager) Provision(ctx context.Context, id string) error {
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", m.Root, err)
	}
	dir, _ := m.Paths(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace %s: %w", dir, err)
	}

	empty, err := isEmptyDir(dir)
	if err != nil {
		return fmt.Errorf("check empty: %w", err)
	}
	if !empty {
		return nil
	}

	// One-shot seed container. Runs as root so it can chown the seeded
	// tree to the unprivileged `sandbox` user the runtime container runs
	// as. No userns-remap in the OSS default, so host uid == container
	// uid on the bind mount and the control plane (root) can always
	// read/write the result.
	seed := []string{
		"run", "--rm",
		"-v", dir + ":/target",
		"--user", "0",
	}
	if m.Userns != "" {
		// On a userns-remap daemon the seed's root would otherwise map to
		// a high host uid that can't write the host-root-owned workspace
		// dir. --userns=host keeps root==host-root (no-op on a normal
		// daemon) so the copy + chown to the sandbox user is deterministic.
		seed = append(seed, "--userns", m.Userns)
	}
	seed = append(seed,
		m.SeedImage,
		"bash", "-lc",
		`cp -aT /opt/sandbox-skel/. /target/ && chown -R sandbox:sandbox /target && chmod 755 /target`,
	)
	if err := runCmd(ctx, m.DockerBin, seed...); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	return nil
}

// ProvisionFromTemplate clones a prebuilt template/snapshot directory
// into the workspace instead of seeding from the image skeleton. Used by
// the snapshots-as-templates and fast-cold-start paths. The template is
// already populated and pre-owned for the sandbox user, so no seeding is
// performed. Idempotent: an existing workspace is preserved untouched.
func (m *Manager) ProvisionFromTemplate(ctx context.Context, id, templatePath string) error {
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", m.Root, err)
	}
	dir, _ := m.Paths(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace %s: %w", dir, err)
	}

	empty, err := isEmptyDir(dir)
	if err != nil {
		return fmt.Errorf("check empty: %w", err)
	}
	if !empty {
		return nil
	}

	if _, terr := os.Stat(templatePath); terr != nil {
		return fmt.Errorf("template unavailable (%s): %w", templatePath, terr)
	}
	// cp -a preserves ownership/perms; trailing /. copies directory
	// contents into the (existing) workspace dir.
	if err := runCmd(ctx, "cp", "-a", strings.TrimRight(templatePath, "/")+"/.", dir+"/"); err != nil {
		return fmt.Errorf("clone template %s: %w", templatePath, err)
	}
	return nil
}

// Release is a no-op for directory storage — there is no loopback to
// unmount. Kept to satisfy the method surface callers expect.
func (m *Manager) Release(_ context.Context, _ string) error { return nil }

// ImgExists reports whether a populated workspace directory exists for
// this id. The reconciler uses it to decide whether an id-reuse POST is
// safe and whether to re-provision on boot.
func (m *Manager) ImgExists(id string) bool {
	dir, _ := m.Paths(id)
	empty, err := isEmptyDir(dir)
	if err != nil {
		return false // does not exist (or unreadable)
	}
	return !empty
}

// ListMounts returns nil — directory storage has no kernel mounts to
// reconcile. Kept so the reconciler's orphan-mount sweep compiles and
// always reports zero orphan mounts.
func (m *Manager) ListMounts() ([]string, error) { return nil, nil }

// ---- internals ------------------------------------------------------

// isEmptyDir reports whether dir is empty. A non-existent directory is
// reported as an error (so callers can distinguish "absent").
func isEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		// lost+found can linger from a previously-loopback workspace;
		// ignore it so the seed gate behaves the same.
		if e.Name() == "lost+found" {
			continue
		}
		return false, nil
	}
	return true, nil
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
