package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/sandboxed/control-plane/internal/runtime"
)

// agentSpec is the input to an agent adapter run.
type agentSpec struct {
	workDir string
	prompt  string
	env     map[string]string
	rawLog  io.Writer // the agent's own diagnostics (stderr)
}

// agent is the coding-agent adapter boundary. This slice implements
// opencode only; claude_code / codex slot in here later.
type agent interface {
	name() string
	run(ctx context.Context, spec agentSpec, emit eventSink) (finalMessage string, usage runtime.TokenUsage, err error)
}

// opencodeAgent drives `opencode run --format json`.
type opencodeAgent struct{ log *slog.Logger }

func (o *opencodeAgent) name() string { return "opencode" }

// opencodeEvent is the envelope of one `opencode run --format json`
// output line. Only the fields runtimed maps are declared; the schema
// is otherwise treated as opaque (provider behaviour is best-effort).
type opencodeEvent struct {
	Type string `json:"type"`
	Part struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Text  string `json:"text"`
		Tool  string `json:"tool"` // tool name on a "tool" part: edit, write, bash, glob…
		State struct {
			Status string          `json:"status"` // pending | running | completed | error
			Input  json.RawMessage `json:"input"`  // tool args — shape varies per tool
		} `json:"state"`
		// step-finish parts carry per-step token accounting.
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
		Cost float64 `json:"cost"`
	} `json:"part"`
	// Error is present on top-level `{"type":"error", ...}` lines. The
	// API-side error shape is `{name, data:{message, statusCode}}`;
	// opencode also emits other error shapes. Capturing both Name and
	// Data.Message handles the observed cases.
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message    string `json:"message"`
			StatusCode int    `json:"statusCode"`
		} `json:"data"`
	} `json:"error,omitempty"`
}

// opencodeParseResult is the outcome of consuming an opencode stdout
// stream. Pulled out for unit testing without spawning opencode.
type opencodeParseResult struct {
	FinalMessage string
	Usage        runtime.TokenUsage
	SawText      bool // any text part observed (real model output)
	SawTool      bool // any tool part observed (real model output)
	APIErr       string
}

// parseOpencodeStream consumes NDJSON from r, dispatches canonical
// events through emit, and returns a structured summary. Pure — no
// process management — so it's exercisable from tests with a string
// reader.
func parseOpencodeStream(r io.Reader, emit eventSink) opencodeParseResult {
	var pr opencodeParseResult
	parts := map[string]string{}
	var order []string
	seenTool := map[string]bool{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev opencodeEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue // best-effort: skip lines we cannot parse
		}
		switch {
		case ev.Type == "error":
			// opencode reported a structured error. Capture the first
			// one's message so the agent.run caller can classify the
			// task as failed; also emit as a canonical `message` event
			// with role=agent_error so SSE consumers see it.
			msg := "agent error (no message)"
			if ev.Error != nil {
				switch {
				case ev.Error.Data.Message != "":
					msg = ev.Error.Data.Message
				case ev.Error.Name != "":
					msg = ev.Error.Name
				}
			}
			if pr.APIErr == "" {
				pr.APIErr = msg
			}
			emit(runtime.EventMessage, map[string]any{"role": "agent_error", "text": msg})

		case ev.Type == "text" && ev.Part.Text != "":
			pr.SawText = true
			if _, seen := parts[ev.Part.ID]; !seen {
				order = append(order, ev.Part.ID)
			}
			parts[ev.Part.ID] = ev.Part.Text
			emit(runtime.EventMessage, map[string]any{"role": "agent", "text": ev.Part.Text})

		case ev.Part.Type == "tool" && ev.Part.Tool != "":
			// A coding-agent sub-step — the live progress feed. Emitted
			// as structured, language-neutral data (a tool name + a
			// path/identifier); the consumer localises the wording.
			// Deduped per (part, status) so a status transition emits
			// at most once.
			pr.SawTool = true
			key := ev.Part.ID + "|" + ev.Part.State.Status
			if !seenTool[key] {
				seenTool[key] = true
				emit(runtime.EventTool, map[string]any{
					"name":   ev.Part.Tool,
					"status": ev.Part.State.Status,
					"path":   toolTarget(ev.Part.State.Input),
				})
			}

		case ev.Part.Type == "step-finish":
			// Per-step token accounting — summed across the session.
			tk := ev.Part.Tokens
			pr.Usage.Input += tk.Input
			pr.Usage.Output += tk.Output
			pr.Usage.Reasoning += tk.Reasoning
			pr.Usage.CacheRead += tk.Cache.Read
			pr.Usage.CacheWrite += tk.Cache.Write
			pr.Usage.Cost += ev.Part.Cost
		}
	}
	pr.Usage.Total = pr.Usage.Input + pr.Usage.Output + pr.Usage.Reasoning +
		pr.Usage.CacheRead + pr.Usage.CacheWrite

	var b strings.Builder
	for _, id := range order {
		b.WriteString(parts[id])
	}
	pr.FinalMessage = b.String()
	return pr
}

