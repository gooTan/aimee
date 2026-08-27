package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	delegateapi "github.com/JBailes/aimee/server-go/delegate"
	"github.com/JBailes/aimee/server-go/internal/db1"
)

type observableTestAgents struct{ fail bool }

func (a observableTestAgents) Delegate(context.Context, DelegateRequest) (DelegateResult, error) {
	if a.fail {
		return DelegateResult{AvailabilityClass: delegateapi.AvailabilityClassQuotaRateLimit}, errors.New("usage limit reached")
	}
	return DelegateResult{Agent: "fable"}, nil
}

type blockingObservableAgents struct{ release <-chan struct{} }

func (a blockingObservableAgents) Delegate(context.Context, DelegateRequest) (DelegateResult, error) {
	<-a.release
	return DelegateResult{Agent: "sol"}, nil
}

type groupedObservableAgents struct{}

func (groupedObservableAgents) Delegate(context.Context, DelegateRequest) (DelegateResult, error) {
	return DelegateResult{}, nil
}

func (groupedObservableAgents) DelegateGroup(_ context.Context, requests []DelegateRequest) []DelegateGroupResult {
	results := make([]DelegateGroupResult, len(requests))
	for i := range results {
		results[i] = DelegateGroupResult{Participant: "opaque-continuation-token"}
	}
	return results
}

func TestObservableAgentsRecordsModelOutcomeAndQuota(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
		kind string
	}{
		{name: "complete", kind: "model_complete"},
		{name: "quota", fail: true, kind: "model_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := db1.Open(filepath.Join(t.TempDir(), "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "plan"}); err != nil {
				t.Fatal(err)
			}
			agents := observableAgents{next: observableTestAgents{fail: tc.fail}, db: store}
			_, _ = agents.Delegate(t.Context(), DelegateRequest{WorkItemID: "wi", Stage: "plan", Delegate: "fable", Role: "draft", Persona: "planner"})
			events, err := store.Events(t.Context(), "wi", 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 3 || events[1].Kind != "model_dispatch" || events[2].Kind != tc.kind {
				t.Fatalf("events=%+v", events)
			}
			if tc.fail && !strings.Contains(events[2].Detail, "status=failed availability=quota_rate_limit error=usage limit reached") {
				t.Fatalf("quota detail=%q", events[2].Detail)
			}
		})
	}
}

func TestObservableDiagnosticIsBoundedAndKeepsFailureTail(t *testing.T) {
	detail := observableDiagnostic(strings.Repeat("startup event ", 2_000) + "You've hit your session limit")
	if len(detail) > maxObservableDiagnosticBytes {
		t.Fatalf("diagnostic length=%d, want <= %d", len(detail), maxObservableDiagnosticBytes)
	}
	if !strings.Contains(detail, "[truncated]") || !strings.HasSuffix(detail, "You've hit your session limit") {
		t.Fatalf("diagnostic=%q", detail)
	}
}

func TestObservableAgentsRecordsHeartbeatWhileModelRuns(t *testing.T) {
	store, err := db1.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "review"}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	agents := observableAgents{next: blockingObservableAgents{release: release}, db: store, heartbeatEvery: time.Millisecond}
	done := make(chan struct{})
	go func() {
		_, _ = agents.Delegate(t.Context(), DelegateRequest{WorkItemID: "wi", Stage: "review", Delegate: "sol", Role: "review", Persona: "reviewer"})
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	close(release)
	<-done
	events, err := store.Events(t.Context(), "wi", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || event.Kind == "model_heartbeat" && event.Actor == "sol"
	}
	if !found {
		t.Fatalf("heartbeat missing: %+v", events)
	}
	if events[len(events)-1].Kind != "model_complete" {
		t.Fatalf("heartbeat recorded after completion: %+v", events)
	}
}

func TestObservableGroupedCompletionUsesModelNotParticipant(t *testing.T) {
	store, err := db1.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "gate"}); err != nil {
		t.Fatal(err)
	}
	agents := observableAgents{next: groupedObservableAgents{}, db: store}
	agents.DelegateGroup(t.Context(), []DelegateRequest{{WorkItemID: "wi", Stage: "gate", Delegate: "sol", Role: "review", Persona: "qa"}})
	events, err := store.Events(t.Context(), "wi", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Kind != "model_complete" || events[len(events)-1].Actor != "sol" {
		t.Fatalf("events=%+v", events)
	}
}

func TestObservableGroupedUnsupportedIsVisible(t *testing.T) {
	store, err := db1.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "gate"}); err != nil {
		t.Fatal(err)
	}
	agents := observableAgents{next: observableTestAgents{}, db: store}
	results := agents.DelegateGroup(t.Context(), []DelegateRequest{{WorkItemID: "wi", Stage: "gate", Delegate: "sol", Role: "review", Persona: "qa"}})
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results=%+v", results)
	}
	events, err := store.Events(t.Context(), "wi", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Kind != "model_error" || events[len(events)-1].Actor != "sol" {
		t.Fatalf("events=%+v", events)
	}
}
