// Package egress owns the Phase 6 nftables integration: the dynamic
// `sandbox_sources_v4` set membership and the kernel-log → structured-
// log collector. CLAUDE.md "Egress policy (v1)" describes the
// surrounding model; roadmap/phase-6-hardening-and-egress.md §3 / §4
// / §7 specify the exact wiring.
//
// **Hard rule from roadmap §4**: if any nft call fails, the sandbox
// must not be left running with no egress rules attached. The caller
// (handleCreate / wake handler) checks the AddSource error and aborts
// the row to status='error' rather than continuing.
//
// **Hard rule from roadmap §"Risks" (boot-time reconciler)**: the
// sandbox_sources_v4 set is in-memory only. After a reboot, the
// reconciler MUST re-populate it from container_ip rows BEFORE
// sandboxd opens its HTTP listener. main.go enforces this ordering;
// see also the comment in cmd/sandboxd/main.go after reconcile.Once.
package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// Nft is the thin shell wrapper. The package doesn't try to BE
// libnftables — Phase 0 already commits to nft for everything else,
// and shell-out keeps the dependency surface tiny.
type Nft struct {
	Bin   string // "nft"
	Table string // "inet sandbox_platform"
	Set   string // "sandbox_sources_v4"
}

// New returns an Nft wired with the Phase 6 defaults.
func New() *Nft {
	return &Nft{
		Bin:   "nft",
		Table: "inet sandbox_platform",
		Set:   "sandbox_sources_v4",
	}
}

// ErrInvalidIP is returned when the caller passes a non-IPv4 string.
// IPv6 is deliberately out of scope (roadmap §"Out of scope").
var ErrInvalidIP = errors.New("egress: not an IPv4 address")

