package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/aimee/server-go/delegate"
	"github.com/JBailes/aimee/server-go/internal/db1"
	roundtablecfg "github.com/JBailes/aimee/server-go/modules/roundtable/panel"
)

type recordingPlaneAgents struct {
	requests     []DelegateRequest
	err          error
	availability delegate.AvailabilityClass
}

func (a *recordingPlaneAgents) Delegate(_ context.Context, request DelegateRequest) (DelegateResult, error) {
	a.requests = append(a.requests, request)
	return DelegateResult{Response: "ok", Participant: "p", AvailabilityClass: a.availability}, a.err
}

func (a *recordingPlaneAgents) DelegateGroup(_ context.Context, requests []DelegateRequest) []DelegateGroupResult {
	a.requests = append(a.requests, requests...)
	out := make([]DelegateGroupResult, len(requests))
	for i := range out {
		out[i] = DelegateGroupResult{Response: "ok", Participant: "p", AvailabilityClass: a.availability, Err: a.err}
	}
	return out
}

func testSeatRun() roundtablecfg.Run {
	return roundtablecfg.Run{ID: "wi", Stage: "gate", ExecutionVersion: "v1",
		Workdir: "/worktree", CostLimitUSD: 6}
}

// The seat prompt already carries the complete artifact under review, so the
// plane must be told not to attach unrelated worktree-diff evidence that would
// compete with it. The panel cannot assert this itself: which evidence a
// delegate receives is this adapter's decision.
func TestPanelDelegatesAlwaysDeclareTheInlineArtifact(t *testing.T) {
	agents := &recordingPlaneAgents{}
	plane := panelDelegates{runner: &NativeRunner{agents: agents}}
	seat := roundtablecfg.SeatRequest{Role: "review", Persona: "qa", Prompt: "review", Tools: true}
	plane.Group(context.Background(), testSeatRun(), []roundtablecfg.SeatRequest{seat})
	plane.One(context.Background(), testSeatRun(), seat)
	if len(agents.requests) != 2 {
		t.Fatalf("requests=%d", len(agents.requests))
	}
	for _, request := range agents.requests {
		if !request.ProvidedTarget {
			t.Fatalf("seat did not declare its inline artifact: %+v", request)
		}
		if request.WorkItemID != "wi" || request.Stage != "gate" || request.ExecutionVersion != "v1" || request.Workdir != "/worktree" {
			t.Fatalf("run identity was not carried onto the seat: %+v", request)
		}
	}
}

// Concurrent seats share one reservation, so their individual ceilings must sum
// to no more than it. Giving each the whole limit would let a three-seat panel
// spend three times what the caller reserved.
func TestPanelDelegatesDivideTheReservationAcrossConcurrentSeats(t *testing.T) {
	agents := &recordingPlaneAgents{}
	plane := panelDelegates{runner: &NativeRunner{agents: agents}}
	seats := []roundtablecfg.SeatRequest{{Persona: "a"}, {Persona: "b"}, {Persona: "c"}}
	plane.Group(context.Background(), testSeatRun(), seats)
	var total float64
	for _, request := range agents.requests {
		total += request.MaxCostUSD
	}
	if total > 6 {
		t.Fatalf("three seats were allowed %v against a reservation of 6", total)
	}
}

// The panel never parses a transport error, so an unclassified failure would
// reach it as an anonymous one and be reported as "malformed_after_repair" --
// blaming the reviewer for the plane's outage.
func TestPanelDelegatesClassifyAndRedactTheirFailures(t *testing.T) {
	agents := &recordingPlaneAgents{err: ErrDelegateReplayUnavailable}
	plane := panelDelegates{runner: &NativeRunner{agents: agents}}
	result := plane.Group(context.Background(), testSeatRun(), []roundtablecfg.SeatRequest{{Persona: "qa"}})[0]
	if result.FailureCategory != "replay_unavailable" || !result.ReplayLost {
		t.Fatalf("replay failure was not classified: %+v", result)
	}
	if result.FailureDetail == "" {
		t.Fatal("failure reached the panel with no detail to report")
	}
}

func TestPanelDelegatesForwardAvailabilityClass(t *testing.T) {
	err := &delegate.DelegateExecutionError{Err: errors.New("provider unavailable"), AvailabilityClass: delegate.AvailabilityClassProviderUnavailable}
	agents := &recordingPlaneAgents{err: err, availability: delegate.AvailabilityClassProviderUnavailable}
	plane := panelDelegates{runner: &NativeRunner{agents: agents}}
	if got := plane.Group(context.Background(), testSeatRun(), []roundtablecfg.SeatRequest{{Persona: "qa"}})[0].AvailabilityClass; got != delegate.AvailabilityClassProviderUnavailable {
		t.Fatalf("group lost availability class: %q", got)
	}
	if got := plane.One(context.Background(), testSeatRun(), roundtablecfg.SeatRequest{Persona: "qa"}).AvailabilityClass; got != delegate.AvailabilityClassProviderUnavailable {
		t.Fatalf("single lost availability class: %q", got)
	}
}

func TestPanelDelegatesRecordChairmanFallback(t *testing.T) {
	store, err := db1.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{ID: "wi", Repo: "repo", ProposalPath: "p", WorkflowName: "build", StartStage: "gate"}); err != nil {
		t.Fatal(err)
	}
	agents := &recordingPlaneAgents{}
	plane := panelDelegates{runner: &NativeRunner{agents: agents, db: store}}
	plane.One(t.Context(), testSeatRun(), roundtablecfg.SeatRequest{Selector: "sol", Persona: "chairman", FallbackFrom: "fable", FallbackReason: "quota_rate_limit"})
	events, err := store.Events(t.Context(), "wi", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Kind != "model_fallback" || events[1].Actor != "fable" || events[1].Detail != "to=sol reason=quota_rate_limit" {
		t.Fatalf("fallback event=%+v", events)
	}
}

func TestPanelDelegatesRedactCredentialsFromSeatFailures(t *testing.T) {
	agents := &recordingPlaneAgents{err: errors.New(`launch failed: authorization: bearer sk-live-secret-value`)}
	plane := panelDelegates{runner: &NativeRunner{agents: agents}}
	result := plane.Group(context.Background(), testSeatRun(), []roundtablecfg.SeatRequest{{Persona: "qa"}})[0]
	if result.FailureDetail == "" || strings.Contains(result.FailureDetail, "sk-live-secret-value") {
		t.Fatalf("credential survived into a durable seat failure: %q", result.FailureDetail)
	}
}

// A plane that cannot run seats concurrently under one reservation cannot host
// a roundtable, and must say so on every seat rather than silently serialising.
type ungroupedAgents struct{}

func (ungroupedAgents) Delegate(context.Context, DelegateRequest) (DelegateResult, error) {
	return DelegateResult{}, nil
}

func TestPanelDelegatesFailEverySeatWithoutGroupedDelegation(t *testing.T) {
	plane := panelDelegates{runner: &NativeRunner{agents: ungroupedAgents{}}}
	results := plane.Group(context.Background(), testSeatRun(),
		[]roundtablecfg.SeatRequest{{Persona: "a"}, {Persona: "b"}})
	for _, result := range results {
		if result.Err == nil {
			t.Fatalf("seat succeeded on a plane without grouped delegation: %+v", result)
		}
	}
}
