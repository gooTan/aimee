package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/aimee/server-go/internal/db1"
	"github.com/JBailes/aimee/server-go/internal/wfe"
)

// gateTestServer builds a server whose registry holds a human-gated workflow
// and one item parked at that gate.
func gateTestServer(t *testing.T) (*Server, *db1.Store, *wfe.ArtifactStore, string) {
	t.Helper()
	root := t.TempDir()
	store, err := db1.Open(filepath.Join(root, "aimee.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := wfe.NewArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	workflowDir := filepath.Join(root, "workflows")
	if err := os.MkdirAll(workflowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := wfe.NewRegistry(workflowDir)
	if err != nil {
		t.Fatal(err)
	}
	definition := []byte(`name: gated
start: scope
nodes:
  - id: scope
    block: understand
    next: implement
  - id: implement
    block: implement
    in: {plan: scope.out}
    next: human_gate
    on_fail: implement
  - id: human_gate
    block: gate.human
    in: {src: implement.out}
    on_pass: deliver
    on_fail: implement
  - id: deliver
    block: gate.deliver
    in: {verdict: human_gate.out}
`)
	if _, err := registry.Save("gated", definition, ""); err != nil {
		t.Fatal(err)
	}
	pinned, err := registry.Pin("gated")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(store, artifacts, workflowDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := "wi_gate"
	if err := artifacts.PutProposal(id, []byte("Gate decision test\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkItem(ctx, db1.CreateWorkItem{
		ID: id, Repo: "repo", ProposalPath: "proposal:gate", WorkflowName: "gated",
		WorkflowVersion: pinned.Version, StartStage: "implement", Submitter: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Move(ctx, id, "implement", "human_gate", "advance", "", "hash1", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.ParkWithDetail(ctx, id, "human_gate", "human_gate", "approval required", 0); err != nil {
		t.Fatal(err)
	}
	return server, store, artifacts, id
}

func postGate(t *testing.T, server *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/workflow/items/"+id+"/gate", strings.NewReader(body))
	setWorkflowIdentity(req, "alice", true)
	server.ServeHTTP(rec, req)
	return rec
}

func TestGateRejectIsAlwaysTerminalEvenWithOnFail(t *testing.T) {
	server, store, _, id := gateTestServer(t)
	rec := postGate(t, server, id, `{"decision":"reject","gate":"human_gate"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	item, err := store.WorkItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "rejected" {
		t.Fatalf("reject left the run %q at stage %q; a rejection must end it", item.State, item.Stage)
	}
}

func TestGateChangesRoutesFindingsToOnFail(t *testing.T) {
	server, store, artifacts, id := gateTestServer(t)
	rec := postGate(t, server, id,
		`{"decision":"changes","gate":"human_gate","findings":[`+
			`{"location":"a.go:1","summary":"rename the constant","recommendation":"call it X"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	item, err := store.WorkItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "active" || item.Stage != "implement" || item.PauseReason != "" {
		t.Fatalf("changes did not route to on_fail: %+v", item)
	}
	feedback, err := artifacts.Feedback(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(feedback.Findings) != 1 || feedback.Findings[0].Persona != "human" ||
		feedback.Findings[0].Severity != "blocking" ||
		feedback.Findings[0].Summary != "rename the constant" {
		t.Fatalf("stored feedback = %+v", feedback.Findings)
	}
}

func TestGateChangesRequiresFindings(t *testing.T) {
	server, _, _, id := gateTestServer(t)
	rec := postGate(t, server, id, `{"decision":"changes","gate":"human_gate"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty changes decision returned %d body=%s", rec.Code, rec.Body.String())
	}
}
