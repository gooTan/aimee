package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrPremiumWriteRefused is a policy refusal, not a provider failure. Premium
// delegates are planners and reviewers; a dispatch that would hand one a
// writable worktree is a workflow-definition bug and must never reach the CLI.
var ErrPremiumWriteRefused = errors.New("premium delegates are read-only and cannot be dispatched with write tools")

// PremiumPolicy bounds how often the expensive planning delegates may be
// invoked within one workflow run tree, and forbids dispatching them with
// write capability at all. The zero value enforces nothing, which preserves
// the behavior of deployments that configure no premium delegates.
type PremiumPolicy struct {
	// Delegates names the agents.json delegate entries that count as premium.
	Delegates map[string]bool
	// MaxCalls is the hard per-run-tree dispatch ceiling.
	MaxCalls int
}

func (p PremiumPolicy) IsPremium(delegate string) bool {
	return len(p.Delegates) > 0 && p.Delegates[strings.ToLower(strings.TrimSpace(delegate))]
}

const (
	defaultPremiumDelegates = "sol,fable"
	defaultPremiumCallCap   = 2
)

// PremiumPolicyFromEnv reads AIMEE_PREMIUM_DELEGATES (comma-separated delegate
// names; "none" disables enforcement) and AIMEE_PREMIUM_CALL_CAP. The defaults
// name the shipped premium planner seats and allow two calls: one planning
// call plus one escalation.
func PremiumPolicyFromEnv() PremiumPolicy {
	names := strings.TrimSpace(os.Getenv("AIMEE_PREMIUM_DELEGATES"))
	if names == "" {
		names = defaultPremiumDelegates
	}
	if strings.EqualFold(names, "none") {
		return PremiumPolicy{}
	}
	policy := PremiumPolicy{Delegates: map[string]bool{}, MaxCalls: defaultPremiumCallCap}
	for _, name := range strings.Split(names, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			policy.Delegates[name] = true
		}
	}
	if raw := strings.TrimSpace(os.Getenv("AIMEE_PREMIUM_CALL_CAP")); raw != "" {
		if cap, err := strconv.Atoi(raw); err == nil && cap > 0 {
			policy.MaxCalls = cap
		}
	}
	return policy
}

// escalationClasses are the only decision classes that justify a second
// premium call. Anything else a reviewer writes in the escalation field is
// treated as routine, so an over-eager reviewer fails toward the cheap repair
// path rather than toward premium spend.
var escalationClasses = map[string]bool{
	"architecture": true,
	"security":     true,
	"migration":    true,
	"contract":     true,
	"requirement":  true,
}

func normalizeEscalation(class string) string {
	class = strings.ToLower(strings.TrimSpace(class))
	if escalationClasses[class] {
		return class
	}
	return ""
}

// ContextBrief is the only shape a premium planner may receive as planning
// input. It is deliberately a summary type: relevant files and symbols,
// interfaces, constraints, prior decisions, risks, open questions, acceptance
// requirements, and artifact references. Full repository listings, raw logs,
// and complete diffs do not fit this schema and are rejected by size.
type ContextBrief struct {
	SchemaVersion      int      `json:"schema_version"`
	Summary            string   `json:"summary"`
	Files              []string `json:"files,omitempty"`
	Interfaces         []string `json:"interfaces,omitempty"`
	Constraints        []string `json:"constraints,omitempty"`
	Decisions          []string `json:"decisions,omitempty"`
	Risks              []string `json:"risks,omitempty"`
	OpenQuestions      []string `json:"open_questions,omitempty"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Artifacts          []string `json:"artifacts,omitempty"`
}

// maxContextBriefBytes bounds what a premium planner can be sent. 32 KiB holds
// a thorough brief for a large change while making a pasted repository listing
// or raw log dump fail loudly at the gate instead of silently costing money.
const maxContextBriefBytes = 32 * 1024

func contextBriefPrompt(proposal string) string {
	return "Prepare a concise ContextBrief for a senior planning reviewer. Return only JSON shaped " +
		`{"schema_version":1,"summary":"...","files":["path or path:symbol"],"interfaces":["..."],"constraints":["..."],"decisions":["..."],"risks":["..."],"open_questions":["..."],"acceptance_criteria":["..."],"artifacts":["..."]}. ` +
		"List only the files, symbols, interfaces, constraints, prior decisions, risks, open questions, acceptance requirements, and artifact references that are relevant to this task. " +
		"Never include full repository listings, raw logs, complete diffs, or conversation history. The whole brief must stay under 32768 bytes.\n\nTASK:\n" + proposal
}

func validateContextBrief(doc []byte) error {
	if len(doc) > maxContextBriefBytes {
		return fmt.Errorf("context brief is %d bytes; the premium planning input cap is %d", len(doc), maxContextBriefBytes)
	}
	var brief ContextBrief
	decoder := json.NewDecoder(strings.NewReader(string(doc)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&brief); err != nil {
		return fmt.Errorf("context brief is not valid JSON of the ContextBrief shape: %w", err)
	}
	if brief.SchemaVersion != 1 {
		return errors.New("context brief schema_version must be 1")
	}
	if strings.TrimSpace(brief.Summary) == "" {
		return errors.New("context brief summary is required")
	}
	if len(brief.AcceptanceCriteria) == 0 {
		return errors.New("context brief acceptance_criteria are required")
	}
	return nil
}
