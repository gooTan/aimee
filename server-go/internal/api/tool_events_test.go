package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/JBailes/aimee/server-go/internal/db1"
)

func TestInternalModelEventsAcceptsToolKinds(t *testing.T) {
	server, store, _ := newTestServer(t)
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_tool_api", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ kind, detail string }{
		{"model_tool_start", "model=codex role=code persona=engineer tool=Bash call_id=c1 status=started"},
		{"model_tool_complete", "model=codex role=code persona=engineer tool=Bash call_id=c1 status=completed elapsed=100ms"},
		{"model_tool_error", "model=claude role=review persona=security tool=Read call_id=c2 status=error elapsed=5ms"},
	} {
		body := `{"work_item_id":"wi_tool_api","stage":"impl","kind":"` + tc.kind + `","actor":"codex","detail":"` + tc.detail + `"}`
		req := httptest.NewRequest(http.MethodPost, "/internal/model-events", strings.NewReader(body))
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("kind %q status=%d body=%s", tc.kind, rec.Code, rec.Body.String())
		}
	}
	events, _ := store.Events(t.Context(), "wi_tool_api", 0, 20)
	if len(events) != 4 { // create + 3 tool events
		t.Fatalf("events=%+v", events)
	}
}

func TestInternalModelEventsRejectsRawPayload(t *testing.T) {
	server, _, _ := newTestServer(t)
	// Detail must not contain raw tool args — but the endpoint enforces only
	// safe kinds and size, not content. The contract is that callers use
	// FormatToolDetail which never includes them. Verify that a raw detail
	// would be rejected only if it exceeds size or uses forbidden kind.
	body := `{"work_item_id":"wi_x","stage":"impl","kind":"model_tool_start","actor":"codex","detail":"` + strings.Repeat("x", 5000) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/model-events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized detail should be rejected: %d", rec.Code)
	}
}

func TestInternalModelEventsRejectsUnsafeToolDetailsFromDirectCallers(t *testing.T) {
	server, store, _ := newTestServer(t)
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_tool_unsafe", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{
		"model=codex tool=Bash call_id=c1 status=started command=cat_secret",
		"model=codex tool=Bash call_id=c1 status=started prompt=system_secret",
		"model=codex tool=Bash call_id=c1 status=started args=password=secret",
		"model=codex tool=Bash call_id=c1 status=started reasoning=hidden",
		"model=codex tool=Bash call_id=c1 status=started call_id=c2",
		"model=codex tool=Bash call_id=c1 status=started status=completed",
		"model=codex tool=Bash call_id=c1 status=started tool=Bash%20secret",
	} {
		body := `{"work_item_id":"wi_tool_unsafe","stage":"impl","kind":"model_tool_start","actor":"codex","detail":"` + detail + `"}`
		req := httptest.NewRequest(http.MethodPost, "/internal/model-events", strings.NewReader(body))
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unsafe detail %q status=%d body=%s", detail, rec.Code, rec.Body.String())
		}
	}
	events, err := store.Events(t.Context(), "wi_tool_unsafe", 0, 20)
	if err != nil || len(events) != 1 {
		t.Fatalf("unsafe details persisted: events=%+v err=%v", events, err)
	}
}

func TestInternalModelEventsCanonicalizesToolDetail(t *testing.T) {
	server, store, _ := newTestServer(t)
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_tool_canonical", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	body := `{"work_item_id":"wi_tool_canonical","stage":"impl","kind":"model_tool_complete","actor":"codex","detail":"status=completed call_id=c1 tool=Bash model=codex elapsed=100ms"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/model-events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("canonical detail status=%d body=%s", rec.Code, rec.Body.String())
	}
	events, err := store.Events(t.Context(), "wi_tool_canonical", 0, 20)
	if err != nil || len(events) != 2 || events[1].Detail != "model=codex tool=Bash call_id=c1 status=completed elapsed=100ms" {
		t.Fatalf("canonical detail events=%+v err=%v", events, err)
	}
}

