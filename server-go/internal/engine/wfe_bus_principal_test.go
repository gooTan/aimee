package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWFEModuleBusPrincipalsAreDistinctAndPinned(t *testing.T) {
	if WFEBusPrincipalRef != 64 {
		t.Fatalf("WFEBusPrincipalRef = %d, want 64", WFEBusPrincipalRef)
	}
	if WFEReviewBusPrincipalRef != 66 {
		t.Fatalf("WFEReviewBusPrincipalRef = %d, want 66", WFEReviewBusPrincipalRef)
	}
	if WFEBusPrincipalRef == WFEReviewBusPrincipalRef {
		t.Fatalf("WFE delegate and reviewer principals must be distinct (both %d)", WFEBusPrincipalRef)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	mainPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "cmd", "aimee-server", "main.go")
	data, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go %s: %v", mainPath, err)
	}
	text := string(data)
	if !strings.Contains(text, "engine.WFEBusPrincipalRef") {
		t.Fatal("main.go must retain WFEBusPrincipalRef for delegate caller")
	}
	if !strings.Contains(text, "engine.WFEReviewBusPrincipalRef") {
		t.Fatal("main.go must use WFEReviewBusPrincipalRef for NewBusReviewer")
	}
	idx := strings.Index(text, "NewBusReviewer")
	if idx == -1 {
		t.Fatal("main.go missing NewBusReviewer")
	}
	snippet := text[idx:]
	if len(snippet) > 800 {
		snippet = snippet[:800]
	}
	if !strings.Contains(snippet, "WFEReviewBusPrincipalRef") {
		t.Fatal("NewBusReviewer call must use WFEReviewBusPrincipalRef")
	}
	logIdx := strings.Index(text, "roundtable review requests will be sent over the event bus")
	if logIdx == -1 {
		t.Fatal("main.go missing reviewer success log")
	}
	logSnippet := text[logIdx:]
	if len(logSnippet) > 600 {
		logSnippet = logSnippet[:600]
	}
	if !strings.Contains(logSnippet, "WFEReviewBusPrincipalRef") {
		t.Fatal("reviewer success log must print WFEReviewBusPrincipalRef (not WFEBusPrincipalRef)")
	}
	if strings.Contains(logSnippet, "engine.WFEBusPrincipalRef") {
		t.Fatal("reviewer success log must not print WFEBusPrincipalRef; it must match the reviewer attachment")
	}
	if !strings.Contains(text, "PrincipalRef:   20") && !strings.Contains(text, "PrincipalRef: 20") {
		t.Fatal("workflow module service principal must remain 20")
	}
}
