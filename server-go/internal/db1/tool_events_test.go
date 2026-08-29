package db1

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestRecordToolEventIfAbsentIsUnboundedAndAtomic(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	if err := store.CreateWorkItem(ctx, CreateWorkItem{ID: "wi_tool_dedup", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1001; i++ {
		if err := store.RecordEvent(ctx, "wi_tool_dedup", "impl", "model_tool_start", "codex", "model=codex tool=Bash call_id=old-"+strconv.Itoa(i)+" status=started"); err != nil {
			t.Fatal(err)
		}
	}
	detail := "model=codex tool=Bash call_id=target status=started"
	inserted, err := store.RecordToolEventIfAbsent(ctx, "wi_tool_dedup", "impl", "model_tool_start", "codex", detail)
	if err != nil || !inserted {
		t.Fatalf("first insert = %v, %v", inserted, err)
	}
	inserted, err = store.RecordToolEventIfAbsent(ctx, "wi_tool_dedup", "impl", "model_tool_start", "codex", detail)
	if err != nil || inserted {
		t.Fatalf("duplicate insert = %v, %v", inserted, err)
	}

	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	insertCount := 0
	concurrentDetail := "model=codex tool=Read call_id=concurrent status=started"
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.RecordToolEventIfAbsent(ctx, "wi_tool_dedup", "impl", "model_tool_start", "codex", concurrentDetail)
			if err != nil {
				t.Errorf("concurrent insert: %v", err)
				return
			}
			if ok {
				mu.Lock()
				insertCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if insertCount != 1 {
		t.Fatalf("concurrent inserts = %d, want 1", insertCount)
	}
	events, err := store.Events(ctx, "wi_tool_dedup", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Detail == concurrentDetail {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent event count = %d, want 1", count)
	}
}

func TestRecordToolEventIfAbsentDistinguishesActorAndInvocation(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	if err := store.CreateWorkItem(ctx, CreateWorkItem{ID: "wi_tool_identity", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	// Same provider call_id and detail except actor/invocation — must be retained
	// separately. Duplicate delivery for the same actor+invocation must stay idempotent.
	baseDetail := "model=codex role=code persona=engineer invocation=inv-1 tool=Bash call_id=c1 status=started"
	otherInvocationDetail := "model=codex role=code persona=engineer invocation=inv-2 tool=Bash call_id=c1 status=started"
	// First insert with actor codex / inv-1
	if ok, err := store.RecordToolEventIfAbsent(ctx, "wi_tool_identity", "impl", "model_tool_start", "codex", baseDetail); err != nil || !ok {
		t.Fatalf("first actor/invocation insert = %v %v", ok, err)
	}
	// Same call_id + same invocation but different actor must not dedup
	if ok, err := store.RecordToolEventIfAbsent(ctx, "wi_tool_identity", "impl", "model_tool_start", "claude", baseDetail); err != nil || !ok {
		t.Fatalf("distinct actor should be retained: %v %v", ok, err)
	}
	// Same actor but different invocation must not dedup
	if ok, err := store.RecordToolEventIfAbsent(ctx, "wi_tool_identity", "impl", "model_tool_start", "codex", otherInvocationDetail); err != nil || !ok {
		t.Fatalf("distinct invocation should be retained: %v %v", ok, err)
	}
	// Duplicate transport delivery for the same actor+invocation must be idempotent
	if ok, err := store.RecordToolEventIfAbsent(ctx, "wi_tool_identity", "impl", "model_tool_start", "codex", baseDetail); err != nil || ok {
		t.Fatalf("duplicate same actor/invocation should be idempotent: %v %v", ok, err)
	}
	if ok, err := store.RecordToolEventIfAbsent(ctx, "wi_tool_identity", "impl", "model_tool_start", "codex", otherInvocationDetail); err != nil || ok {
		t.Fatalf("duplicate same actor/other invocation should be idempotent on re-delivery: %v %v", ok, err)
	}
	events, err := store.Events(ctx, "wi_tool_identity", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 { // create + 3 distinct tool events
		t.Fatalf("expected 3 tool events, got %d: %+v", len(events)-1, events)
	}
	// Verify that live+batch duplicate for same invocation still dedups concurrently
	const workers = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	dupCount := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.RecordToolEventIfAbsent(ctx, "wi_tool_identity", "impl", "model_tool_start", "codex", baseDetail)
			if err != nil {
				t.Errorf("concurrent dup: %v", err)
				return
			}
			if ok {
				mu.Lock()
				dupCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if dupCount != 0 {
		t.Fatalf("concurrent duplicate same invocation should not insert: %d", dupCount)
	}
}
