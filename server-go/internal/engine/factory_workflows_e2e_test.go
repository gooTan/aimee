package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JBailes/aimee/server-go/internal/db1"
	"github.com/JBailes/aimee/server-go/internal/wfe"
)

// factoryPremiumPolicy is the shipped subscription-factory budget: sol and
// fable are premium, two dispatches per run tree.
func factoryPremiumPolicy() PremiumPolicy {
	return PremiumPolicy{Delegates: map[string]bool{"sol": true, "fable": true}, MaxCalls: 2}
}

const factoryValidBrief = `{"schema_version":1,"summary":"add the feature","files":["feature.txt"],` +
	`"interfaces":["none"],"constraints":["stay small"],"decisions":["none"],"risks":["low"],` +
	`"open_questions":[],"acceptance_criteria":["feature exists"],"artifacts":[]}`

// factoryAgents scripts every delegate seat of the factory workflows and
// records each dispatch so tests can assert exactly which delegates ran.
type factoryAgents struct {
	mu       sync.Mutex
	requests []DelegateRequest
	codeRuns int
	// briefResponse overrides the ContextBrief Luna returns; empty means valid.
	briefResponse string
	// lunaReviews and solReviews are consumed per review call by delegate pin;
	// exhausted scripts approve.
	lunaReviews []string
	solReviews  []string
}

func (a *factoryAgents) recorded() []DelegateRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]DelegateRequest(nil), a.requests...)
}

func (a *factoryAgents) delegateCalls(name string) int {
	count := 0
	for _, request := range a.recorded() {
		if request.Delegate == name {
			count++
		}
	}
	return count
}

func reviewApprove() string {
	return `{"verdict":"approve","findings":[]}`
}

func reviewChanges(escalation string) string {
	response := `{"verdict":"changes","findings":[{"id":"f1","severity":"blocking",` +
		`"summary":"needs work","recommendation":"fix it"}]`
	if escalation != "" {
		response += `,"escalation":"` + escalation + `"`
	}
	return response + `}`
}

func (a *factoryAgents) Delegate(_ context.Context, request DelegateRequest) (DelegateResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	switch request.Role {
	case "draft":
		if strings.Contains(request.Prompt, "Prepare a concise ContextBrief") {
			a.mu.Lock()
			brief := a.briefResponse
			a.mu.Unlock()
			if brief == "" {
				brief = factoryValidBrief
			}
			return DelegateResult{Response: brief}, nil
		}
		if strings.Contains(request.Prompt, "Author a complete implementation plan") {
			return DelegateResult{Response: "1. Implement the feature.\n2. Verify it.\n"}, nil
		}
		if strings.Contains(request.Prompt, "Scope the engineering task") {
			return DelegateResult{Response: `{"schema_version":1,"status":"unconfirmed","summary":"implement feature","rationale":"proposal","acceptance_criteria":["feature exists"]}`}, nil
		}
		return DelegateResult{Response: "1. Implement feature.\n"}, nil
	case "review":
		a.mu.Lock()
		var script *[]string
		if request.Delegate == "sol" {
			script = &a.solReviews
		} else {
			script = &a.lunaReviews
		}
		response := reviewApprove()
		if len(*script) > 0 {
			response = (*script)[0]
			*script = (*script)[1:]
		}
		a.mu.Unlock()
		return DelegateResult{Response: response}, nil
	case "code":
		a.mu.Lock()
		a.codeRuns++
		n := a.codeRuns
		a.mu.Unlock()
		path := filepath.Join(request.Workdir, "feature.txt")
		if err := os.WriteFile(path, []byte(fmt.Sprintf("run %d\n", n)), 0o600); err != nil {
			return DelegateResult{}, err
		}
		return DelegateResult{Response: "done"}, nil
	default:
		return DelegateResult{}, fmt.Errorf("unexpected role %s", request.Role)
	}
}

func (a *factoryAgents) DelegateGroup(ctx context.Context, requests []DelegateRequest) []DelegateGroupResult {
	return testDelegateGroup(ctx, requests, a.Delegate)
}

// failNVerifier fails its first n verifications with a deterministic
// diagnostic and passes afterwards.
type failNVerifier struct {
	mu       sync.Mutex
	failures int
	calls    int
}

func (v *failNVerifier) Verify(context.Context, string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.calls <= v.failures {
		return errors.New("verify failed: tests/feature_test exited 1")
	}
	return nil
}

type factoryRun struct {
	store     *db1.Store
	artifacts *wfe.ArtifactStore
	forge     *e2eForge
	agents    *factoryAgents
	id        string
	cancel    context.CancelFunc
	done      chan struct{}
}

