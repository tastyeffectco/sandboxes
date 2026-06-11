package activity

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sandboxd/control-plane/internal/store"
)

// Poller scrapes Traefik's Prometheus metrics endpoint and bumps
// last_active_at for any service whose currently-open-connections
// value is non-zero.
//
// **HOST-VERIFICATION GATE** (roadmap §3): the exact metric name and
// label shape used to track currently-open connections per service
// differs between Traefik versions and the `addServicesLabels`
// configuration setting. Phase 5 does NOT commit a poller that
// assumes a specific metric name from this design — operators verify
// the metric on the running host first (see
// control-plane/README.md "Phase 5 Traefik connection metric") and
// either:
//
//	(a) supply MetricNameRE + ServiceLabel to the poller, OR
//	(b) skip the poller entirely (the access-log tailer is the only
//	    signal; SANDBOXD_WAKE_GRACE_SECONDS is widened to compensate).
//
// If MetricNameRE is empty the Poller logs a one-liner and exits
// cleanly without doing any work. Run() returns nil so main.go's
// goroutine fan-out doesn't trip on it.
type Poller struct {
	MetricsURL    string
	Interval      time.Duration
	MetricNameRE  *regexp.Regexp // matches the OPEN-connections metric name; nil = fallback mode
	ServiceLabel  string         // e.g. "service" or "name"; the label that carries the docker router/service id
	PreviewDomain string         // for parsing the router-name back to a sandbox id
	Store         *store.Store
	Log           *slog.Logger

	hostRE *regexp.Regexp
	http   *http.Client
}

// Run blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	if p.MetricNameRE == nil {
		p.Log.Info("poller: fallback mode — no Traefik connection-metric configured; relying on access-log tailer alone")
		return nil
	}
	if p.Interval <= 0 {
		p.Interval = 15 * time.Second
	}
	if p.http == nil {
		p.http = &http.Client{Timeout: 4 * time.Second}
	}
	// Router names from Phase 4's traefik.Labels: `s-<id>-<port>`.
	p.hostRE = regexp.MustCompile(`^s-([0-9A-Za-z]+)-([0-9]+)(?:@docker)?$`)

	tk := time.NewTicker(p.Interval)
	defer tk.Stop()
	for {
		if err := p.pollOnce(ctx); err != nil && !contextDone(err) {
			p.Log.Warn("poller: scrape failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", p.MetricsURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("scrape %s: HTTP %d", p.MetricsURL, resp.StatusCode)
	}
	return p.parseAndBump(ctx, resp.Body)
}

// parseAndBump walks the Prometheus text-exposition body and bumps
// any sandbox id whose configured metric is > 0.
func (p *Poller) parseAndBump(ctx context.Context, r io.Reader) error {
	now := time.Now().UTC()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: name{labels} value [timestamp]
		brace := strings.IndexByte(line, '{')
		end := strings.LastIndexByte(line, ' ')
		if brace < 0 || end < 0 || end < brace {
			continue
		}
		name := line[:brace]
		if !p.MetricNameRE.MatchString(name) {
			continue
		}
		labels := line[brace+1 : strings.LastIndexByte(line, '}')]
		valueStr := strings.TrimSpace(line[end+1:])
		v, err := strconv.ParseFloat(valueStr, 64)
		if err != nil || v <= 0 {
			continue
		}
		svc := extractLabel(labels, p.ServiceLabel)
		if svc == "" {
			continue
		}
		m := p.hostRE.FindStringSubmatch(svc)
		if m == nil {
			continue
		}
		id := m[1]
		if err := p.Store.BumpLastActive(ctx, id, now); err != nil {
			p.Log.Warn("poller: BumpLastActive failed",
				"sandbox_id", id, "err", err.Error())
		}
	}
	return sc.Err()
}

// extractLabel pulls the value of a specific label key from a
// Prometheus-text labels segment. Format is `k="v",k2="v2"`.
func extractLabel(labels, key string) string {
	want := key + `="`
	i := strings.Index(labels, want)
	if i < 0 {
		return ""
	}
	start := i + len(want)
	end := strings.IndexByte(labels[start:], '"')
	if end < 0 {
		return ""
	}
	return labels[start : start+end]
}

func contextDone(err error) bool {
	for e := err; e != nil; {
		if e == context.Canceled || e == context.DeadlineExceeded {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		uw, ok := e.(unwrapper)
		if !ok {
			break
		}
		e = uw.Unwrap()
	}
	return false
}
