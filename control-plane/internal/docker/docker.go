// Package docker is a thin, typed wrapper around the `docker` CLI.
//
// CLAUDE.md control-plane scope: "Shells out to `docker` CLI via
// os/exec. Replace with the Docker SDK only when shell-out is a
// measured bottleneck." Phase 4 keeps this rule.
//
// Design rule: NO MAGIC DEFAULTS inside this package. Callers pass
// the full RunSpec so a change to the locked flag set is obvious in
// code review. The package only encodes the CLI invocation, not the
// policy choices documented in CLAUDE.md.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrNotFound is returned when `docker inspect` reports the target
// container does not exist.
var ErrNotFound = errors.New("container not found")

// Client is the entry point for the wrapper.
type Client struct {
	// Bin is the docker binary path. Defaults to "docker" via NewClient.
	Bin string
}

func NewClient() *Client { return &Client{Bin: "docker"} }

// RunSpec carries every flag the Phase 2/3 sandbox-up script emitted.
// No defaults; callers fill all of these explicitly.
type RunSpec struct {
	Name        string   // --name
	Hostname    string   // --hostname
	Network     string   // --network (OSS: shared sandboxd_net so Traefik can route)
	Userns      string   // --userns (OSS: "host" for deterministic workspace ownership)
	ReadOnly    bool     // --read-only
	CapDrop     []string // --cap-drop=ALL (passed once per entry)
	SecurityOpt []string // --security-opt=no-new-privileges
	CPUShares   int      // --cpu-shares=100
	Memory      string   // --memory=10g
	MemorySwap  string   // --memory-swap=10g
	PidsLimit   int      // --pids-limit=1024
	Ulimits     []string // --ulimit nofile=65536:65536
	Tmpfs       []string // --tmpfs /tmp:size=512m   (one --tmpfs flag per entry)
	Env         []string // --env KEY=VAL   (one --env flag per entry)
	Volumes     []string // -v <host>:<container>[:opts]
	Labels      []string // --label key=value
	Image       string   // last positional
	Cmd         []string // optional CMD override
}

// Run starts the container detached and returns its 12-char short ID.
func (c *Client) Run(ctx context.Context, spec RunSpec) (string, error) {
	args := []string{"run", "-d"}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if spec.Hostname != "" {
		args = append(args, "--hostname", spec.Hostname)
	}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
	}
	if spec.Userns != "" {
		args = append(args, "--userns", spec.Userns)
	}
	if spec.ReadOnly {
		args = append(args, "--read-only")
	}
	for _, v := range spec.CapDrop {
		args = append(args, "--cap-drop="+v)
	}
	for _, v := range spec.SecurityOpt {
		args = append(args, "--security-opt="+v)
	}
	if spec.CPUShares > 0 {
		args = append(args, fmt.Sprintf("--cpu-shares=%d", spec.CPUShares))
	}
	if spec.Memory != "" {
		args = append(args, "--memory="+spec.Memory)
	}
	if spec.MemorySwap != "" {
		args = append(args, "--memory-swap="+spec.MemorySwap)
	}
	if spec.PidsLimit > 0 {
		args = append(args, fmt.Sprintf("--pids-limit=%d", spec.PidsLimit))
	}
	for _, v := range spec.Ulimits {
		args = append(args, "--ulimit", v)
	}
	for _, v := range spec.Tmpfs {
		args = append(args, "--tmpfs", v)
	}
	for _, v := range spec.Env {
		args = append(args, "--env", v)
	}
	for _, v := range spec.Volumes {
		args = append(args, "-v", v)
	}
	for _, v := range spec.Labels {
		args = append(args, "--label", v)
	}
	if spec.Image == "" {
		return "", errors.New("docker.Run: spec.Image is required")
	}
	args = append(args, spec.Image)
	args = append(args, spec.Cmd...)
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	full := strings.TrimSpace(string(out))
	if len(full) >= 12 {
		return full[:12], nil
	}
	return full, nil
}

// ContainerJSON is the slice of `docker inspect` output we care
// about. Fields can be added as needed; unknown fields are ignored
// by encoding/json.
type ContainerJSON struct {
	ID    string `json:"Id"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
		Pid     int    `json:"Pid"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	// NetworkSettings is the subset Phase 5's wake-readiness probe
	// needs: the container's bridge IP on the default network. We
	// deliberately surface only what we use; adding fields later is
	// non-breaking because callers reach in by name.
	NetworkSettings struct {
		IPAddress string                          `json:"IPAddress"`
		Networks  map[string]*NetworkEndpointInfo `json:"Networks"`
	} `json:"NetworkSettings"`
}

// NetworkEndpointInfo carries the per-network endpoint info; the
// wake-readiness probe walks Networks["bridge"].IPAddress when the
// top-level IPAddress is empty (which it can be when a non-default
// network is the only attachment).
type NetworkEndpointInfo struct {
	IPAddress string `json:"IPAddress"`
}

