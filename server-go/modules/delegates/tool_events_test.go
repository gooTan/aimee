package delegates

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	delegatecontract "github.com/JBailes/aimee/server-go/delegate"
	internalapi "github.com/JBailes/aimee/server-go/internal/api"
	"github.com/JBailes/aimee/server-go/internal/db1"
	"github.com/JBailes/aimee/server-go/internal/wfe"
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

func TestParseCodexToolCompleteSurfacesFailedAndCancelledMCP(t *testing.T) {
	col := newToolCollector()
	parseCodexToolComplete(`{"type":"item.completed","item":{"type":"mcp_tool_call","name":"search_memory","id":"mcp-1","status":"failed","result":"secret raw result"}}`, col)
	parseCodexToolComplete(`{"type":"item.completed","item":{"type":"mcp_tool_call","name":"search_memory","id":"mcp-2","status":"cancelled","error":"raw cancellation detail"}}`, col)
	evs := col.result()
	if len(evs) != 2 || evs[0].Status != "failed" || evs[1].Status != "cancelled" {
		t.Fatalf("codex mcp statuses = %+v", evs)
	}
	for _, ev := range evs {
		if strings.Contains(ev.ToolName, "secret raw result") || strings.Contains(ev.CallID, "raw cancellation") {
			t.Fatalf("raw MCP payload leaked into event: %+v", ev)
		}
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

func TestParseACPToolEventsPreserveFailedAndCancelled(t *testing.T) {
	col := newToolCollector()
	parseACPToolEvent("tool_call_update", "Read", "call-f", "failed", col)
	parseACPToolEvent("tool_call_update", "Read", "call-c", "cancelled", col)
	evs := col.result()
	if len(evs) != 2 || evs[0].Status != "failed" || evs[1].Status != "cancelled" {
		t.Fatalf("status = %+v", evs)
	}
}

func TestACPAnonymousToolCallsGetDistinctIDsAcrossCollectors(t *testing.T) {
	col := newToolCollector()
	parseACPToolEvent("tool_call", "Read", "", "in_progress", col)
	parseACPToolEvent("tool_call", "Read", "", "in_progress", col)
	parseACPToolEvent("tool_call_update", "Read", "", "completed", col)
	parseACPToolEvent("tool_call_update", "Read", "", "completed", col)
	evs := col.result()
	if len(evs) != 4 {
		t.Fatalf("anonymous ACP events = %+v", evs)
	}
	if evs[0].CallID == evs[1].CallID || evs[2].CallID != evs[0].CallID || evs[3].CallID != evs[1].CallID {
		t.Fatalf("anonymous ACP call IDs = %+v", evs)
	}
	col2 := newToolCollector()
	parseACPToolEvent("tool_call", "Read", "", "in_progress", col2)
	if got := col2.result()[0].CallID; got == evs[0].CallID {
		t.Fatalf("separate collectors reused synthetic call ID %q", got)
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

func TestAntigravityToolStreamParserEmitsToolEvents(t *testing.T) {
	col := newToolCollector()
	writer := &toolEventStreamWriter{kind: "agy", col: col, underlying: io.Discard}
	_, err := writer.Write([]byte(
		`{"event":"tool_call","tool":"bash","id":"agy-call-1"}` + "\n" +
			`{"event":"tool_result","tool":"bash","id":"agy-call-1"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	evs := col.result()
	if len(evs) != 2 || evs[0].Status != "started" || evs[1].Status != "completed" || evs[1].CallID != "agy-call-1" {
		t.Fatalf("antigravity events = %+v", evs)
	}

	col = newToolCollector()
	writer = &toolEventStreamWriter{kind: "agy", col: col, underlying: io.Discard}
	_, err = writer.Write([]byte(
		`{"event":"step_update","step_update":{"conversation_id":"conversation-1","step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"view_file"}}` + "\n" +
			`{"event":"step_update","step_update":{"conversation_id":"conversation-1","step_index":2,"state":"DONE","step_type":"tool","tool_name":"view_file"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	evs = col.result()
	if len(evs) != 2 || evs[0].Status != "started" || evs[1].Status != "completed" ||
		evs[0].ToolName != "view_file" || evs[0].CallID != evs[1].CallID {
		t.Fatalf("antigravity step events = %+v", evs)
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
	// Exercise the real parser -> collector -> Unix socket -> API -> DB path
	// while the provider turn is blocked after writing its structured event.
	dir := t.TempDir()
	socket := dir + "/aimee-wfe-http.sock"
	l, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("Unix sockets unavailable in this test sandbox: %v", err)
		}
		t.Fatal(err)
	}
	defer l.Close()
	store, err := db1.Open(filepath.Join(dir, "aimee.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	artifacts, err := wfe.NewArtifactStore(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := internalapi.New(store, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_live_http", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: server}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	t.Setenv("AIMEE_WFE_HTTP_SOCKET", socket)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wf := &delegatecontract.WorkflowContext{WorkItemID: "wi_live_http", Stage: "impl", Model: "codex", Role: "code", Persona: "engineer"}
	col := newToolCollector()
	col.setWorkflow(wf)
	col.setContext(ctx)
	defer col.close()
	writer := &toolEventStreamWriter{kind: "claude", col: col, underlying: io.Discard}
	release := make(chan struct{})
	parsed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _ = writer.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","id":"call_http","input":{"command":"ls"}}]}}` + "\n"))
		close(parsed)
		<-release
		close(done)
	}()
	<-parsed
	var liveFound, progressFound bool
	for i := 0; i < 100; i++ {
		events, queryErr := store.Events(t.Context(), "wi_live_http", 0, 20)
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		for _, event := range events {
			if event.Kind == delegatecontract.ToolEventStart && strings.Contains(event.Detail, "call_id=call_http") {
				liveFound = true
			}
			if event.Kind == "model_progress" && event.Detail == "status=response_streaming" {
				progressFound = true
			}
		}
		if liveFound && progressFound {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !liveFound {
		t.Fatal("live HTTP event was not observed while provider turn was blocked")
	}
	if !progressFound {
		t.Fatal("response stream progress was not observed while provider turn was blocked")
	}
	close(release)
	<-done
	// The same safe event is the batch fallback if delivery was still pending;
	// persistence dedup keeps the live and batch paths to one history row.
	detail := delegatecontract.FormatToolDetail(wf, delegatecontract.ToolEvent{ToolName: "Bash", CallID: "call_http", Status: "started"}, 0)
	dupBody := `{"work_item_id":"wi_live_http","stage":"impl","kind":"model_tool_start","actor":"codex","detail":"` + detail + `"}`
	req, err := http.NewRequest(http.MethodPost, "http://localhost/internal/model-events", strings.NewReader(dupBody))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", socket)
	}}}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	events, err := store.Events(t.Context(), "wi_live_http", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, event := range events {
		if event.Kind == delegatecontract.ToolEventStart && strings.Contains(event.Detail, "call_id=call_http") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate live/batch created %d events: %+v", count, events)
	}
}

func jsonMarshal(v any) ([]byte, error)      { return json.Marshal(v) }
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
