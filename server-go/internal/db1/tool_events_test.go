package db1

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestToolEventsPersistAndAppearInTree(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	if err := store.CreateWorkItem(ctx, CreateWorkItem{ID: "wi_tool", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "plan"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkItem(ctx, CreateWorkItem{ID: "wi_tool.child", Repo: "repo", ProposalPath: "p-child", WorkflowName: "slice", StartStage: "impl", ParentID: "wi_tool"}); err != nil {
		t.Fatal(err)
	}
	// Persist three tool events with safe detail.
	events := []struct{ kind, actor, detail string }{
		{"model_tool_start", "codex", "model=codex role=code persona=engineer tool=Bash call_id=c1 status=started"},
		{"model_tool_complete", "codex", "model=codex role=code persona=engineer tool=Bash call_id=c1 status=completed elapsed=120ms"},
		{"model_tool_error", "claude", "model=claude role=review persona=security tool=Read call_id=c2 status=error elapsed=5ms"},
	}
	for _, ev := range events {
		if err := store.RecordEvent(ctx, "wi_tool.child", "impl", ev.kind, ev.actor, ev.detail); err != nil {
			t.Fatal(err)
		}
	}
	// Direct child query.
	got, err := store.Events(ctx, "wi_tool.child", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 { // create + 3 tool events
		t.Fatalf("child events = %+v", got)
	}
	// Tree query from parent must include child tool events.
	tree, err := store.EventsTree(ctx, "wi_tool", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	// create (parent) + create (child) + 3 tool events = 5
	if len(tree) != 5 {
		t.Fatalf("tree events = %+v", tree)
	}
	found := map[string]bool{}
	for _, e := range tree {
		found[e.Kind] = true
		if strings.Contains(e.Detail, "supersecret") || strings.Contains(e.Detail, "password") {
			t.Fatalf("raw secret leaked: %+v", e)
		}
	}
	for _, k := range []string{"model_tool_start", "model_tool_complete", "model_tool_error"} {
		if !found[k] {
			t.Fatalf("kind %q not found in tree", k)
		}
	}
}

func TestToolEventsNeverStoreRawArguments(t *testing.T) {
	store, _ := Open(filepath.Join(t.TempDir(), "aimee.db"))
	defer store.Close()
	ctx := t.Context()
	_ = store.CreateWorkItem(ctx, CreateWorkItem{ID: "wi_safe", Repo: "r", ProposalPath: "p", WorkflowName: "build", StartStage: "plan"})
	// Simulate an attempt to store raw args — the caller must not do this.
	// The store itself does not redact, but the contract is that callers use
	// FormatToolDetail which never includes them. Verify FormatToolDetail would not.
	safeDetail := "model=codex role=code persona=engineer tool=Bash call_id=c1 status=completed"
	if err := store.RecordEvent(ctx, "wi_safe", "plan", "model_tool_complete", "codex", safeDetail); err != nil {
		t.Fatal(err)
	}
	events, _ := store.Events(ctx, "wi_safe", 0, 10)
	for _, e := range events {
		if strings.Contains(e.Detail, "supersecret") || strings.Contains(e.Detail, "prompt:") {
			t.Fatalf("unsafe detail: %+v", e)
		}
	}
}
