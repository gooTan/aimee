package delegates

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"

	delegatecontract "github.com/JBailes/aimee/server-go/delegate"
)

func TestParseClaudeToolStartEmitsStartedOnly(t *testing.T) {
	// Claude stream-json assistant tool_use — honest normalized behavior is started only.
	col := newToolCollector()
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","id":"call_1","input":{"command":"ls"}}]}}`
	parseClaudeToolStart(line, col)
	evs := col.result()
	if len(evs) != 1 || evs[0].ToolName != "Bash" || evs[0].CallID != "call_1" || evs[0].Status != "started" {
		t.Fatalf("claude parse = %+v", evs)
	}
	// Ensure raw arguments are not stored.
	for _, ev := range evs {
		if strings.Contains(ev.ToolName, "ls") || strings.Contains(ev.CallID, "ls") {
			t.Fatalf("raw arg leaked into event: %+v", ev)
		}
	}
	// Incremental stream_event variant.
	col2 := newToolCollector()
	line2 := `{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Read","id":"call_2"}}}`
	parseClaudeToolStart(line2, col2)
	evs2 := col2.result()
	if len(evs2) != 1 || evs2[0].ToolName != "Read" {
		t.Fatalf("stream_event parse = %+v", evs2)
	}
}

func TestParseClaudeToolStartIgnoresNonTool(t *testing.T) {
	col := newToolCollector()
	parseClaudeToolStart(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`, col)
	if len(col.result()) != 0 {
		t.Fatalf("non-tool should not emit: %+v", col.result())
	}
}

func TestParseCodexToolCompleteEmitsCompletedOnly(t *testing.T) {
	// Codex exec does not expose started; the honest behavior is a single completed per item.
	col := newToolCollector()
	line := `{"type":"item.completed","item":{"type":"command_execution","id":"tool-9","text":"ls done"}}`
	parseCodexToolComplete(line, col)
	evs := col.result()
	if len(evs) != 1 || evs[0].ToolName != "command_execution" || evs[0].Status != "completed" || evs[0].CallID != "tool-9" {
		t.Fatalf("codex parse = %+v", evs)
	}
	// Agent message should not be counted as tool.
	col2 := newToolCollector()
	parseCodexToolComplete(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`, col2)
	if len(col2.result()) != 0 {
		t.Fatalf("agent_message should be ignored: %+v", col2.result())
	}
}

func TestParseACPToolEventsStartAndComplete(t *testing.T) {
	col := newToolCollector()
	// Simulate tool_call start.
	parseACPToolEvent("tool_call", "Read", "call-1", "in_progress", col)
	// Simulate tool_call_update completed.
	parseACPToolEvent("tool_call_update", "Read", "call-1", "completed", col)
	evs := col.result()
	if len(evs) != 2 {
		t.Fatalf("acp events = %+v", evs)
	}
	if evs[0].Status != "started" || evs[1].Status != "completed" {
		t.Fatalf("status = %+v", evs)
	}
	if evs[0].CallID != "call-1" || evs[1].CallID != "call-1" {
		t.Fatalf("call_id = %+v", evs)
	}
	// Elapsed should be measured between start and complete.
	if evs[1].ElapsedMS < 0 {
		t.Fatalf("elapsed negative: %+v", evs[1])
	}
}

func TestACPProtocolToolCollectorIntegration(t *testing.T) {
	st := &acpTurnState{promptID: 3}
	col := newToolCollector()
	if err := acpTurnConsumeWithCollector(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"tool-1","title":"Bash","status":"in_progress"}}}`, st, col); err != nil {
		t.Fatal(err)
	}
	if st.ToolCalls() != 1 {
		t.Fatalf("toolCalls = %d", st.ToolCalls())
	}
	if len(col.result()) != 1 || col.result()[0].Status != "started" || col.result()[0].ToolName != "Bash" {
		t.Fatalf("collector = %+v", col.result())
	}
	if err := acpTurnConsumeWithCollector(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"tool-1","title":"Bash","status":"completed"}}}`, st, col); err != nil {
		t.Fatal(err)
	}
	evs := col.result()
	if len(evs) != 2 || evs[1].Status != "completed" || evs[1].CallID != "tool-1" {
		t.Fatalf("second event = %+v", evs)
	}
}