func (r *factoryRun) shutdown() {
	r.cancel()
	<-r.done
}

func startFactoryWorkflow(t *testing.T, workflowName string, agents *factoryAgents,
	verifier Verifier) *factoryRun {
	t.Helper()
	root := t.TempDir()
	repo := newShippedWorkflowRepo(t, root)
	workflowDir := copyShippedWorkflowDefinitions(t)
	registry, err := wfe.NewRegistry(workflowDir)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := registry.Pin(workflowName)
	if err != nil {
		t.Fatal(err)
	}
	store, err := db1.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	artifacts, err := wfe.NewArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	id := "wi_factory_" + strings.ReplaceAll(workflowName, "-", "_")
	if err := artifacts.PutProposal(id, []byte("Implement the test feature.\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkItem(t.Context(), db1.CreateWorkItem{
		ID: id, Repo: repo, ProposalPath: "proposal:" + workflowName,
		WorkflowName: workflowName, WorkflowVersion: definition.Version,
		StartStage: definition.Start,
	}); err != nil {
		t.Fatal(err)
	}
	worktrees, err := NewWorktreeManager(store, filepath.Join(root, "trees"))
	if err != nil {
		t.Fatal(err)
	}
	if verifier == nil {
		verifier = passVerifier{}
	}
	forge := &e2eForge{}
	runner, err := NewNativeRunner(store, worktrees, agents, verifier, artifacts, registry, forge)
	if err != nil {
		t.Fatal(err)
	}
	runner.SetPremiumPolicy(factoryPremiumPolicy())
	engine, err := New(store, artifacts, workflowDir, runner)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, engine, 2, nil)
	scheduler.pollEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { scheduler.Run(ctx); close(done) }()
	run := &factoryRun{store: store, artifacts: artifacts, forge: forge, agents: agents,
		id: id, cancel: cancel, done: done}
	t.Cleanup(run.shutdown)
	return run
}

func (r *factoryRun) waitFor(t *testing.T, wantState, wantPause string) db1.WorkItem {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		item, err := r.store.WorkItem(context.Background(), r.id)
		if err != nil {
			t.Fatal(err)
		}
		if item.State == wantState && item.PauseReason == wantPause &&
			(item.State != "active" || item.PauseReason != "") {
			return item
		}
		if item.State != "active" && item.State != wantState {
			events, _ := r.store.Events(context.Background(), r.id, 0, 1000)
			t.Fatalf("workflow ended unexpectedly: item=%+v events=%+v", item, events)
		}
		if item.PauseReason != "" && item.PauseReason != wantPause {
			events, _ := r.store.Events(context.Background(), r.id, 0, 1000)
			t.Fatalf("workflow parked unexpectedly: item=%+v events=%+v", item, events)
		}
		time.Sleep(10 * time.Millisecond)
	}
	item, _ := r.store.WorkItem(context.Background(), r.id)
	events, _ := r.store.Events(context.Background(), r.id, 0, 1000)
	t.Fatalf("workflow timed out waiting for state=%q pause=%q: item=%+v events=%+v",
		wantState, wantPause, item, events)
	return db1.WorkItem{}
}