// toolTarget pulls the most user-meaningful argument out of a tool
// call's input — a file path, a glob, or a command — for the live
// progress feed. It is language-neutral by design (a path/identifier,
// not prose): the consumer localises the verb around it.
func toolTarget(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in struct {
		FilePath string `json:"filePath"`
		Path     string `json:"path"`
		File     string `json:"file"`
		Pattern  string `json:"pattern"`
		Command  string `json:"command"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	t := in.FilePath
	for _, c := range []string{in.Path, in.File, in.Pattern, in.Command} {
		if t == "" {
			t = c
		}
	}
	if len(t) > 200 {
		t = t[:200]
	}
	return t
}

func (o *opencodeAgent) run(ctx context.Context, spec agentSpec, emit eventSink) (string, runtime.TokenUsage, error) {
	var usage runtime.TokenUsage
	cmd := exec.Command("opencode", "run",
		"--format", "json", "--dangerously-skip-permissions", spec.prompt)
	cmd.Dir = spec.workDir
	cmd.Env = os.Environ()
	for k, v := range spec.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Own process group so cancel/timeout kills the whole agent tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", usage, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", usage, err
	}
	if err := cmd.Start(); err != nil {
		return "", usage, fmt.Errorf("start opencode: %w", err)
	}
	pgid := cmd.Process.Pid

	// ctx cancellation (cancel or timeout) → kill the process group.
	finished := make(chan struct{})
	go func() {
		select {
		case <-finished:
		case <-ctx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			t := time.NewTimer(5 * time.Second)
			defer t.Stop()
			select {
			case <-finished:
			case <-t.C:
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
	}()
	defer close(finished)

	// stderr → the per-task agent log.
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(spec.rawLog, stderr)
		close(stderrDone)
	}()

	// Parse stdout (one JSON event per line) — pure function so it can
	// be unit-tested without spawning opencode.
	pr := parseOpencodeStream(stdout, emit)
	waitErr := cmd.Wait()
	<-stderrDone

	// Classification, in order of authority:
	//   1. ctx already errored (cancel / timeout) → caller decides; we
	//      return nil err and let the upper layer mark the task.
	//   2. opencode reported an error event → real failure regardless
	//      of exit code (it often exits 0 even after an auth failure;
	//      that was the original "succeeded with empty result" bug).
	//   3. process exited non-zero with a live ctx → real failure.
	//   4. exit 0 with NO text and NO tool events → opencode crashed
	//      silently or never produced output (`agent_no_output`). Catches
	//      the case where the error event shape changes underneath us.
	switch {
	case ctx.Err() != nil:
		return pr.FinalMessage, pr.Usage, nil
	case pr.APIErr != "":
		return pr.FinalMessage, pr.Usage, fmt.Errorf("agent error: %s", pr.APIErr)
	case waitErr != nil:
		return pr.FinalMessage, pr.Usage, fmt.Errorf("opencode exited: %w", waitErr)
	case !pr.SawText && !pr.SawTool:
		return pr.FinalMessage, pr.Usage,
			fmt.Errorf("agent produced no output (opencode exited 0 with zero events)")
	}
	return pr.FinalMessage, pr.Usage, nil
}
