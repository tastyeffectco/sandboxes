package main

import (
	"strings"
	"testing"

	"github.com/sandboxd/control-plane/internal/runtime"
)

// captured records the (type, data) of every event the parser emits,
// so tests can assert on the live stream that flows to SSE.
type captured struct {
	events []struct {
		typ  string
		data map[string]any
	}
}

func (c *captured) sink(typ string, data any) {
	m, _ := data.(map[string]any)
	c.events = append(c.events, struct {
		typ  string
		data map[string]any
	}{typ, m})
}

func (c *captured) ofType(t string) []map[string]any {
	out := []map[string]any{}
	for _, e := range c.events {
		if e.typ == t {
			out = append(out, e.data)
		}
	}
	return out
}

// --- happy path: text + tool + step-finish ----------------------------

func TestParse_TextToolStepFinish(t *testing.T) {
	const stream = `
{"type":"text","part":{"id":"p1","type":"text","text":"Hello "}}
{"type":"text","part":{"id":"p2","type":"text","text":"world"}}
{"part":{"id":"t1","type":"tool","tool":"read","state":{"status":"running","input":{"filePath":"/x/y.tsx"}}}}
{"part":{"id":"t1","type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":"/x/y.tsx"}}}}
{"part":{"type":"step-finish","tokens":{"input":100,"output":50,"reasoning":10,"cache":{"read":5,"write":1}},"cost":0.0025}}
`
	c := &captured{}
	pr := parseOpencodeStream(strings.NewReader(stream), c.sink)

	if !pr.SawText || !pr.SawTool {
		t.Fatalf("SawText=%v SawTool=%v", pr.SawText, pr.SawTool)
	}
	if got, want := pr.FinalMessage, "Hello world"; got != want {
		t.Errorf("FinalMessage = %q, want %q", got, want)
	}
	if pr.APIErr != "" {
		t.Errorf("APIErr = %q, want empty", pr.APIErr)
	}
	want := runtime.TokenUsage{Input: 100, Output: 50, Reasoning: 10, CacheRead: 5, CacheWrite: 1, Total: 166, Cost: 0.0025}
	if pr.Usage != want {
		t.Errorf("Usage = %+v, want %+v", pr.Usage, want)
	}

	msgs := c.ofType(runtime.EventMessage)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 message events, got %d", len(msgs))
	}
	if msgs[0]["text"] != "Hello " || msgs[1]["text"] != "world" {
		t.Errorf("message texts wrong: %v", msgs)
	}

	tools := c.ofType(runtime.EventTool)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tool events (running, completed), got %d", len(tools))
	}
	if tools[0]["status"] != "running" || tools[1]["status"] != "completed" {
		t.Errorf("tool statuses wrong: %v", tools)
	}
	if tools[0]["path"] != "/x/y.tsx" {
		t.Errorf("tool path = %v, want /x/y.tsx", tools[0]["path"])
	}
}

// --- type:"error" event captures the message + emits role=agent_error -

func TestParse_ErrorEventCapturesMessage(t *testing.T) {
	const stream = `{"type":"error","timestamp":1,"sessionID":"s","error":{"name":"APIError","data":{"message":"Invalid API key.","statusCode":401}}}`
	c := &captured{}
	pr := parseOpencodeStream(strings.NewReader(stream), c.sink)

	if pr.APIErr != "Invalid API key." {
		t.Fatalf("APIErr = %q, want 'Invalid API key.'", pr.APIErr)
	}
	if pr.SawText || pr.SawTool {
		t.Errorf("SawText=%v SawTool=%v on a pure-error stream", pr.SawText, pr.SawTool)
	}
	if pr.FinalMessage != "" {
		t.Errorf("FinalMessage = %q, want empty", pr.FinalMessage)
	}
	msgs := c.ofType(runtime.EventMessage)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message event, got %d", len(msgs))
	}
	if msgs[0]["role"] != "agent_error" {
		t.Errorf("role = %v, want agent_error", msgs[0]["role"])
	}
	if msgs[0]["text"] != "Invalid API key." {
		t.Errorf("text = %v, want 'Invalid API key.'", msgs[0]["text"])
	}
}

// --- type:"error" with only Name field (no Data.Message) --------------

func TestParse_ErrorEventFallsBackToName(t *testing.T) {
	const stream = `{"type":"error","error":{"name":"BoomError"}}`
	c := &captured{}
	pr := parseOpencodeStream(strings.NewReader(stream), c.sink)
	if pr.APIErr != "BoomError" {
		t.Fatalf("APIErr = %q, want BoomError", pr.APIErr)
	}
}

// --- type:"error" with neither field still produces something --------

func TestParse_ErrorEventNoFieldsStillCaptured(t *testing.T) {
	const stream = `{"type":"error"}`
	c := &captured{}
	pr := parseOpencodeStream(strings.NewReader(stream), c.sink)
	if pr.APIErr == "" {
		t.Fatal("APIErr should be non-empty even when error fields are absent")
	}
}

// --- unparseable lines are skipped silently ---------------------------

func TestParse_GarbageLinesSkipped(t *testing.T) {
	const stream = `not json at all
{"type":"text","part":{"id":"p1","type":"text","text":"ok"}}
also not json`
	c := &captured{}
	pr := parseOpencodeStream(strings.NewReader(stream), c.sink)
	if pr.FinalMessage != "ok" {
		t.Errorf("FinalMessage = %q, want 'ok'", pr.FinalMessage)
	}
}

// --- tool event dedup per (part, status) ------------------------------

func TestParse_ToolDedupPerPartStatus(t *testing.T) {
	const stream = `
{"part":{"id":"t1","type":"tool","tool":"glob","state":{"status":"running"}}}
{"part":{"id":"t1","type":"tool","tool":"glob","state":{"status":"running"}}}
{"part":{"id":"t1","type":"tool","tool":"glob","state":{"status":"completed"}}}
{"part":{"id":"t1","type":"tool","tool":"glob","state":{"status":"completed"}}}
`
	c := &captured{}
	parseOpencodeStream(strings.NewReader(stream), c.sink)
	if got := len(c.ofType(runtime.EventTool)); got != 2 {
		t.Fatalf("expected 2 dedup'd tool events, got %d", got)
	}
}

// --- multiple step-finish events accumulate tokens --------------------

func TestParse_StepFinishAccumulates(t *testing.T) {
	const stream = `
{"part":{"type":"step-finish","tokens":{"input":10,"output":1}}}
{"part":{"type":"step-finish","tokens":{"input":20,"output":2}}}
`
	c := &captured{}
	pr := parseOpencodeStream(strings.NewReader(stream), c.sink)
	if pr.Usage.Input != 30 || pr.Usage.Output != 3 {
		t.Errorf("Usage Input/Output = %d/%d, want 30/3", pr.Usage.Input, pr.Usage.Output)
	}
}

// --- mixed: partial text then error → SawText true, APIErr set --------

func TestParse_PartialTextThenError(t *testing.T) {
	const stream = `
{"type":"text","part":{"id":"p1","type":"text","text":"partial..."}}
{"type":"error","error":{"data":{"message":"rate limit","statusCode":429}}}
`
	c := &captured{}
	pr := parseOpencodeStream(strings.NewReader(stream), c.sink)
	if !pr.SawText {
		t.Error("SawText should be true (we did see text before the error)")
	}
	if pr.APIErr != "rate limit" {
		t.Errorf("APIErr = %q, want 'rate limit'", pr.APIErr)
	}
	if pr.FinalMessage != "partial..." {
		t.Errorf("FinalMessage = %q, want 'partial...'", pr.FinalMessage)
	}
}