// approveHumanGate resolves the parked human gate exactly the way the API
// handler does: write the approval artifact, then ResolveGate under the SQL
// guard that only releases pause_reason='human_gate'.
func (r *factoryRun) approveHumanGate(t *testing.T, item db1.WorkItem) {
	t.Helper()
	artifact, err := r.artifacts.PutNodeArtifact(r.id, "human_gate", "approval", []byte("approved"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.ResolveGate(context.Background(), r.id, "human_gate", "deliver",
		"approve", artifact.Hash); err != nil {
		t.Fatal(err)
	}
	_ = item
}

func (r *factoryRun) premiumCalls(t *testing.T) int {
	t.Helper()
	count, err := r.store.PremiumCallCount(context.Background(), r.id)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func assertNoPremiumWrites(t *testing.T, requests []DelegateRequest) {
	t.Helper()
	for _, request := range requests {
		if request.Delegate != "sol" && request.Delegate != "fable" {
			continue
		}
		if request.Tools || request.Role == "code" {
			t.Fatalf("premium delegate %q was dispatched write-capable: %+v", request.Delegate, request)
		}
	}
}

func TestQuickChangeUsesZeroPremiumCalls(t *testing.T) {
	agents := &factoryAgents{}
	run := startFactoryWorkflow(t, "quick-change", agents, nil)
	item := run.waitFor(t, "active", "human_gate")
	if got := run.premiumCalls(t); got != 0 {
		t.Fatalf("quick-change recorded %d premium calls, want 0", got)
	}
	for _, name := range []string{"sol", "fable"} {
		if calls := agents.delegateCalls(name); calls != 0 {
			t.Fatalf("quick-change dispatched premium delegate %s %d times", name, calls)
		}
	}
	if calls := agents.delegateCalls("deepseek"); calls != 1 {
		t.Fatalf("deepseek implemented %d times, want 1", calls)
	}
	if calls := agents.delegateCalls("luna"); calls != 2 {
		t.Fatalf("luna prepared+reviewed %d times, want 2", calls)
	}
	assertNoPremiumWrites(t, agents.recorded())
	run.approveHumanGate(t, item)
	run.waitFor(t, "accepted", "")
	if got := run.premiumCalls(t); got != 0 {
		t.Fatalf("quick-change finished with %d premium calls, want 0", got)
	}
}

func TestOrchestratedChangeHappyPathUsesExactlyOnePremiumCall(t *testing.T) {
	agents := &factoryAgents{}
	run := startFactoryWorkflow(t, "orchestrated-change", agents, nil)
	item := run.waitFor(t, "active", "human_gate")
	if got := run.premiumCalls(t); got != 1 {
		t.Fatalf("orchestrated-change recorded %d premium calls, want exactly 1", got)
	}
	if calls := agents.delegateCalls("fable"); calls != 1 {
		t.Fatalf("fable planned %d times, want exactly 1", calls)
	}
	if calls := agents.delegateCalls("sol"); calls != 0 {
		t.Fatalf("sol was dispatched %d times on the happy path, want 0", calls)
	}
	assertNoPremiumWrites(t, agents.recorded())
	// The premium planner must receive the brief and the request, never raw
	// repository content.
	for _, request := range agents.recorded() {
		if request.Delegate != "fable" {
			continue
		}
		if !strings.Contains(request.Prompt, "CONTEXT BRIEF") {
			t.Fatalf("premium planning prompt lacks the ContextBrief section:\n%s", request.Prompt)
		}
	}
	run.forge.mu.Lock()
	opens := append([]PullRequestSpec(nil), run.forge.opens...)
	run.forge.mu.Unlock()
	if len(opens) != 1 || !opens[0].Draft {
		t.Fatalf("want exactly one draft PR before the human gate, got %+v", opens)
	}
	run.approveHumanGate(t, item)
	run.waitFor(t, "accepted", "")
	if got := run.premiumCalls(t); got != 1 {
		t.Fatalf("orchestrated-change finished with %d premium calls, want exactly 1", got)
	}
}

func TestRoutineVerifierFailureRepairsWithoutPremium(t *testing.T) {
	agents := &factoryAgents{}
	run := startFactoryWorkflow(t, "orchestrated-change", agents, &failNVerifier{failures: 1})
	run.waitFor(t, "active", "human_gate")
	if got := run.premiumCalls(t); got != 1 {
		t.Fatalf("verifier failure changed premium spend: %d calls, want 1", got)
	}
	if calls := agents.delegateCalls("sol"); calls != 0 {
		t.Fatalf("routine verifier failure dispatched sol %d times", calls)
	}
	if calls := agents.delegateCalls("deepseek"); calls < 2 {
		t.Fatalf("deepseek repaired %d times, want at least 2 (initial + repair)", calls)
	}
}

func TestRoutineReviewFindingsReturnToImplementerWithoutPremium(t *testing.T) {
	agents := &factoryAgents{lunaReviews: []string{reviewChanges("")}}
	run := startFactoryWorkflow(t, "orchestrated-change", agents, nil)
	run.waitFor(t, "active", "human_gate")
	if got := run.premiumCalls(t); got != 1 {
		t.Fatalf("routine review finding changed premium spend: %d calls, want 1", got)
	}
	if calls := agents.delegateCalls("sol"); calls != 0 {
		t.Fatalf("routine review finding dispatched sol %d times", calls)
	}
	if calls := agents.delegateCalls("deepseek"); calls < 2 {
		t.Fatalf("deepseek repaired %d times, want at least 2", calls)
	}
}

func TestEscalationInvokesSecondPremiumOpinionOnce(t *testing.T) {
	agents := &factoryAgents{lunaReviews: []string{reviewChanges("architecture")}}
	run := startFactoryWorkflow(t, "orchestrated-change", agents, nil)
	item := run.waitFor(t, "active", "human_gate")
	if got := run.premiumCalls(t); got != 2 {
		t.Fatalf("escalated run recorded %d premium calls, want 2 (fable plan + sol opinion)", got)
	}
	if calls := agents.delegateCalls("sol"); calls != 1 {
		t.Fatalf("sol was dispatched %d times, want exactly 1", calls)
	}
	assertNoPremiumWrites(t, agents.recorded())
	run.approveHumanGate(t, item)
	run.waitFor(t, "accepted", "")
}

func TestUnknownEscalationClassStaysOnRoutinePath(t *testing.T) {
	agents := &factoryAgents{lunaReviews: []string{reviewChanges("vibes")}}
	run := startFactoryWorkflow(t, "orchestrated-change", agents, nil)
	run.waitFor(t, "active", "human_gate")
	if calls := agents.delegateCalls("sol"); calls != 0 {
		t.Fatalf("unknown escalation class bought %d sol dispatches, want 0", calls)
	}
	if got := run.premiumCalls(t); got != 1 {
		t.Fatalf("unknown escalation class changed premium spend: %d calls, want 1", got)
	}
}

func TestPremiumCallsCannotExceedCap(t *testing.T) {
	agents := &factoryAgents{
		lunaReviews: []string{reviewChanges("architecture"), reviewChanges("security")},
		solReviews:  []string{reviewChanges("")},
	}
	run := startFactoryWorkflow(t, "orchestrated-change", agents, nil)
	item := run.waitFor(t, "active", "premium_cap")
	if got := run.premiumCalls(t); got != 2 {
		t.Fatalf("capped run recorded %d premium calls, want exactly 2", got)
	}
	if calls := agents.delegateCalls("sol"); calls != 1 {
		t.Fatalf("sol executed %d times, want 1 (second attempt must be refused pre-dispatch)", calls)
	}
	if item.Stage != "second_opinion" {
		t.Fatalf("premium_cap parked at stage %q, want second_opinion", item.Stage)
	}
}

func TestMalformedBriefCannotReachPremium(t *testing.T) {
	agents := &factoryAgents{briefResponse: "this is not a context brief"}
	run := startFactoryWorkflow(t, "orchestrated-change", agents, nil)
	run.waitFor(t, "active", "retry_limit")
	if got := run.premiumCalls(t); got != 0 {
		t.Fatalf("malformed brief still recorded %d premium calls", got)
	}
	if calls := agents.delegateCalls("fable"); calls != 0 {
		t.Fatalf("malformed brief still dispatched fable %d times", calls)
	}
}

func TestHumanGateBlocksAutomatedDelivery(t *testing.T) {
	agents := &factoryAgents{}
	run := startFactoryWorkflow(t, "quick-change", agents, nil)
	run.waitFor(t, "active", "human_gate")
	// Let the scheduler keep polling: the parked gate must hold.
	time.Sleep(300 * time.Millisecond)
	item, err := run.store.WorkItem(context.Background(), run.id)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "active" || item.PauseReason != "human_gate" {
		t.Fatalf("human gate did not hold: %+v", item)
	}
	// A manual resume without a gate decision is refused outright: the pause is
	// lifecycle-owned and only ResolveGate under its SQL guard can clear it.
	err = run.store.Resume(context.Background(), run.id)
	if err == nil || !strings.Contains(err.Error(), "human_gate") {
		t.Fatalf("manual resume of a human gate returned %v, want a lifecycle-owned refusal", err)
	}
	item, err = run.store.WorkItem(context.Background(), run.id)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "active" || item.PauseReason != "human_gate" || item.Stage != "human_gate" {
		t.Fatalf("refused resume still moved the item: %+v", item)
	}
}

func TestPremiumDelegateCannotBeDispatchedWithWriteTools(t *testing.T) {
	root := t.TempDir()
	store, err := db1.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	artifacts, err := wfe.NewArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := wfe.NewRegistry(filepath.Join(root, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	worktrees, err := NewWorktreeManager(store, filepath.Join(root, "trees"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewNativeRunner(store, worktrees, &factoryAgents{}, passVerifier{}, artifacts, registry, &e2eForge{})
	if err != nil {
		t.Fatal(err)
	}
	runner.SetPremiumPolicy(factoryPremiumPolicy())
	step := StepRequest{WorkItem: db1.WorkItem{ID: "wi_premium_write"}, Node: wfe.Node{ID: "implement"}}
	_, err = runner.delegate(context.Background(), step, DelegateRequest{
		Role: "code", Persona: "engineer", Delegate: "fable", Prompt: "write code", Tools: true})
	if !errors.Is(err, ErrPremiumWriteRefused) {
		t.Fatalf("write-capable premium dispatch returned %v, want ErrPremiumWriteRefused", err)
	}
	count, err := store.PremiumCallCount(context.Background(), "wi_premium_write")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("refused write dispatch still recorded %d premium calls", count)
	}
}
