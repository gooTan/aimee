package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/aimee/server-go/internal/db1"
	"github.com/JBailes/aimee/server-go/internal/wfe"
)

func TestPremiumPolicyFromEnvDefaults(t *testing.T) {
	t.Setenv("AIMEE_PREMIUM_DELEGATES", "")
	t.Setenv("AIMEE_PREMIUM_CALL_CAP", "")
	policy := PremiumPolicyFromEnv()
	if !policy.IsPremium("sol") || !policy.IsPremium("fable") || !policy.IsPremium(" Fable ") {
		t.Fatalf("default policy does not cover sol/fable: %+v", policy)
	}
	if policy.IsPremium("luna") || policy.IsPremium("muse") || policy.IsPremium("") {
		t.Fatalf("default policy covers non-premium delegates: %+v", policy)
	}
	if policy.MaxCalls != 15 {
		t.Fatalf("default cap = %d, want 15", policy.MaxCalls)
	}
}

func TestPremiumPolicyFromEnvOverrides(t *testing.T) {
	t.Setenv("AIMEE_PREMIUM_DELEGATES", "opus, Sol ")
	t.Setenv("AIMEE_PREMIUM_CALL_CAP", "3")
	policy := PremiumPolicyFromEnv()
	if !policy.IsPremium("opus") || !policy.IsPremium("sol") || policy.IsPremium("fable") {
		t.Fatalf("override policy wrong: %+v", policy)
	}
	if policy.MaxCalls != 3 {
		t.Fatalf("override cap = %d, want 3", policy.MaxCalls)
	}
}

func TestPremiumPolicyFromEnvNoneDisables(t *testing.T) {
	t.Setenv("AIMEE_PREMIUM_DELEGATES", "none")
	policy := PremiumPolicyFromEnv()
	if policy.IsPremium("sol") || policy.IsPremium("fable") {
		t.Fatalf("'none' policy still marks premium delegates: %+v", policy)
	}
}

func TestNormalizeEscalationAcceptsOnlyDecisionClasses(t *testing.T) {
	for _, class := range []string{"architecture", "security", "migration", "contract", "requirement"} {
		if got := normalizeEscalation(" " + strings.ToUpper(class) + " "); got != class {
			t.Fatalf("normalizeEscalation(%q) = %q, want %q", class, got, class)
		}
	}
	for _, junk := range []string{"", "vibes", "style", "urgent", "architecture; drop table"} {
		if got := normalizeEscalation(junk); got != "" {
			t.Fatalf("normalizeEscalation(%q) = %q, want empty", junk, got)
		}
	}
}