func TestToolCollectorDoesNotStoreRawContent(t *testing.T) {
	col := newToolCollector()
	// Even if input contains secrets, only name/id/status are kept.
	parseClaudeToolStart(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","id":"call_secret","input":{"command":"echo password=supersecret"}}]}}`, col)
	evs := col.result()
	for _, ev := range evs {
		if strings.Contains(ev.ToolName, "supersecret") || strings.Contains(ev.CallID, "supersecret") {
			t.Fatalf("secret leaked: %+v", ev)
		}
	}
}

func TestToolEventsRoundTripThroughInvocationResult(t *testing.T) {
	// Ensure ToolEvents survive JSON marshal/unmarshal of InvocationResult.
	orig := delegatecontract.InvocationResult{
		Version:  delegatecontract.WireVersion,
		Status:   "done",
		Response: "ok",
		ToolEvents: []delegatecontract.ToolEvent{
			{ToolName: "Bash", CallID: "c1", Status: "started"},
			{ToolName: "Bash", CallID: "c1", Status: "completed", ElapsedMS: 120},
		},
	}
	data, err := jsonMarshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var decoded delegatecontract.InvocationResult
	if err := jsonUnmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.ToolEvents) != 2 || decoded.ToolEvents[0].ToolName != "Bash" {
		t.Fatalf("roundtrip = %+v", decoded.ToolEvents)
	}
}

