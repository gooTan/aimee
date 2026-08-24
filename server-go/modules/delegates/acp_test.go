package delegates

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	delegatecontract "github.com/JBailes/aimee/server-go/delegate"
)

func acpMuseCliCmd(scenario string) string {
	return fmt.Sprintf("%q -test.run=TestACPHelperProcess -- %s", os.Args[0], scenario)
}

func newMuseExecutor(t *testing.T, scenario string, idleMs int64) (*RegistryExecutor, string) {
	t.Helper()
	home := t.TempDir()
	workdir := filepath.Join(home, "workdir")
	if err := os.MkdirAll(filepath.Join(workdir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	cliCmd := acpMuseCliCmd(scenario)
	agent := map[string]any{
		"name":             "muse",
		"model":            "opencode-go/muse-spark-1.2-contributor",
		"cli_kind":         "acp",
		"cli_cmd":          cliCmd,
		"enabled":          true,
		"roles":            []string{"draft", "code", "execute"},
		"reasoning_effort": "xhigh",
	}
	if idleMs > 0 {
		agent["cli_idle_timeout_ms"] = idleMs
	}
	registry := map[string]any{
		"models": []map[string]any{agent},
	}
	body, _ := json.Marshal(registry)
	if err := os.WriteFile(filepath.Join(home, "models.json"), body, 0o600); err != nil {
		t.Fatalf("write models.json: %v", err)
	}
	exec, err := NewRegistryExecutor(home)
	if err != nil {
		t.Fatalf("NewRegistryExecutor: %v", err)
	}
	return exec, workdir
}

func TestACPTransportHappyReadOnly(t *testing.T) {
	exec, workdir := newMuseExecutor(t, "happy-readonly", 0)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := exec.Execute(ctx, delegatecontract.Invocation{
		Version: delegatecontract.WireVersion,
		Role:    "draft",
		Persona: "architect",
		Prompt:  "hello",
		Workdir: workdir,
		Tools:   false,
	})
	if result.Status != "done" {
		t.Fatalf("status = %q want done err=%q", result.Status, result.Error)
	}
	if result.Agent != "muse" {
		t.Fatalf("agent = %q want muse", result.Agent)
	}
	if result.Response != "MUSE-ACP-OK" {
		t.Fatalf("response = %q want MUSE-ACP-OK", result.Response)
	}
	if !result.ResponseStarted {
		t.Fatalf("ResponseStarted = false want true")
	}
	if _, err := os.Stat(filepath.Join(workdir, "blocked.txt")); !os.IsNotExist(err) {
		t.Fatalf("blocked.txt was created or stat error: %v", err)
	}
}

func TestACPTransportRejectsUnacceptedPins(t *testing.T) {
	cases := []struct {
		name       string
		scenario   string
		wantSubstr string
		wantMethod string
	}{
		{"refuse-model", "refuse-model", "did not accept pinned model", "session/set_model"},
		{"missing-model-result", "missing-model-result", "did not accept pinned model", "session/set_model"},
		{"refuse-effort", "refuse-effort", "did not accept reasoning effort", "session/set_config_option"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec, workdir := newMuseExecutor(t, tc.scenario, 0)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result := exec.Execute(ctx, delegatecontract.Invocation{
				Version: delegatecontract.WireVersion,
				Role:    "draft",
				Persona: "architect",
				Prompt:  "hello",
				Workdir: workdir,
				Tools:   false,
			})
			if result.Status != "failed" {
				t.Fatalf("status = %q want failed", result.Status)
			}
			if !strings.Contains(result.Error, tc.wantSubstr) {
				t.Fatalf("error %q does not contain %q", result.Error, tc.wantSubstr)
			}
			if !strings.Contains(result.Error, tc.wantMethod) {
				t.Fatalf("error %q does not contain %q", result.Error, tc.wantMethod)
			}
			if result.ResponseStarted {
				t.Fatalf("ResponseStarted = true want false")
			}
		})
	}
}

func TestACPTransportCallerDeadline(t *testing.T) {
	exec, workdir := newMuseExecutor(t, "stall-after-new", 1000)
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := exec.Execute(ctx, delegatecontract.Invocation{
		Version: delegatecontract.WireVersion,
		Role:    "draft",
		Persona: "architect",
		Prompt:  "hello",
		Workdir: workdir,
		Tools:   false,
	})
	elapsed := time.Since(start)
	if result.Status != "failed" {
		t.Fatalf("status = %q want failed", result.Status)
	}
	if result.ResponseStarted {
		t.Fatalf("ResponseStarted = true want false")
	}
	if !strings.Contains(result.Error, delegatecontract.ErrDelegateExecutionDeadline.Error()) {
		t.Fatalf("error %q does not contain %q", result.Error, delegatecontract.ErrDelegateExecutionDeadline.Error())
	}
	if elapsed >= time.Second {
		t.Fatalf("elapsed %s >= 1s want well under one second", elapsed)
	}
}

func TestACPTransportConfiguredIdleTimeout(t *testing.T) {
	exec, workdir := newMuseExecutor(t, "stall-after-new", 60)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	result := exec.Execute(ctx, delegatecontract.Invocation{
		Version: delegatecontract.WireVersion,
		Role:    "draft",
		Persona: "architect",
		Prompt:  "hello",
		Workdir: workdir,
		Tools:   false,
	})
	elapsed := time.Since(start)
	if result.Status != "failed" {
		t.Fatalf("status = %q want failed", result.Status)
	}
	if result.ResponseStarted {
		t.Fatalf("ResponseStarted = true want false")
	}
	if !strings.Contains(result.Error, "ACP delegate idle timeout") {
		t.Fatalf("error %q does not contain ACP delegate idle timeout", result.Error)
	}
	if strings.Contains(result.Error, delegatecontract.ErrDelegateExecutionDeadline.Error()) {
		t.Fatalf("error %q incorrectly contains delegate deadline", result.Error)
	}
	if elapsed >= time.Second {
		t.Fatalf("elapsed %s >= 1s want well under one second", elapsed)
	}
}
