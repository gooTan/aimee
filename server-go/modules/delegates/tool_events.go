package delegates

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	delegatecontract "github.com/JBailes/aimee/server-go/delegate"
)

// toolEventCollector accumulates safe per-tool-call telemetry during one
// delegate invocation. It reuses the existing structured stream signals —
// claude's tool_use blocks, codex's item.completed, ACP's session/update
// tool_call — and never stores raw arguments, prompts, or results.
//
// For protocols that only expose completed tool items (codex exec), the
// collector emits a single completed event per tool. For protocols that
// expose start but not completion (claude stream-json), it emits started
// only. The caller documents this honest truncation rather than fabricating
// precision; see the adapter comments below.
//
// Live path: as each safe event is parsed, the collector attempts an
// immediate POST to the WFE's internal model-events HTTP endpoint over the
// Unix socket (existing infrastructure also used by roundtable observer).
// This makes `aimee workflow status --watch` observe tool activity during a
// long model turn, before the delegate finishes. If live delivery succeeds,
// the event is not retained for batch fallback, avoiding duplicates. If live
// is unavailable/failed, the event remains in the batch for the engine's
// post-turn persistence. Deduplication on the engine side (stable
// call/event identity) is the second line of defense.
type toolEventCollector struct {
	startTimes map[string]time.Time
	events     []delegatecontract.ToolEvent
	workflow   *delegatecontract.WorkflowContext
	// liveFunc is injected for tests; if nil the default HTTP poster is used.
	liveFunc func(wf *delegatecontract.WorkflowContext, ev delegatecontract.ToolEvent) bool
}

func newToolCollector() *toolEventCollector {
	return &toolEventCollector{startTimes: make(map[string]time.Time)}
}

func (c *toolEventCollector) setWorkflow(wf *delegatecontract.WorkflowContext) {
	if c == nil {
		return
	}
	c.workflow = wf
}

// wfeHTTPSocket returns the Unix socket for the WFE internal HTTP endpoint.
func wfeHTTPSocket() string {
	if s := strings.TrimSpace(os.Getenv("AIMEE_WFE_HTTP_SOCKET")); s != "" {
		return s
	}
	if home := strings.TrimSpace(os.Getenv("AIMEE_HOME")); home != "" {
		return filepath.Join(home, "aimee-wfe-http.sock")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "aimee", "aimee-wfe-http.sock")
	}
	return ""
}

