package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	delegateapi "github.com/JBailes/aimee/server-go/delegate"
	"github.com/JBailes/aimee/server-go/internal/db1"
)

type toolTestAgents struct {
	events []delegateapi.ToolEvent
	agent  string
}

func (a toolTestAgents) Delegate(context.Context, DelegateRequest) (DelegateResult, error) {
	return DelegateResult{Agent: a.agent, ToolEvents: a.events}, nil
}

func (a toolTestAgents) DelegateGroup(_ context.Context, reqs []DelegateRequest) []DelegateGroupResult {
	results := make([]DelegateGroupResult, len(reqs))
	for i := range reqs {
		results[i] = DelegateGroupResult{Participant: a.agent, ToolEvents: a.events}
	}
	return results
}

func TestObservableAgentsPersistsToolEventsWithSafeDetail(t *testing.T) {
	store, err := db1.Open(filepath.Join(t.TempDir(), "aimee.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_tool_obs", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	events := []delegateapi.ToolEvent{
		{ToolName: "Bash", CallID: "call-1", Status: "started"},
		{ToolName: "Bash", CallID: "call-1", Status: "completed", ElapsedMS: 200},
		{ToolName: "Read", CallID: "call-2", Status: "error", ElapsedMS: 50},
	}
	agents := observableAgents{next: toolTestAgents{events: events, agent: "codex"}, db: store}
	_, _ = agents.Delegate(t.Context(), DelegateRequest{WorkItemID: "wi_tool_obs", Stage: "impl", Delegate: "codex", Role: "code", Persona: "engineer", Tools: true})
	evs, err := store.Events(t.Context(), "wi_tool_obs", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	// create + dispatch + 3 tool events + complete = at least 5
	if len(evs) < 5 {
		t.Fatalf("events = %+v", evs)
	}
	// Tool events must be in causal order: started before completed.
	var startedIdx, completedIdx, errorIdx = -1, -1, -1
	for i, e := range evs {
		if e.Kind == "model_tool_start" && strings.Contains(e.Detail, "tool=Bash") && strings.Contains(e.Detail, "call_id=call-1") {
			startedIdx = i
		}
		if e.Kind == "model_tool_complete" && strings.Contains(e.Detail, "call_id=call-1") {
			completedIdx = i
		}
		if e.Kind == "model_tool_error" && strings.Contains(e.Detail, "tool=Read") {
			errorIdx = i
		}
		// Never persist raw prompt/args.
		if strings.Contains(e.Detail, "supersecret") || strings.Contains(e.Detail, "prompt") {
			t.Fatalf("unsafe detail: %+v", e)
		}
		if e.Kind == "model_tool_start" || e.Kind == "model_tool_complete" || e.Kind == "model_tool_error" {
			if !strings.Contains(e.Detail, "model=codex") || !strings.Contains(e.Detail, "role=code") || !strings.Contains(e.Detail, "persona=engineer") {
				t.Fatalf("tool detail missing workflow identity: %+v", e)
			}
			if e.Stage != "impl" || e.WorkItemID != "wi_tool_obs" {
				t.Fatalf("tool event not associated with work item: %+v", e)
			}
		}
	}
	if startedIdx < 0 || completedIdx < 0 || errorIdx < 0 {
		t.Fatalf("missing tool events: started=%d completed=%d error=%d evs=%+v", startedIdx, completedIdx, errorIdx, evs)
	}
	if startedIdx > completedIdx {
		t.Fatal("started should precede completed")
	}
}

func TestObservableAgentsGroupPersistsToolEventsWithPhase(t *testing.T) {
	store, err := db1.Open(filepath.Join(t.TempDir(), "aimee.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_group", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "gate"}); err != nil {
		t.Fatal(err)
	}
	events := []delegateapi.ToolEvent{{ToolName: "Grep", CallID: "g1", Status: "started"}}
	agents := observableAgents{next: toolTestAgents{events: events, agent: "sol"}, db: store, heartbeatEvery: time.Millisecond}
	reqs := []DelegateRequest{
		{WorkItemID: "wi_group", Stage: "gate", Delegate: "sol", Role: "review", Persona: "security", DurableSlot: "run:analysis:0", Tools: true},
		{WorkItemID: "wi_group", Stage: "gate", Delegate: "sol", Role: "review", Persona: "qa", DurableSlot: "run:discussion:0", Tools: true},
	}
	agents.DelegateGroup(t.Context(), reqs)
	evs, _ := store.Events(t.Context(), "wi_group", 0, 30)
	phases := map[string]bool{}
	for _, e := range evs {
		if e.Kind == "model_tool_start" {
			if strings.Contains(e.Detail, "phase=analysis") {
				phases["analysis"] = true
			}
			if strings.Contains(e.Detail, "phase=discussion") {
				phases["discussion"] = true
			}
		}
	}
	if !phases["analysis"] || !phases["discussion"] {
		t.Fatalf("phase not propagated: %+v", evs)
	}
}

func TestObservableAgentsDoesNotPersistRawToolArguments(t *testing.T) {
	// Even if a malicious tool event tried to smuggle raw args in ToolName, the
	// sanitizer truncates and the detail never includes args/results.
	ev := delegateapi.ToolEvent{ToolName: "Bash`echo supersecret`", CallID: "c1", Status: "started"}
	store, _ := db1.Open(filepath.Join(t.TempDir(), "aimee.db"))
	defer store.Close()
	_ = store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_safe2", Repo: "r", ProposalPath: "p", WorkflowName: "build", StartStage: "plan"})
	agents := observableAgents{next: toolTestAgents{events: []delegateapi.ToolEvent{ev}, agent: "codex"}, db: store}
	_, _ = agents.Delegate(t.Context(), DelegateRequest{WorkItemID: "wi_safe2", Stage: "plan", Delegate: "codex", Role: "code", Persona: "engineer"})
	evs, _ := store.Events(t.Context(), "wi_safe2", 0, 10)
	for _, e := range evs {
		if strings.Contains(e.Detail, "supersecret") {
			t.Fatalf("secret leaked: %q", e.Detail)
		}
	}
}
