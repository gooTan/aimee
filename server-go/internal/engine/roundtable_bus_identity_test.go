package engine

import "testing"

func TestWFEDelegateAndReviewCallersUseDistinctBusPrincipals(t *testing.T) {
	if WFEBusPrincipalRef == WFEReviewBusPrincipalRef {
		t.Fatal("delegate and roundtable callers cannot attach with the same bus principal")
	}
}