func postToolEventLive(wf *delegatecontract.WorkflowContext, ev delegatecontract.ToolEvent) bool {
	if wf == nil || strings.TrimSpace(wf.WorkItemID) == "" || strings.TrimSpace(wf.Stage) == "" {
		return false
	}
	socket := wfeHTTPSocket()
	if socket == "" {
		return false
	}
	if _, err := os.Stat(socket); err != nil {
		return false
	}
	kind := delegatecontract.ToolEventKind(ev.Status)
	detail := delegatecontract.FormatToolDetail(wf, ev, 0)
	actor := strings.TrimSpace(wf.Model)
	if actor == "" {
		actor = "unknown"
	}
	payload := map[string]string{
		"work_item_id": wf.WorkItemID,
		"stage":        wf.Stage,
		"kind":         kind,
		"actor":        actor,
		"detail":       detail,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		Timeout: 400 * time.Millisecond,
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://localhost/internal/model-events", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK
}

func (c *toolEventCollector) tryLive(ev delegatecontract.ToolEvent) bool {
	if c == nil || c.workflow == nil {
		return false
	}
	if strings.TrimSpace(c.workflow.WorkItemID) == "" || strings.TrimSpace(c.workflow.Stage) == "" {
		return false
	}
	if c.liveFunc != nil {
		return c.liveFunc(c.workflow, ev)
	}
	return postToolEventLive(c.workflow, ev)
}

func (c *toolEventCollector) add(toolName, callID, status, phase string, elapsed time.Duration) {
	if c == nil {
		return
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		toolName = "unknown"
	}
	// Sanitize to 31-char limit mirroring rescueNameMax; truncate.
	if len(toolName) > 31 {
		toolName = toolName[:31]
	}
	callID = strings.TrimSpace(callID)
	if len(callID) > 64 {
		callID = callID[:64]
	}
	ev := delegatecontract.ToolEvent{
		ToolName: toolName,
		CallID:   callID,
		Status:   status,
		Phase:    phase,
	}
	if elapsed > 0 {
		ev.ElapsedMS = elapsed.Milliseconds()
	}
	// Attempt live delivery first; on success, batch is fallback-only.
	if c.tryLive(ev) {
		return
	}
	c.events = append(c.events, ev)
}

func (c *toolEventCollector) recordStart(toolName, callID, phase string) {
	if c == nil {
		return
	}
	key := callID
	if key == "" {
		key = toolName + ":" + phase + ":" + time.Now().Format(time.RFC3339Nano)
	}
	c.startTimes[key] = time.Now()
	c.add(toolName, callID, "started", phase, 0)
}

func (c *toolEventCollector) recordComplete(toolName, callID, phase string) {
	if c == nil {
		return
	}
	var elapsed time.Duration
	for k, t := range c.startTimes {
		if callID != "" && strings.HasPrefix(k, callID) || k == callID {
			elapsed = time.Since(t)
			delete(c.startTimes, k)
			break
		}
		if callID == "" && strings.Contains(k, toolName) {
			elapsed = time.Since(t)
			delete(c.startTimes, k)
			break
		}
	}
	if elapsed == 0 {
		// No matching start — this protocol may only emit completions (codex).
		c.add(toolName, callID, "completed", phase, 0)
	} else {
		c.add(toolName, callID, "completed", phase, elapsed)
	}
}

func (c *toolEventCollector) recordError(toolName, callID, phase string) {
	if c == nil {
		return
	}
	var elapsed time.Duration
	if callID != "" {
		if t, ok := c.startTimes[callID]; ok {
			elapsed = time.Since(t)
			delete(c.startTimes, callID)
		}
	}
	c.add(toolName, callID, "error", phase, elapsed)
}

func (c *toolEventCollector) result() []delegatecontract.ToolEvent {
	if c == nil || len(c.events) == 0 {
		return nil
	}
	out := make([]delegatecontract.ToolEvent, len(c.events))
	copy(out, c.events)
	return out
}

// parseClaudeToolStart inspects one JSONL line from `claude -p --output-format stream-json`
// and reports a tool start when the line carries a tool_use block.
//
// Claude's stream-json exposes tool calls as:
//
//	{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash",...}]}}
//
// and also as incremental stream_event content_block_start tool_use.
// The existing C adapter (cli_claude.c:claude_parse_line) only emits
// CLI_EVENT_TOOL_START for these, never a completion. We reuse that
// honest signal and emit started only; completed/error are not
// fabricated. See docs in client.go for the normalized behavior.
func parseClaudeToolStart(line string, col *toolEventCollector) {
	if strings.TrimSpace(line) == "" || col == nil {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return
	}
	typ, _ := obj["type"].(string)
	switch typ {
	case "assistant":
		msg, _ := obj["message"].(map[string]any)
		if msg == nil {
			return
		}
		content, _ := msg["content"].([]any)
		for _, item := range content {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			if m["type"] == "tool_use" {
				if name, _ := m["name"].(string); name != "" {
					callID, _ := m["id"].(string)
					col.recordStart(name, callID, "")
				}
			}
		}
	case "stream_event":
		ev, _ := obj["event"].(map[string]any)
		if ev == nil {
			return
		}
		if ev["type"] == "content_block_start" {
			block, _ := ev["content_block"].(map[string]any)
			if block != nil && block["type"] == "tool_use" {
				if name, _ := block["name"].(string); name != "" {
					callID, _ := block["id"].(string)
					col.recordStart(name, callID, "")
				}
			}
		}
	}
}

// parseCodexToolComplete inspects one JSONL line from `codex exec --json`
// and reports a tool completion.
//
// Codex exec --json emits NDJSON where each tool invocation appears as:
//
//	{"type":"item.completed","item":{"type":"command_execution",...}}
//
// or file_change, mcp_tool_call, etc. The Go executor previously only
// counted these for turn budgeting. We reuse that signal but treat the
// completed item as the honest observability event: codex exec does not
// expose a separate started notification, so we emit a single
// model_tool_complete per completed non-message item. Tool name is the
// item.type (e.g., command_execution), call_id is item.id when present.
func parseCodexToolComplete(line string, col *toolEventCollector) {
	if strings.TrimSpace(line) == "" || col == nil {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return
	}
	if obj["type"] != "item.completed" {
		return
	}
	item, _ := obj["item"].(map[string]any)
	if item == nil {
		return
	}
	itemType, _ := item["type"].(string)
	if itemType == "" || itemType == "agent_message" || itemType == "reasoning" {
		return
	}
	// Codex exec item.completed is already a completed tool invocation.
	// We emit completed directly; started is not available, so we do not
	// fabricate it.
	toolName := itemType
	// Some items carry a nested tool name.
	if n, ok := item["name"].(string); ok && n != "" {
		toolName = n
	} else if n, ok := item["tool"].(string); ok && n != "" {
		toolName = n
	}
	callID, _ := item["id"].(string)
	if callID == "" {
		callID, _ = item["call_id"].(string)
	}
	col.recordComplete(toolName, callID, "")
}

// parseAgyToolComplete handles `agy -p --output-format stream-json` similarly
// to codex: completed items carry the tool work.
func parseAgyToolComplete(line string, col *toolEventCollector) {
	if strings.TrimSpace(line) == "" || col == nil {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return
	}
	// Agy stream-json uses {"event":"tool_call","tool":"bash",...} or similar
	// in practice. We handle the generic shape plus the codex-like fallback.
	if ev, _ := obj["event"].(string); ev == "tool_call" || ev == "tool_result" {
		if name, _ := obj["tool"].(string); name != "" {
			callID, _ := obj["id"].(string)
			if ev == "tool_call" {
				col.recordStart(name, callID, "")
			} else {
				col.recordComplete(name, callID, "")
			}
			return
		}
	}
	// Fallback to codex-like item.completed
	parseCodexToolComplete(line, col)
}

// parseACPToolEvent is called from acpTurnConsume for each
// session/update tool_call or tool_call_update line. The ACP protocol
// exposes both started (tool_call in_progress) and completed
// (tool_call_update completed) with a stable toolCallId and title.
func parseACPToolEvent(updateKind, toolName, callID, status string, col *toolEventCollector) {
	if col == nil {
		return
	}
	status = strings.ToLower(strings.TrimSpace(status))
	toolName = strings.TrimSpace(toolName)
	callID = strings.TrimSpace(callID)
	if status == "completed" || status == "done" {
		col.recordComplete(toolName, callID, "")
	} else if status == "failed" || status == "error" {
		col.recordError(toolName, callID, "")
	} else {
		col.recordStart(toolName, callID, "")
	}
}

// toolEventStreamWriter is an io.Writer that tees bytes to an underlying
// writer while also extracting safe per-tool-call telemetry from the same
// NDJSON stream. It reuses the existing structured signals (claude's
// tool_use, codex's item.completed, agy's tool_call) and never stores raw
// tool arguments or results.
type toolEventStreamWriter struct {
	kind       string
	col        *toolEventCollector
	underlying io.Writer
	mu         sync.Mutex
	pending    []byte
}

func (w *toolEventStreamWriter) Write(p []byte) (int, error) {
	n, err := w.underlying.Write(p)
	if w == nil || w.col == nil {
		return n, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		idx := bytes.IndexByte(w.pending, '\n')
		if idx < 0 {
			break
		}
		line := string(w.pending[:idx])
		w.pending = w.pending[idx+1:]
		switch strings.ToLower(strings.TrimSpace(w.kind)) {
		case "claude", "claude-code":
			parseClaudeToolStart(line, w.col)
		case "codex":
			parseCodexToolComplete(line, w.col)
		case "agy":
			parseAgyToolComplete(line, w.col)
		case "generic":
			// Generic CLI has no structured tool stream; no events emitted.
		default:
			// Unknown kind: attempt codex-like fallback.
			parseCodexToolComplete(line, w.col)
		}
	}
	return n, err
}

var _ io.Writer = (*toolEventStreamWriter)(nil)
