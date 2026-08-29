package delegates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	startTimes      map[string]time.Time
	anonymousStarts map[string][]string
	events          []delegatecontract.ToolEvent
	workflow        *delegatecontract.WorkflowContext
	// liveFunc is injected for tests; if nil the default HTTP poster is used.
	liveFunc  func(wf *delegatecontract.WorkflowContext, ev delegatecontract.ToolEvent) bool
	stateMu   sync.Mutex
	liveCtx   context.Context
	cancel    context.CancelFunc
	queue     chan delegatecontract.ToolEvent
	liveWG    sync.WaitGroup
	delivered map[string]struct{}
	closed    bool
}

func newToolCollector() *toolEventCollector {
	return &toolEventCollector{startTimes: make(map[string]time.Time), anonymousStarts: make(map[string][]string), delivered: make(map[string]struct{})}
}

func (c *toolEventCollector) setWorkflow(wf *delegatecontract.WorkflowContext) {
	if c == nil {
		return
	}
	c.workflow = wf
}

func (c *toolEventCollector) setContext(ctx context.Context) {
	if c == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.liveCtx == nil {
		c.liveCtx, c.cancel = context.WithCancel(ctx)
	}
}

func (c *toolEventCollector) close() {
	if c == nil {
		return
	}
	c.stateMu.Lock()
	if !c.closed {
		c.closed = true
		if c.cancel != nil {
			c.cancel()
		}
	}
	c.stateMu.Unlock()
	c.liveWG.Wait()
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

func postToolEventLive(ctx context.Context, wf *delegatecontract.WorkflowContext, ev delegatecontract.ToolEvent) bool {
	kind := delegatecontract.ToolEventKind(ev.Status)
	detail := delegatecontract.FormatToolDetail(wf, ev, 0)
	return postModelEventLive(ctx, wf, kind, detail)
}

func postModelEventLive(ctx context.Context, wf *delegatecontract.WorkflowContext, kind, detail string) bool {
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
			DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(dialCtx, "unix", socket)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/internal/model-events", bytes.NewReader(body))
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

func postModelProgressLive(ctx context.Context, wf *delegatecontract.WorkflowContext, status string) bool {
	status = delegatecontract.SafeDiagnostic(strings.TrimSpace(status))
	if status == "" {
		return false
	}
	detail := "status=" + strings.ReplaceAll(status, " ", "_")
	return postModelEventLive(ctx, wf, "model_progress", detail)
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
	c.enqueueLive(ev)
	return false
}

const liveQueueCapacity = 64

func (c *toolEventCollector) enqueueLive(ev delegatecontract.ToolEvent) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closed || c.liveCtx == nil || c.workflow == nil {
		return
	}
	if c.queue == nil {
		c.queue = make(chan delegatecontract.ToolEvent, liveQueueCapacity)
		c.liveWG.Add(1)
		go c.liveLoop(c.liveCtx, c.queue)
	}
	select {
	case c.queue <- ev:
	default:
		// The event remains in c.events for the batch fallback.
	}
}

func (c *toolEventCollector) liveLoop(ctx context.Context, queue <-chan delegatecontract.ToolEvent) {
	defer c.liveWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-queue:
			if postToolEventLive(ctx, c.workflow, ev) {
				key := c.eventKey(ev)
				c.stateMu.Lock()
				c.delivered[key] = struct{}{}
				c.stateMu.Unlock()
			}
		}
	}
}

func (c *toolEventCollector) eventKey(ev delegatecontract.ToolEvent) string {
	return delegatecontract.ToolEventKind(ev.Status) + "\x00" + delegatecontract.FormatToolDetail(c.workflow, ev, 0)
}

func (c *toolEventCollector) add(toolName, callID, status, phase string, elapsed time.Duration) {
	if c == nil {
		return
	}
	toolName = normalizedToolName(toolName)
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
	c.stateMu.Lock()
	c.events = append(c.events, ev)
	c.stateMu.Unlock()
	// Production delivery is asynchronous. The event stays in c.events until
	// the worker confirms delivery, so a dropped or canceled request is retained.
	if c.tryLive(ev) {
		key := c.eventKey(ev)
		c.stateMu.Lock()
		filtered := c.events[:0]
		for _, existing := range c.events {
			if c.eventKey(existing) != key {
				filtered = append(filtered, existing)
			}
		}
		c.events = filtered
		c.stateMu.Unlock()
	}
}

func normalizedToolName(toolName string) string {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return "unknown"
	}
	if len(toolName) > 31 {
		return toolName[:31]
	}
	return toolName
}

var nextSyntheticCallID atomic.Uint64

func (c *toolEventCollector) syntheticCallID() string {
	return "acp-synthetic-" + strconv.FormatUint(nextSyntheticCallID.Add(1), 10)
}

