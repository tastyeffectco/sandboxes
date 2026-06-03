// Package runtime defines the internal control protocol between
// sandboxd (the host control plane) and runtimed (the in-sandbox
// supervisor). See ops/design/v1-external-api.md §4 and the runtimed
// contract.
//
// Transport is HTTP/1.1 over a Unix domain socket on the workspace
// loopback (DefaultSocketPath): the socket inode is reachable both
// from inside the container and, via the same loopback mount, from
// the host at <workspaces>/<id>.mnt/.runtimed/sock. There is no
// network port and no cross-tenant reachability.
//
// This package is imported by both sides: runtimed serves these
// types; the sandboxd-side Client (client.go) consumes them.
package runtime

import "time"

// DefaultSocketPath is where runtimed binds its control socket inside
// the container. It is under the durable workspace so the host sees
// the same inode at <workspaces>/<id>.mnt/.runtimed/sock.
const DefaultSocketPath = "/home/sandbox/.runtimed/sock"

// PreviewStatus is the dev-server / app runtime state. It is reported,
// never commanded (ops/design/v1-external-api.md §4.3).
type PreviewStatus string

const (
	// PreviewDown — no dev-server process is running.
	PreviewDown PreviewStatus = "down"
	// PreviewStarting — the process is up but not yet serving HTTP 200.
	PreviewStarting PreviewStatus = "starting"
	// PreviewReady — the dev server is serving HTTP 200.
	PreviewReady PreviewStatus = "ready"
	// PreviewError — the app fails to compile. Set by the post-task
	// build check; not produced until the task subsystem lands.
	PreviewError PreviewStatus = "error"
)

// Status is the GET /status response — the whole runtimed snapshot.
type Status struct {
	Runtimed RuntimedInfo `json:"runtimed"`
	Preview  PreviewState `json:"preview"`
	// ActiveTask is the running task, or null when idle.
	ActiveTask *ActiveTask `json:"active_task"`
}

// RuntimedInfo identifies the supervisor and how long it has been up.
type RuntimedInfo struct {
	Version  string    `json:"version"`
	BootedAt time.Time `json:"booted_at"`
	UptimeS  int64     `json:"uptime_s"`
}

// PreviewState is the reported dev-server / app runtime state.
type PreviewState struct {
	Status            PreviewStatus `json:"status"`
	Pid               int           `json:"pid,omitempty"`
	LastHTTPStatus    int           `json:"last_http_status,omitempty"`
	LastCheckedAt     *time.Time    `json:"last_checked_at,omitempty"`
	BuildErrorMessage string        `json:"build_error_message,omitempty"`
	// Restarts counts how many times the supervisor has restarted the
	// dev server since runtimed booted.
	Restarts int `json:"restarts"`
}