// BridgeIP returns the container's bridge IP, preferring the
// top-level IPAddress and falling back to the explicit "bridge"
// entry under Networks. Empty string when the container has no
// bridge attachment yet (race against docker start).
func (cj *ContainerJSON) BridgeIP() string {
	if cj.NetworkSettings.IPAddress != "" {
		return cj.NetworkSettings.IPAddress
	}
	// Prefer the legacy default bridge if present, then fall back to the
	// first network that has an IP. In the OSS build sandboxes join a
	// user-defined network (sandboxd_net), so the "bridge" key is absent
	// and we must scan whatever network the container actually got.
	if n, ok := cj.NetworkSettings.Networks["bridge"]; ok && n != nil && n.IPAddress != "" {
		return n.IPAddress
	}
	for _, n := range cj.NetworkSettings.Networks {
		if n != nil && n.IPAddress != "" {
			return n.IPAddress
		}
	}
	return ""
}

// Inspect returns the parsed JSON for a container, or ErrNotFound.
// The "not found" detection uses a CASE-INSENSITIVE substring match
// because docker's exact wording varies by version and locale:
//
//	"Error response from daemon: No such object: s-..."   (older / some)
//	"Error: no such object: s-..."                         (current)
//	"Error response from daemon: no such container: s-..." (legacy)
//
// The original Phase 4 draft used case-sensitive matches and missed
// the lowercase 'no such object' variant Docker emitted on this host,
// causing the reconciler's V6 path to mis-handle missing containers.
// Issue #7 in ops/implementation/phase-4-report.md.
func (c *Client) Inspect(ctx context.Context, name string) (*ContainerJSON, error) {
	out, err := c.run(ctx, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if isNotFoundStderr(exitErr.Stderr) {
				return nil, ErrNotFound
			}
		}
		return nil, err
	}
	var cj ContainerJSON
	if err := json.Unmarshal(bytes.TrimSpace(out), &cj); err != nil {
		return nil, fmt.Errorf("inspect: parse json: %w", err)
	}
	return &cj, nil
}

// Remove forces removal. Idempotent — returns nil on "no such container".
func (c *Client) Remove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "rm", "-f", name)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if isNotFoundStderr(exitErr.Stderr) {
			return nil
		}
	}
	return err
}

// isNotFoundStderr matches docker's varying "not found" wordings
// case-insensitively. See Inspect doc-comment for the catalogue.
func isNotFoundStderr(stderr []byte) bool {
	low := strings.ToLower(string(stderr))
	return strings.Contains(low, "no such object") ||
		strings.Contains(low, "no such container")
}

// Start runs `docker start <name>`. Idempotent — a no-op when the
// container is already running. Required by Phase 5's wake path.
func (c *Client) Start(ctx context.Context, name string) error {
	_, err := c.run(ctx, "start", name)
	return err
}

// Stop runs `docker stop --time=<sec> <name>`. The kernel still kills
// the container after the grace period, so a runaway process can't
// pin the call open indefinitely. Required by Phase 5's idle and
// pressure reapers.
//
// Idempotent: docker stop on an already-stopped container is a no-op
// (exit 0). On a missing container, returns ErrNotFound so callers
// can distinguish from a transient daemon error.
func (c *Client) Stop(ctx context.Context, name string, timeoutSec int) error {
	if timeoutSec < 0 {
		timeoutSec = 0
	}
	_, err := c.run(ctx, "stop", "--time", fmt.Sprintf("%d", timeoutSec), name)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if isNotFoundStderr(exitErr.Stderr) {
			return ErrNotFound
		}
	}
	return err
}

// ExecResult carries the stdout/stderr/exit-code of a non-interactive
// `docker exec` call. See API exec semantics in §7 of the roadmap.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Exec runs a non-interactive command inside the named container.
// v1 deliberately has no PTY / TTY / stdin support.
func (c *Client) Exec(ctx context.Context, name string, cmd []string) (ExecResult, error) {
	args := append([]string{"exec", name}, cmd...)
	cmdEx := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmdEx.Stdout = &stdout
	cmdEx.Stderr = &stderr
	err := cmdEx.Run()
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}
	if err == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}

// ListByNamePrefix returns the names of all containers (any state)
// whose name starts with the given prefix. Used by the reconciler to
// find orphans.
func (c *Client) ListByNamePrefix(ctx context.Context, prefix string) ([]string, error) {
	out, err := c.run(ctx, "ps", "-a", "--filter", "name=^"+prefix, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// Info runs `docker info -f {{.ServerVersion}}` for the readyz probe.
func (c *Client) Info(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// run is the single chokepoint for invoking the docker CLI.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitErr.Stderr = stderr.Bytes()
			return stdout.Bytes(), exitErr
		}
		return stdout.Bytes(), fmt.Errorf("docker %s: %w (%s)", args[0], err, stderr.String())
	}
	return stdout.Bytes(), nil
}
