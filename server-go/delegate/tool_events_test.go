package delegate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFormatToolDetailIsSafeAndBounded(t *testing.T) {
	wf := &WorkflowContext{Model: "claude-code", Role: "code", Persona: "engineer", Phase: "analysis"}
	ev := ToolEvent{ToolName: "Bash", CallID: "call-123", Status: "started", ElapsedMS: 1500}
	detail := FormatToolDetail(wf, ev, 0)
	if !strings.Contains(detail, "model=claude-code") || !strings.Contains(detail, "tool=Bash") || !strings.Contains(detail, "call_id=call-123") || !strings.Contains(detail, "status=started") {
		t.Fatalf("detail missing fields: %q", detail)
	}
	if strings.Contains(detail, "secret") {
		t.Fatalf("detail leaked: %q", detail)
	}
	// Never include raw args.
	ev2 := ToolEvent{ToolName: "Bash`rm -rf", CallID: "id with spaces\nnewline", Status: "completed"}
	detail2 := FormatToolDetail(nil, ev2, 2*time.Second)
	if strings.Contains(detail2, "rm -rf") && !strings.Contains(detail2, "Bash_rm") {
		// sanitize replaces spaces/newlines
		t.Fatalf("unsanitized: %q", detail2)
	}
	if strings.Contains(detail2, "\n") {
		t.Fatalf("newline not sanitized: %q", detail2)
	}
	if len(detail) > 4096 {
		t.Fatalf("detail too long: %d", len(detail))
	}
}

func TestFormatToolDetailHonorsElapsed(t *testing.T) {
	ev := ToolEvent{ToolName: "Read", CallID: "c1", Status: "completed", ElapsedMS: 2345}
	detail := FormatToolDetail(nil, ev, 0)
	if !strings.Contains(detail, "elapsed=") {
		t.Fatalf("elapsed missing: %q", detail)
	}
	// Explicit elapsed param overrides ElapsedMS.
	detail2 := FormatToolDetail(nil, ev, 5*time.Second)
	if !strings.Contains(detail2, "5s") {
		t.Fatalf("explicit elapsed not used: %q", detail2)
	}
}

func TestToolEventKind(t *testing.T) {
	cases := map[string]string{
		"started":   ToolEventStart,
		"completed": ToolEventComplete,
		"error":     ToolEventError,
		"failed":    ToolEventError,
		"":          ToolEventStart,
		"unknown":   ToolEventStart,
	}
	for status, want := range cases {
		if got := ToolEventKind(status); got != want {
			t.Fatalf("status %q kind = %q want %q", status, got, want)
		}
	}
}

func TestBusWireIncludesWorkflowContextSafely(t *testing.T) {
	caller := &recordingCaller{result: InvocationResult{Version: WireVersion, Status: "done", Response: "ok"}}
	client, _ := NewClient(caller, 0)
	_, err := client.Delegate(nil, DelegateRequest{Role: "code", Persona: "engineer", Prompt: "work", WorkItemID: "wi-1", Stage: "impl", Delegate: "codex", DurableSlot: "slot:analysis:1"})
	if err != nil {
		t.Fatal(err)
	}
	// Top-level forbidden fields must not leak, but workflow_context envelope must be present.
	var wire map[string]any
	if err := jsonUnmarshal(caller.request, &wire); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"work_item_id", "stage", "execution_version", "retry_tag", "durable_slot", "participant", "replay_only"} {
		if _, exists := wire[forbidden]; exists {
			t.Fatalf("forbidden top-level %q present", forbidden)
		}
	}
	wfRaw, ok := wire["workflow_context"]
	if !ok {
		t.Fatalf("workflow_context missing: %v", wire)
	}
	wf, _ := wfRaw.(map[string]any)
	if wf["work_item_id"] != "wi-1" || wf["stage"] != "impl" {
		t.Fatalf("workflow_context = %+v", wf)
	}
	if wf["phase"] != "analysis" {
		t.Fatalf("phase = %v want analysis", wf["phase"])
	}
	// Never include prompts or args.
	if _, ok := wf["prompt"]; ok {
		t.Fatal("prompt leaked into workflow_context")
	}
}

func jsonUnmarshal(data []byte, v any) error {
	// local helper to avoid import cycle.
	return json.Unmarshal(data, v)
}
