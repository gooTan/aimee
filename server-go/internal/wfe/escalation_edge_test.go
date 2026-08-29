package wfe

import (
	"strings"
	"testing"
)

func TestOnEscalateEdgeValidation(t *testing.T) {
	base := `name: t
start: impl
nodes:
  - id: impl
    block: implement
    in: {plan: review.out}
    next: review
  - id: review
    block: review
    in: {src: impl.out}
    on_pass: impl
    on_fail: impl
%s
`
	if _, err := ParseDefinition([]byte(strings.Replace(base, "%s", "    on_escalate: impl", 1))); err != nil {
		t.Fatalf("valid on_escalate rejected: %v", err)
	}
	if _, err := ParseDefinition([]byte(strings.Replace(base, "%s", "    on_escalate: missing", 1))); err == nil {
		t.Fatal("on_escalate to a missing node was accepted")
	}
	nonReview := `name: t
start: impl
nodes:
  - id: impl
    block: implement
    in: {plan: impl.out}
    on_fail: impl
    on_escalate: impl
`
	if _, err := ParseDefinition([]byte(nonReview)); err == nil ||
		!strings.Contains(err.Error(), "on_escalate") {
		t.Fatalf("on_escalate on a non-review block err=%v, want rejection", err)
	}
}
