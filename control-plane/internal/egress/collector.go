package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sandboxd/control-plane/internal/metrics"
)

// Collector tails journalctl for kernel messages with prefix
// `sandbox-egress `, joins the SRC field against the SourceMap, and
// writes a structured JSON line to the per-day egress log.
//
// roadmap/phase-6-hardening-and-egress.md §7 Stage B. nftables already
// rate-limits the kernel log at 5000/s; the collector is purely
// downstream — if journalctl backs up, the lag shows in
// `sandboxd_egress_log_lag_seconds` and journald's own ring buffer
// caps the damage.
type Collector struct {
	Manager  *Manager
	LogDir   string // /var/log/sandboxed/egress
	Log      *slog.Logger

	// JournalctlArgs lets tests inject a fake binary; production
	// path uses the default ("journalctl", "-k", "-f", "-o", "json",
	// "--grep=sandbox-egress").
	JournalctlBin  string
	JournalctlArgs []string

	mu        sync.Mutex
	lastWrite time.Time
}

// kernelMsgRE pulls the fields we care about out of the iptables /
// nftables kernel log line. Reference shape from the roadmap §7:
//   sandbox-egress IN=docker0 OUT=ens3 MAC=... SRC=172.17.0.5
//   DST=140.82.121.4 LEN=60 ... PROTO=TCP SPT=44782 DPT=443 ... SYN
//
// We pull SRC, DST, DPT.
var (
	srcRE = regexp.MustCompile(`\bSRC=([0-9.]+)`)
	dstRE = regexp.MustCompile(`\bDST=([0-9.]+)`)
	dptRE = regexp.MustCompile(`\bDPT=([0-9]+)`)
)

// Run blocks until ctx is cancelled. Restarts journalctl with a 5 s
// backoff if it exits unexpectedly.
func (c *Collector) Run(ctx context.Context) error {
	if c.JournalctlBin == "" {
		c.JournalctlBin = "journalctl"
	}
	if len(c.JournalctlArgs) == 0 {
		c.JournalctlArgs = []string{
			"-k", "-f", "-o", "json", "--grep=sandbox-egress",
		}
	}
	if c.LogDir == "" {
		c.LogDir = "/var/log/sandboxed/egress"
	}
	if err := os.MkdirAll(c.LogDir, 0o750); err != nil {
		c.Log.Warn("egress: mkdir log dir failed", "err", err.Error())
	}
	for {
		if err := c.runOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			c.Log.Warn("egress: journal tail errored; retrying in 5s", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *Collector) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.JournalctlBin, c.JournalctlArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start journalctl: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if err := c.consume(ctx, stdout); err != nil {
		return err
	}
	return nil
}

// journalEntry is the slim subset of `journalctl -o json` we read.
// REALTIME timestamps are microseconds since epoch (systemd convention).
type journalEntry struct {
	Message            string `json:"MESSAGE"`
	RealtimeTimestamp  string `json:"__REALTIME_TIMESTAMP"`
}

// egressLine is the JSON shape we write to disk. roadmap §7 Stage B.
type egressLine struct {
	TS         int64  `json:"ts"`
	SandboxID  string `json:"sandbox_id"`
	SrcIP      string `json:"src_ip"`
	DstIP      string `json:"dst_ip"`
	DstPort    int    `json:"dst_port"`
	Event      string `json:"event"`
}

func (c *Collector) consume(ctx context.Context, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		c.handle(sc.Bytes())
	}
	return sc.Err()
}

func (c *Collector) handle(line []byte) {
	var je journalEntry
	if err := json.Unmarshal(line, &je); err != nil {
		return // partial line, etc.
	}
	if !strings.Contains(je.Message, "sandbox-egress") {
		return
	}
	src := matchOne(srcRE, je.Message)
	dst := matchOne(dstRE, je.Message)
	dptStr := matchOne(dptRE, je.Message)
	if src == "" || dst == "" || dptStr == "" {
		return
	}
	dpt, err := strconv.Atoi(dptStr)
	if err != nil {
		return
	}
	sandboxID := c.Manager.Sources.Lookup(src)
	// Even with empty sandbox_id we still log — a race against
	// very-recently-removed sandboxes is possible and the line is
	// worth preserving for forensics.
	rec := egressLine{
		TS:        time.Now().UTC().Unix(),
		SandboxID: sandboxID,
		SrcIP:     src,
		DstIP:     dst,
		DstPort:   dpt,
		Event:     "connection_start",
	}
	if err := c.write(rec); err != nil {
		c.Log.Warn("egress: write failed", "err", err.Error())
		return
	}
	// Metrics — bucket the port to keep cardinality bounded.
	if sandboxID != "" {
		metrics.EgressConnections.WithLabelValues(sandboxID, portBucket(dpt)).Inc()
	}
	c.updateLag(je.RealtimeTimestamp)
}

func matchOne(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// portBucket folds the destination port into one of four reporting
// buckets so the Prometheus cardinality stays bounded.
// roadmap §9 prescribes these four.
func portBucket(p int) string {
	switch p {
	case 80:
		return "http"
	case 443:
		return "https"
	case 22:
		return "ssh"
	default:
		return "other"
	}
}

func (c *Collector) write(rec egressLine) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	day := time.Unix(rec.TS, 0).UTC().Format("2006-01-02")
	path := filepath.Join(c.LogDir, day+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = f.Write(b)
	if err == nil {
		c.lastWrite = time.Now()
	}
	return err
}

func (c *Collector) updateLag(realtimeMicros string) {
	if realtimeMicros == "" {
		return
	}
	us, err := strconv.ParseInt(realtimeMicros, 10, 64)
	if err != nil {
		return
	}
	t := time.Unix(0, us*1000)
	metrics.EgressLogLagSeconds.Set(time.Since(t).Seconds())
}

// DropPoller is the small goroutine that polls `nft -j list table`
// every 30 s, diffs the per-rule counters, and feeds the deltas into
// `sandboxd_egress_drops_total`. roadmap §9: "derived by tailing
// nftables counters every 30 s and reporting deltas. Cheap,
// accurate, no extra log volume."
type DropPoller struct {
	Nft      *Nft
	Interval time.Duration
	Log      *slog.Logger

	mu    sync.Mutex
	prev  map[string]uint64 // reason -> packets
}

// Run blocks until ctx is cancelled. Interval<=0 → defaults to 30s.
func (p *DropPoller) Run(ctx context.Context) error {
	if p.Interval <= 0 {
		p.Interval = 30 * time.Second
	}
	p.prev = map[string]uint64{}
	// First tick records the baseline without emitting deltas — the
	// counters reflect everything since boot, not since sandboxd
	// started, so we don't want the first scrape to spike.
	if cs, err := p.Nft.ReadDropCounters(ctx); err == nil {
		for _, c := range cs {
			p.prev[c.Reason] += c.Packets
		}
	}
	tk := time.NewTicker(p.Interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
		}
		cs, err := p.Nft.ReadDropCounters(ctx)
		if err != nil {
			p.Log.Warn("egress: drop-counter poll failed", "err", err.Error())
			continue
		}
		curr := map[string]uint64{}
		for _, c := range cs {
			curr[c.Reason] += c.Packets
		}
		p.mu.Lock()
		for reason, n := range curr {
			old := p.prev[reason]
			if n > old {
				metrics.EgressDrops.WithLabelValues(reason).Add(float64(n - old))
			}
		}
		p.prev = curr
		p.mu.Unlock()
	}
}