func TestEventsTreeIncludesToolEventsForWFEWatch(t *testing.T) {
	_, store, _ := newTestServer(t)
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_watch", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "plan"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a delegate turn that emitted tool events.
	for _, ev := range []struct{ kind, detail string }{
		{"model_dispatch", "role=code persona=engineer tools=true status=running"},
		{"model_tool_start", "model=codex role=code persona=engineer tool=Bash call_id=c1 status=started"},
		{"model_tool_complete", "model=codex role=code persona=engineer tool=Bash call_id=c1 status=completed elapsed=80ms"},
		{"model_complete", "role=code persona=engineer tools=true status=complete elapsed=1s"},
	} {
		_ = store.RecordEvent(t.Context(), "wi_watch", "plan", ev.kind, "codex", ev.detail)
	}
	// GET /v1/workflow/items/:id/events is what `aimee workflow status --watch` polls.
	// Need to create work item with submitter=alice to pass auth; recreate with alice.
	store2, _ := db1.Open(filepath.Join(t.TempDir(), "aimee2.db"))
	defer store2.Close()
	_ = store2.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_watch2", Repo: "repo", ProposalPath: "p2", WorkflowName: "build", StartStage: "plan", Submitter: "alice"})
	for _, ev := range []struct{ kind, detail string }{
		{"model_tool_start", "model=codex role=code persona=engineer tool=Bash call_id=c1 status=started"},
	} {
		_ = store2.RecordEvent(t.Context(), "wi_watch2", "plan", ev.kind, "codex", ev.detail)
	}
	server2, _ := New(store2, nil)
	req2 := httptest.NewRequest(http.MethodGet, "/v1/workflow/items/wi_watch2/events?after=0&limit=200", nil)
	req2.Header.Set("X-Aimee-Webuser", "alice")
	rec := httptest.NewRecorder()
	server2.ServeHTTP(rec, req2)
	if rec.Code != http.StatusOK {
		t.Fatalf("events endpoint %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_tool_start") {
		t.Fatalf("tool event not in events tree: %s", rec.Body.String())
	}
}

func TestInternalModelEventsDeduplicatesLiveBatch(t *testing.T) {
	server, store, _ := newTestServer(t)
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_dedup", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	body := `{"work_item_id":"wi_dedup","stage":"impl","kind":"model_tool_start","actor":"codex","detail":"model=codex role=code persona=engineer tool=Bash call_id=c1 status=started"}`
	// First POST is the live emission (as the structured event is parsed).
	req1 := httptest.NewRequest(http.MethodPost, "/internal/model-events", strings.NewReader(body))
	rec1 := httptest.NewRecorder()
	server.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusNoContent {
		t.Fatalf("first live post %d %s", rec1.Code, rec1.Body.String())
	}
	// Second POST is the fallback batch for the same stable identity; it must not duplicate.
	req2 := httptest.NewRequest(http.MethodPost, "/internal/model-events", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	server.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("second batch post %d %s", rec2.Code, rec2.Body.String())
	}
	events, _ := store.Events(t.Context(), "wi_dedup", 0, 20)
	// create + exactly one tool event (no duplicate)
	if len(events) != 2 {
		t.Fatalf("dedup failed: events=%+v", events)
	}
	// A distinct call_id must not be deduped.
	body2 := `{"work_item_id":"wi_dedup","stage":"impl","kind":"model_tool_start","actor":"codex","detail":"model=codex role=code persona=engineer tool=Bash call_id=c2 status=started"}`
	req3 := httptest.NewRequest(http.MethodPost, "/internal/model-events", strings.NewReader(body2))
	rec3 := httptest.NewRecorder()
	server.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNoContent {
		t.Fatalf("distinct post %d %s", rec3.Code, rec3.Body.String())
	}
	events2, _ := store.Events(t.Context(), "wi_dedup", 0, 20)
	if len(events2) != 3 {
		t.Fatalf("distinct tool event should persist: %+v", events2)
	}
}

func TestInternalModelEventsDeduplicatesUnboundedConcurrentPosts(t *testing.T) {
	server, store, _ := newTestServer(t)
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi_dedup_large", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "impl"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1001; i++ {
		if err := store.RecordEvent(t.Context(), "wi_dedup_large", "impl", "model_tool_start", "codex", "model=codex tool=Bash call_id=old-"+strconv.Itoa(i)+" status=started"); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"work_item_id":"wi_dedup_large","stage":"impl","kind":"model_tool_start","actor":"codex","detail":"model=codex tool=Bash call_id=target status=started"}`
	const workers = 20
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/internal/model-events", strings.NewReader(body))
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Errorf("concurrent post status=%d body=%s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	events, err := store.Events(t.Context(), "wi_dedup_large", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if strings.Contains(event.Detail, "call_id=target") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("target event count = %d, want 1", count)
	}
}
