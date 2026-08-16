package engine

import (
	"strings"
	"testing"
)

func TestPremiumPolicyFromEnvDefaults(t *testing.T) {
	t.Setenv("AIMEE_PREMIUM_DELEGATES", "")
	t.Setenv("AIMEE_PREMIUM_CALL_CAP", "")
	policy := PremiumPolicyFromEnv()
	if !policy.IsPremium("sol") || !policy.IsPremium("fable") || !policy.IsPremium(" Fable ") {
		t.Fatalf("default policy does not cover sol/fable: %+v", policy)
	}
	if policy.IsPremium("luna") || policy.IsPremium("deepseek") || policy.IsPremium("") {
		t.Fatalf("default policy covers non-premium delegates: %+v", policy)
	}
	if policy.MaxCalls != 2 {
		t.Fatalf("default cap = %d, want 2", policy.MaxCalls)
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

func TestContextBriefPromptLeadsWithTaskForRetrieval(t *testing.T) {
	task := "Add password reset through the authentication service"
	prompt := contextBriefPrompt(task)
	prefix := prompt
	if len(prefix) > 240 {
		prefix = prefix[:240]
	}
	if !strings.Contains(prefix, task) {
		t.Fatalf("task is absent from retrieval prefix: %q", prefix)
	}
}