func TestValidateContextBrief(t *testing.T) {
	valid := `{"schema_version":1,"summary":"do the thing","files":["a.go"],"acceptance_criteria":["it works"]}`
	if err := validateContextBrief([]byte(valid)); err != nil {
		t.Fatalf("valid brief rejected: %v", err)
	}
	tests := []struct {
		name string
		doc  string
	}{
		{name: "not json", doc: "hello"},
		{name: "wrong version", doc: `{"schema_version":2,"summary":"x","acceptance_criteria":["y"]}`},
		{name: "no summary", doc: `{"schema_version":1,"summary":" ","acceptance_criteria":["y"]}`},
		{name: "no acceptance", doc: `{"schema_version":1,"summary":"x","acceptance_criteria":[]}`},
		{name: "unknown field smuggling", doc: `{"schema_version":1,"summary":"x","acceptance_criteria":["y"],"raw_diff":"..."}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateContextBrief([]byte(test.doc)); err == nil {
				t.Fatalf("brief %q was accepted", test.name)
			}
		})
	}
	oversized := `{"schema_version":1,"summary":"x","acceptance_criteria":["y"],"files":["` +
		strings.Repeat("a", maxContextBriefBytes) + `"]}`
	if err := validateContextBrief([]byte(oversized)); err == nil ||
		!strings.Contains(err.Error(), "cap") {
		t.Fatalf("oversized brief err = %v, want size-cap rejection", err)
	}
}

func TestAdmitPremiumPlanningLedgerOnlyCountsDraft(t *testing.T) {
	store, err := db1.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateWorkItem(ctx, db1.CreateWorkItem{
		ID: "wi_premium_review", Repo: "r", ProposalPath: "p",
		WorkflowName: "build", StartStage: "plan",
	}); err != nil {
		t.Fatal(err)
	}
	policy := PremiumPolicy{Delegates: map[string]bool{"sol": true, "fable": true}, MaxCalls: 2}
	runner := &NativeRunner{db: store, premium: policy}

	// Repeated premium review/chairman/analysis/explain calls must not consume the planning ledger.
	for i := 0; i < 5; i++ {
		for _, req := range []DelegateRequest{
			{Delegate: "fable", Role: "review", Persona: "reviewer"},
			{Delegate: "sol", Role: "review", Persona: "qa"},
			{Delegate: "fable", Role: "analysis"},
			{Delegate: "sol", Role: "explain"},
			{Delegate: "claude:claude-fable-5", Role: "review"},
		} {
			step := StepRequest{WorkItem: db1.WorkItem{ID: "wi_premium_review"}, Node: wfe.Node{ID: "plan_gate"}}
			if err := runner.admitPremium(ctx, step, req); err != nil {
				t.Fatalf("review call %d role %q rejected: %v", i, req.Role, err)
			}
		}
		if count, err := store.PremiumCallCount(ctx, "wi_premium_review"); err != nil || count != 0 {
			t.Fatalf("review calls incremented planning ledger: count=%d err=%v", count, err)
		}
	}

	// Two draft calls consume the limited planning ledger.
	stepDraft := StepRequest{WorkItem: db1.WorkItem{ID: "wi_premium_review"}, Node: wfe.Node{ID: "plan"}}
	if err := runner.admitPremium(ctx, stepDraft, DelegateRequest{Delegate: "fable", Role: "draft"}); err != nil {
		t.Fatalf("first draft premium call rejected: %v", err)
	}
	if count, _ := store.PremiumCallCount(ctx, "wi_premium_review"); count != 1 {
		t.Fatalf("first draft count=%d, want 1", count)
	}
	if err := runner.admitPremium(ctx, stepDraft, DelegateRequest{Delegate: "sol", Role: "draft"}); err != nil {
		t.Fatalf("second draft premium call rejected: %v", err)
	}
	if count, _ := store.PremiumCallCount(ctx, "wi_premium_review"); count != 2 {
		t.Fatalf("second draft count=%d, want 2", count)
	}
	// Third draft exceeds the cap.
	if err := runner.admitPremium(ctx, stepDraft, DelegateRequest{Delegate: "fable", Role: "draft"}); !errors.Is(err, db1.ErrPremiumCallLimit) {
		t.Fatalf("third draft err=%v, want ErrPremiumCallLimit", err)
	}
	if count, _ := store.PremiumCallCount(ctx, "wi_premium_review"); count != 2 {
		t.Fatalf("cap count changed after refusal: %d, want 2", count)
	}

	// Replay-only draft calls must not double count.
	stepReplay := StepRequest{WorkItem: db1.WorkItem{ID: "wi_premium_review"}, Node: wfe.Node{ID: "plan"}, ReplayOnly: true}
	if err := runner.admitPremium(ctx, stepReplay, DelegateRequest{Delegate: "fable", Role: "draft"}); err != nil {
		t.Fatalf("replay-only draft rejected: %v", err)
	}
	if count, _ := store.PremiumCallCount(ctx, "wi_premium_review"); count != 2 {
		t.Fatalf("replay-only incremented ledger: %d, want 2", count)
	}

	// Premium write roles are still refused and do not record.
	for _, req := range []DelegateRequest{
		{Delegate: "fable", Role: "code", Tools: true},
		{Delegate: "fable", Role: "draft", Tools: true},
		{Delegate: "sol", Role: "code"},
		{Delegate: "fable", Role: "code", Persona: "engineer"},
	} {
		step := StepRequest{WorkItem: db1.WorkItem{ID: "wi_premium_review"}, Node: wfe.Node{ID: "impl"}}
		if err := runner.admitPremium(ctx, step, req); !errors.Is(err, ErrPremiumWriteRefused) {
			t.Fatalf("write-capable premium dispatch %+v err=%v, want ErrPremiumWriteRefused", req, err)
		}
	}
	if count, _ := store.PremiumCallCount(ctx, "wi_premium_review"); count != 2 {
		t.Fatalf("refused write dispatch incremented ledger: %d, want 2", count)
	}

	// Non-premium delegates are unrestricted and do not touch the ledger.
	if err := runner.admitPremium(ctx, stepDraft, DelegateRequest{Delegate: "muse", Role: "draft"}); err != nil {
		t.Fatalf("non-premium draft rejected: %v", err)
	}
	if err := runner.admitPremium(ctx, stepDraft, DelegateRequest{Delegate: "luna", Role: "review"}); err != nil {
		t.Fatalf("non-premium review rejected: %v", err)
	}
	if count, _ := store.PremiumCallCount(ctx, "wi_premium_review"); count != 2 {
		t.Fatalf("non-premium touched ledger: %d, want 2", count)
	}
}