func (c *toolEventCollector) anonymousCallID(toolName string) string {
	ids := c.anonymousStarts[normalizedToolName(toolName)]
	if len(ids) == 0 {
		return ""
	}
	callID := ids[0]
	if len(ids) == 1 {
		delete(c.anonymousStarts, normalizedToolName(toolName))
	} else {
		c.anonymousStarts[normalizedToolName(toolName)] = ids[1:]
	}
	return callID
}

func (c *toolEventCollector) recordStart(toolName, callID, phase string) {
	if c == nil {
		return
	}
	toolName = normalizedToolName(toolName)
	callID = strings.TrimSpace(callID)
	if callID == "" {
		callID = c.syntheticCallID()
		c.anonymousStarts[toolName] = append(c.anonymousStarts[toolName], callID)
	}
	c.startTimes[callID] = time.Now()
	c.add(toolName, callID, "started", phase, 0)
}

func (c *toolEventCollector) recordComplete(toolName, callID, phase string) {
	c.recordFinish(toolName, callID, "completed", phase)
}

func (c *toolEventCollector) recordFinish(toolName, callID, status, phase string) {
	if c == nil {
		return
	}
	toolName = normalizedToolName(toolName)
	callID = strings.TrimSpace(callID)
	if callID == "" {
		callID = c.anonymousCallID(toolName)
		if callID == "" {
			callID = c.syntheticCallID()
		}
	}
	var elapsed time.Duration
	if t, ok := c.startTimes[callID]; ok {
		elapsed = time.Since(t)
		delete(c.startTimes, callID)
	}
	if elapsed == 0 {
		// No matching start — this protocol may only emit completions (codex).
		c.add(toolName, callID, status, phase, 0)
	} else {
		c.add(toolName, callID, status, phase, elapsed)
	}
}

func (c *toolEventCollector) recordCancelled(toolName, callID, phase string) {
	c.recordFinish(toolName, callID, "cancelled", phase)
}

func (c *toolEventCollector) recordFailed(toolName, callID, phase string) {
	c.recordFinish(toolName, callID, "failed", phase)
}

func (c *toolEventCollector) recordError(toolName, callID, phase string) {
	if c == nil {
		return
	}
	toolName = normalizedToolName(toolName)
	callID = strings.TrimSpace(callID)
	if callID == "" {
		callID = c.anonymousCallID(toolName)
		if callID == "" {
			callID = c.syntheticCallID()
		}
	}
	var elapsed time.Duration
	if t, ok := c.startTimes[callID]; ok {
		elapsed = time.Since(t)
		delete(c.startTimes, callID)
	}
	c.add(toolName, callID, "error", phase, elapsed)
}

func (c *toolEventCollector) result() []delegatecontract.ToolEvent {
	if c == nil {
		return nil
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	var out []delegatecontract.ToolEvent
	for _, ev := range c.events {
		if _, ok := c.delivered[c.eventKey(ev)]; ok {
			continue
		}
		out = append(out, ev)
	}
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
	toolName := itemType
	if n, ok := item["name"].(string); ok && n != "" {
		toolName = n
	} else if n, ok := item["tool"].(string); ok && n != "" {
		toolName = n
	}
	callID, _ := item["id"].(string)
	if callID == "" {
		callID, _ = item["call_id"].(string)
	}
	status, _ := item["status"].(string)
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed":
		col.recordFailed(toolName, callID, "")
	case "error":
		col.recordError(toolName, callID, "")
	case "cancelled", "canceled":
		col.recordCancelled(toolName, callID, "")
	default:
		col.recordComplete(toolName, callID, "")
	}
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
	if ev, _ := obj["event"].(string); ev == "step_update" {
		step, _ := obj["step_update"].(map[string]any)
		stepType, _ := step["step_type"].(string)
		if stepType == "tool" || stepType == "tool_use" {
			name, _ := step["tool_name"].(string)
			if name == "" {
				info, _ := step["tool_info"].(map[string]any)
				name, _ = info["name"].(string)
			}
			callID := fmt.Sprint(step["conversation_id"], ":", step["step_index"])
			switch strings.ToUpper(strings.TrimSpace(fmt.Sprint(step["state"]))) {
			case "DONE", "COMPLETED":
				col.recordComplete(name, callID, "")
			case "ERROR", "FAILED":
				col.recordFailed(name, callID, "")
			case "CANCELLED", "CANCELED":
				col.recordCancelled(name, callID, "")
			default:
				col.recordStart(name, callID, "")
			}
			return
		}
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
	} else if status == "failed" {
		col.recordFailed(toolName, callID, "")
	} else if status == "error" {
		col.recordError(toolName, callID, "")
	} else if status == "cancelled" || status == "canceled" {
		col.recordCancelled(toolName, callID, "")
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
	progress   sync.Once
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
		if outputResponseStarted(w.kind, []byte(line+"\n")) {
			w.progress.Do(func() {
				postModelProgressLive(w.col.liveCtx, w.col.workflow, "response_streaming")
			})
		}
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
