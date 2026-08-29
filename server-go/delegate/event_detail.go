package delegate

import (
	"strings"
	"time"
)

var toolDetailKeys = [...]string{"model", "role", "persona", "phase", "invocation", "tool", "call_id", "status", "elapsed"}

// ValidateToolEventDetail accepts only the bounded key=value format emitted by
// FormatToolDetail and returns its canonical representation for persistence.
func ValidateToolEventDetail(kind, detail string) (string, bool) {
	if len(detail) == 0 || len(detail) > 4096 {
		return "", false
	}
	values := make(map[string]string, len(toolDetailKeys))
	for _, field := range strings.Fields(detail) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || value == "" || !toolDetailKey(key) || values[key] != "" || len(value) > 128 || !safeToolDetailValue(value) {
			return "", false
		}
		values[key] = value
	}
	if values["tool"] == "" || values["call_id"] == "" || values["status"] == "" {
		return "", false
	}
	wantStatus := map[string]string{ToolEventStart: "started", ToolEventComplete: "completed", ToolEventError: values["status"]}[kind]
	if wantStatus == "" || values["status"] != wantStatus {
		return "", false
	}
	if kind == ToolEventError && values["status"] != "error" && values["status"] != "failed" && values["status"] != "cancelled" && values["status"] != "canceled" {
		return "", false
	}
	if elapsed := values["elapsed"]; elapsed != "" {
		if !validElapsed(elapsed) {
			return "", false
		}
	}
	var out strings.Builder
	for _, key := range toolDetailKeys {
		if value := values[key]; value != "" {
			if out.Len() > 0 {
				out.WriteByte(' ')
			}
			out.WriteString(key)
			out.WriteByte('=')
			out.WriteString(value)
		}
	}
	return out.String(), true
}

func toolDetailKey(key string) bool {
	for _, allowed := range toolDetailKeys {
		if key == allowed {
			return true
		}
	}
	return false
}

func safeToolDetailValue(value string) bool {
	if value == "[REDACTED]" {
		return true
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func validElapsed(value string) bool {
	duration, err := time.ParseDuration(value)
	return err == nil && duration >= 0
}
