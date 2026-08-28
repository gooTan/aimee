package delegate

import (
	"fmt"
	"strings"
	"time"
)

// Tool event kind constants for lifecycle_event.kind.
const (
	ToolEventStart    = "model_tool_start"
	ToolEventComplete = "model_tool_complete"
	ToolEventError    = "model_tool_error"
)

// FormatToolDetail builds a safe, redacted detail string for a per-tool-call
// lifecycle event. It never includes raw tool arguments, results, prompts,
// commands, credentials, or hidden reasoning — only the safe identifiers and
// timing.
//
// Fields that are empty are omitted. The output is a space-separated
// key=value list, matching the existing model_dispatch/detail convention, so
// the workflow status renderer and event API can display it without a schema
// change.
func FormatToolDetail(workflow *WorkflowContext, tool ToolEvent, elapsed time.Duration) string {
	var b strings.Builder
	if workflow != nil {
		if workflow.Model != "" {
			fmt.Fprintf(&b, "model=%s ", sanitizeValue(workflow.Model))
		}
		if workflow.Role != "" {
			fmt.Fprintf(&b, "role=%s ", sanitizeValue(workflow.Role))
		}
		if workflow.Persona != "" {
			fmt.Fprintf(&b, "persona=%s ", sanitizeValue(workflow.Persona))
		}
		if workflow.Phase != "" {
			fmt.Fprintf(&b, "phase=%s ", sanitizeValue(workflow.Phase))
		}
		if workflow.Invocation != "" {
			fmt.Fprintf(&b, "invocation=%s ", sanitizeValue(workflow.Invocation))
		}
	}
	if tool.ToolName != "" {
		fmt.Fprintf(&b, "tool=%s ", sanitizeValue(tool.ToolName))
	}
	if tool.CallID != "" {
		fmt.Fprintf(&b, "call_id=%s ", sanitizeValue(tool.CallID))
	}
	if tool.Status != "" {
		fmt.Fprintf(&b, "status=%s ", sanitizeValue(tool.Status))
	}
	if tool.Phase != "" && (workflow == nil || workflow.Phase == "") {
		fmt.Fprintf(&b, "phase=%s ", sanitizeValue(tool.Phase))
	}
	if elapsed > 0 {
		fmt.Fprintf(&b, "elapsed=%s ", elapsed.Round(time.Millisecond).String())
	} else if tool.ElapsedMS > 0 {
		fmt.Fprintf(&b, "elapsed=%s ", (time.Duration(tool.ElapsedMS) * time.Millisecond).Round(time.Millisecond).String())
	}
	s := strings.TrimSpace(b.String())
	s = SafeDiagnostic(s)
	if len(s) > 4096 {
		s = s[:4096]
	}
	return s
}

func sanitizeValue(v string) string {
	v = strings.TrimSpace(v)
	// Keep only safe identifier characters; everything else becomes _.
	// This prevents a crafted tool name carrying prompt fragments or secrets
	// from leaking through the safe detail.
	var b strings.Builder
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
		if b.Len() >= 128 {
			break
		}
	}
	s := b.String()
	// Redact any credential-like patterns that survived sanitization.
	s = SafeDiagnostic(s)
	// Plain "secret" substrings (e.g., a tool name smuggling a credential)
	// must not survive even without a key= prefix. This is defense in depth
	// beyond the SafeDiagnostic patterns, which require a key delimiter.
	lower := strings.ToLower(s)
	if strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "token") {
		s = "[REDACTED]"
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

// ToolEventKind returns the lifecycle kind for a tool event status.
func ToolEventKind(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "failed":
		return ToolEventError
	case "completed", "complete", "done":
		return ToolEventComplete
	default:
		return ToolEventStart
	}
}