// AddSource adds an IP to sandbox_sources_v4. Idempotent — re-adding
// the same IP returns nil (nft is lenient about duplicates in the
// `flags interval` / `flags timeout` cases; for a plain set the
// duplicate returns an error that we filter).
func (n *Nft) AddSource(ctx context.Context, ip string) error {
	if !isV4(ip) {
		return fmt.Errorf("AddSource(%q): %w", ip, ErrInvalidIP)
	}
	out, err := run(ctx, n.Bin, "add", "element", "inet", "sandbox_platform", n.Set,
		"{ "+ip+" }")
	if err != nil {
		if isAlreadyExists(out) {
			return nil
		}
		return fmt.Errorf("nft add element %s: %w (%s)", n.Set, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveSource removes an IP from sandbox_sources_v4. Idempotent —
// removing an absent IP returns nil (the kernel's "No such file or
// directory" is filtered).
func (n *Nft) RemoveSource(ctx context.Context, ip string) error {
	if !isV4(ip) {
		return fmt.Errorf("RemoveSource(%q): %w", ip, ErrInvalidIP)
	}
	out, err := run(ctx, n.Bin, "delete", "element", "inet", "sandbox_platform", n.Set,
		"{ "+ip+" }")
	if err != nil {
		if isNotFound(out) {
			return nil
		}
		return fmt.Errorf("nft delete element %s: %w (%s)", n.Set, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ListSources reads sandbox_sources_v4 via `nft -j` and returns the
// IPs currently in the set. Used by main.go at boot to detect whether
// the reconciler-driven repopulation succeeded.
func (n *Nft) ListSources(ctx context.Context) ([]string, error) {
	out, err := run(ctx, n.Bin, "-j", "list", "set", "inet", "sandbox_platform", n.Set)
	if err != nil {
		return nil, fmt.Errorf("nft list set %s: %w (%s)", n.Set, err, strings.TrimSpace(string(out)))
	}
	type nftSetElem struct {
		Set struct {
			Elem []any `json:"elem"`
		} `json:"set"`
	}
	type nftJSON struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	var doc nftJSON
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("nft -j parse: %w", err)
	}
	var ips []string
	for _, obj := range doc.Nftables {
		raw, ok := obj["set"]
		if !ok {
			continue
		}
		var sv struct {
			Elem []any `json:"elem"`
		}
		if err := json.Unmarshal(raw, &sv); err != nil {
			continue
		}
		for _, e := range sv.Elem {
			switch v := e.(type) {
			case string:
				if isV4(v) {
					ips = append(ips, v)
				}
			}
		}
	}
	return ips, nil
}

// DropCounter is a single (reason, packets, bytes) sample lifted from
// `nft -j list table inet sandbox_platform`. The drops poller turns
// these into deltas + emits the EgressDrops counter.
type DropCounter struct {
	Reason  string
	Packets uint64
	Bytes   uint64
}

// readDropCounters lifts the counter on every rule in the
// sandbox_platform table whose comment maps to a known reason.
//
// nft -j output for a rule with a counter looks like:
//
//	{"rule": {"expr": [...], "comment": "block abuse list", ...}}
//
// where the expr contains a counter element:
//
//	{"counter": {"packets": 123, "bytes": 4567}}
//
// We walk every rule, extract (comment, counter) where present.
func (n *Nft) readDropCounters(ctx context.Context) ([]DropCounter, error) {
	out, err := run(ctx, n.Bin, "-j", "list", "table", "inet", "sandbox_platform")
	if err != nil {
		return nil, fmt.Errorf("nft -j list table: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("nft -j parse: %w", err)
	}
	var counters []DropCounter
	for _, obj := range doc.Nftables {
		ruleRaw, ok := obj["rule"]
		if !ok {
			continue
		}
		var rule struct {
			Comment string            `json:"comment"`
			Expr    []json.RawMessage `json:"expr"`
		}
		if err := json.Unmarshal(ruleRaw, &rule); err != nil {
			continue
		}
		reason := reasonFromComment(rule.Comment)
		if reason == "" {
			continue
		}
		for _, e := range rule.Expr {
			var step struct {
				Counter *struct {
					Packets uint64 `json:"packets"`
					Bytes   uint64 `json:"bytes"`
				} `json:"counter"`
			}
			if err := json.Unmarshal(e, &step); err != nil {
				continue
			}
			if step.Counter != nil {
				counters = append(counters, DropCounter{
					Reason:  reason,
					Packets: step.Counter.Packets,
					Bytes:   step.Counter.Bytes,
				})
				break
			}
		}
	}
	return counters, nil
}

// ReadDropCounters is the exported alias used by the metrics poller.
func (n *Nft) ReadDropCounters(ctx context.Context) ([]DropCounter, error) {
	return n.readDropCounters(ctx)
}

// reasonFromComment maps a rule comment to a Prometheus label value.
// Keep this table in sync with host/nftables/sandboxed.nft.
func reasonFromComment(c string) string {
	switch c {
	case "block cloud metadata":
		return "metadata"
	case "block outbound SMTP":
		return "smtp"
	case "block abuse list":
		return "abuse"
	case "block sandbox SSH except git hosts":
		return "ssh"
	case "block sandbox cross-host":
		return "cross_sandbox"
	}
	// All three RFC1918 rules share the rfc1918 reason.
	if strings.HasPrefix(c, "block RFC1918") {
		return "rfc1918"
	}
	return ""
}

// isV4 returns true iff s parses as an IPv4 address.
func isV4(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	return ip.To4() != nil
}

// isAlreadyExists filters the kernel's "File exists" response when we
// re-add an element that's already present.
func isAlreadyExists(out []byte) bool {
	low := bytes.ToLower(out)
	return bytes.Contains(low, []byte("file exists")) ||
		bytes.Contains(low, []byte("already in use"))
}

// isNotFound filters the kernel's "No such file or directory" response
// when we delete an element that's already gone.
func isNotFound(out []byte) bool {
	low := bytes.ToLower(out)
	return bytes.Contains(low, []byte("no such file or directory"))
}

// run is the single chokepoint for nft invocations. CombinedOutput so
// we capture both stdout (`-j` JSON output) and stderr (error text)
// uniformly — nft prints errors to stderr.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}