func TestToolCollectorLiveIsFallbackOnly(t *testing.T) {
	// When live delivery succeeds, the collector must not retain the event for
	// batch fallback; when live fails, it must retain. This is the deduplication
	// contract that keeps `workflow status --watch` live without duplicates.
	wf := &delegatecontract.WorkflowContext{WorkItemID: "wi_live", Stage: "impl", Model: "codex", Role: "code", Persona: "engineer"}
	// Live succeeds: batch should be empty.
	col := newToolCollector()
	col.setWorkflow(wf)
	var liveCalled []delegatecontract.ToolEvent
	col.liveFunc = func(_ *delegatecontract.WorkflowContext, ev delegatecontract.ToolEvent) bool {
		liveCalled = append(liveCalled, ev)
		return true
	}
	parseClaudeToolStart(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","id":"call_1","input":{"command":"ls"}}]}}`, col)
	if len(liveCalled) != 1 || liveCalled[0].ToolName != "Bash" {
		t.Fatalf("live not called: %+v", liveCalled)
	}
	if len(col.result()) != 0 {
		t.Fatalf("batch should be fallback-only when live succeeded: %+v", col.result())
	}
	// Live fails: batch must retain.
	col2 := newToolCollector()
	col2.setWorkflow(wf)
	col2.liveFunc = func(_ *delegatecontract.WorkflowContext, ev delegatecontract.ToolEvent) bool { return false }
	parseClaudeToolStart(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","id":"call_1","input":{"command":"ls"}}]}}`, col2)
	if len(col2.result()) != 1 {
		t.Fatalf("batch should retain when live failed: %+v", col2.result())
	}
	// Without workflow (no work item), live is not attempted and batch retains.
	col3 := newToolCollector()
	parseCodexToolComplete(`{"type":"item.completed","item":{"type":"command_execution","id":"tool-9"}}`, col3)
	if len(col3.result()) != 1 {
		t.Fatalf("without workflow, batch should retain: %+v", col3.result())
	}
}

func TestToolCollectorLivePreservesSafeMetadata(t *testing.T) {
	wf := &delegatecontract.WorkflowContext{WorkItemID: "wi_safe", Stage: "plan", Model: "codex", Role: "code", Persona: "engineer", Phase: "analysis"}
	col := newToolCollector()
	col.setWorkflow(wf)
	var captured delegatecontract.ToolEvent
	col.liveFunc = func(_ *delegatecontract.WorkflowContext, ev delegatecontract.ToolEvent) bool {
		captured = ev
		// Simulate live success.
		return true
	}
	// Even if tool name tries to smuggle secrets, only safe identifiers are forwarded.
	parseClaudeToolStart(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash --secret supersecret","id":"call_secret","input":{"command":"echo"}}]}}`, col)
	if captured.ToolName != "Bash --secret supersecret" && !strings.Contains(captured.ToolName, "Bash") {
		// The collector stores raw name but FormatToolDetail will sanitize.
		t.Fatalf("captured = %+v", captured)
	}
	// Verify the formatted detail that would be sent live is safe.
	detail := delegatecontract.FormatToolDetail(wf, captured, 0)
	if strings.Contains(detail, "supersecret") {
		t.Fatalf("live detail leaked secret: %q", detail)
	}
	if !strings.Contains(detail, "model=codex") || !strings.Contains(detail, "tool=") {
		t.Fatalf("live detail missing safe metadata: %q", detail)
	}
}

func TestToolCollectorLiveViaHTTPSocketIsVisibleBeforeBatch(t *testing.T) {
	// Integration-like test for the true live path: the collector's default
	// HTTP poster (over the WFE Unix socket) must make the tool event
	// observable before the delegate turn completes, and batch must be
	// fallback-only so history has no duplicate.
	dir := t.TempDir()
	socket := dir + "/aimee-wfe-http.sock"
	// Minimal WFE HTTP handler that records to an in-memory slice, mimicking
	// the real internal/model-events endpoint's safe validation and dedup.
	var mu strings.Builder
	_ = mu
	type recorded struct {
		kind   string
		detail string
	}
	var recordedEvents []recorded
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev struct {
			WorkItemID string `json:"work_item_id"`
			Stage      string `json:"stage"`
			Kind       string `json:"kind"`
			Actor      string `json:"actor"`
			Detail     string `json:"detail"`
		}
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		// Only safe kinds and bounded detail are accepted, matching api/server.go.
		if ev.WorkItemID == "" || ev.Stage == "" || ev.Actor == "" || len(ev.Detail) > 4096 {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		// Dedup by stable identity (kind+detail) like the real handler.
		for _, e := range recordedEvents {
			if e.kind == ev.Kind && e.detail == ev.Detail {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		recordedEvents = append(recordedEvents, recorded{kind: ev.Kind, detail: ev.Detail})
		w.WriteHeader(http.StatusNoContent)
	})
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := &http.Server{Handler: handler}
	go srv.Serve(l)
	defer srv.Close()
	t.Setenv("AIMEE_WFE_HTTP_SOCKET", socket)
	wf := &delegatecontract.WorkflowContext{WorkItemID: "wi_live_http", Stage: "impl", Model: "codex", Role: "code", Persona: "engineer"}
	col := newToolCollector()
	col.setWorkflow(wf)
	// This parse should trigger a live HTTP POST before returning, and the
	// collector's batch must be empty (fallback-only).
	parseClaudeToolStart(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","id":"call_http","input":{"command":"ls"}}]}}`, col)
	if len(recordedEvents) != 1 || !strings.Contains(recordedEvents[0].detail, "tool=Bash") || !strings.Contains(recordedEvents[0].detail, "call_id=call_http") {
		t.Fatalf("live HTTP not observed: %+v", recordedEvents)
	}
	if len(col.result()) != 0 {
		t.Fatalf("batch should be empty after live success: %+v", col.result())
	}
	// A second identical event (as batch retry would) must be deduped and not duplicate live.
	dupBody := `{"work_item_id":"wi_live_http","stage":"impl","kind":"model_tool_start","actor":"codex","detail":"` + recordedEvents[0].detail + `"}`
	req, _ := http.NewRequest(http.MethodPost, "http://localhost/internal/model-events", strings.NewReader(dupBody))
	// Simulate batch POST directly to the same handler via HTTP client over socket.
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", socket)
	}}}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(recordedEvents) != 1 {
		t.Fatalf("duplicate live/batch should not create second event: %+v", recordedEvents)
	}
}

func jsonMarshal(v any) ([]byte, error)      { return json.Marshal(v) }
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
