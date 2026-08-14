package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JBailes/aimee/server-go/internal/db1"
	"github.com/JBailes/aimee/server-go/internal/wfe"
)

// TestFactoryLiveSmoke drives one shipped factory workflow end to end with
// REAL delegate CLIs on the operator's subscriptions: the true engine,
// scheduler, worktrees, verifier, premium ledger, and human gate, with only
// the bus transport replaced by direct CLI execution. It is opt-in and costs
// real model turns:
//
//	AIMEE_FACTORY_LIVE_SMOKE=1 \
//	AIMEE_SMOKE_WORKROOT=/mnt/c/Users/you/AppData/Local/Temp/aimee-smoke \
//	AIMEE_SMOKE_CMD_LUNA="codex.exe exec --sandbox read-only -m gpt-5.6-luna -" \
//	AIMEE_SMOKE_CMD_DEEPSEEK="opencode.exe run -m opencode-go/deepseek-v4-flash {PROMPT}" \
//	... go test ./internal/engine -run TestFactoryLiveSmoke -v -timeout 60m
//
// Command templates come from AIMEE_SMOKE_CMD_<DELEGATE> (upper-cased
// delegate name, whitespace-split, so executables must not contain spaces).
// Tokens: {PROMPT} passes the prompt as one argv element; {TASK_FILE} and
// {OUT_FILE} write the prompt to a file and read the reply from a file
// (oracle's shape), translated with wslpath -w when targeting Windows CLIs
// from WSL; with no token the prompt is fed on stdin. Argv prompts are
// smoke-scale only: they carry the whole frozen diff.
//
// AIMEE_SMOKE_WORKFLOW selects the definition (default quick-change) and
// AIMEE_SMOKE_DEADLINE_MIN bounds the run (default 30).
func TestFactoryLiveSmoke(t *testing.T) {
	if os.Getenv("AIMEE_FACTORY_LIVE_SMOKE") != "1" {
		t.Skip("live smoke is opt-in: set AIMEE_FACTORY_LIVE_SMOKE=1 and AIMEE_SMOKE_CMD_* templates")
	}
	workflowName := os.Getenv("AIMEE_SMOKE_WORKFLOW")
	if workflowName == "" {
		workflowName = "quick-change"
	}
	deadlineMin := 30
	if raw := os.Getenv("AIMEE_SMOKE_DEADLINE_MIN"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			deadlineMin = n
		}
	}
	workroot := os.Getenv("AIMEE_SMOKE_WORKROOT")
	if workroot == "" {
		workroot = t.TempDir()
	} else {
		workroot = filepath.Join(workroot, fmt.Sprintf("run-%d", time.Now().Unix()))
		if err := os.MkdirAll(workroot, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("live smoke: workflow=%s workroot=%s", workflowName, workroot)

	repo := newLiveSmokeRepo(t, workroot)
	workflowDir := copyShippedWorkflowDefinitions(t)
	registry, err := wfe.NewRegistry(workflowDir)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := registry.Pin(workflowName)
	if err != nil {
		t.Fatal(err)
	}
	store, err := db1.Open(filepath.Join(workroot, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	artifacts, err := wfe.NewArtifactStore(filepath.Join(workroot, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	id := "wi_live_smoke"
	proposal := "Add the factory smoke marker file\n\n" +
		"Create a file named FACTORY-SMOKE.md in the repository root containing exactly one line: " +
		"factory smoke ok\n" +
		"Do not change any other file. The repository verification is `sh verify.sh`, which checks " +
		"exactly this outcome.\n"
	if err := artifacts.PutProposal(id, []byte(proposal)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkItem(context.Background(), db1.CreateWorkItem{
		ID: id, Repo: repo, ProposalPath: "proposal:live-smoke",
		WorkflowName: workflowName, WorkflowVersion: definition.Version,
		StartStage: definition.Start,
	}); err != nil {
		t.Fatal(err)
	}
	worktrees, err := NewWorktreeManager(store, filepath.Join(workroot, "trees"))
	if err != nil {
		t.Fatal(err)
	}
	verifier := CommandVerifier{Command: []string{"sh", "verify.sh"},
		LockFile: filepath.Join(workroot, "verify.lock")}
	forge := &e2eForge{}
	agents := &liveSmokeAgents{t: t, fileDir: workroot}
	runner, err := NewNativeRunner(store, worktrees, agents, verifier, artifacts, registry, forge)
	if err != nil {
		t.Fatal(err)
	}
	runner.SetPremiumPolicy(factoryPremiumPolicy())
	eng, err := New(store, artifacts, workflowDir, runner)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, eng, 1, nil)
	scheduler.pollEvery = 250 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { scheduler.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(time.Duration(deadlineMin) * time.Minute)
	var item db1.WorkItem
	lastStage := ""
	for time.Now().Before(deadline) {
		item, err = store.WorkItem(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if item.Stage != lastStage {
			t.Logf("stage: %s", item.Stage)
			lastStage = item.Stage
		}
		if item.PauseReason == "human_gate" {
			break
		}
		if item.State != "active" || (item.PauseReason != "" && item.PauseReason != "human_gate") {
			events, _ := store.Events(context.Background(), id, 0, 1000)
			t.Fatalf("live run stopped early: item=%+v events=%+v", item, events)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if item.PauseReason != "human_gate" {
		events, _ := store.Events(context.Background(), id, 0, 1000)
		t.Fatalf("live run did not reach the human gate in %d min: item=%+v events=%+v",
			deadlineMin, item, events)
	}
	premium, err := store.PremiumCallCount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parked at human gate; premium calls ledgered: %d", premium)
	if workflowName == "quick-change" && premium != 0 {
		t.Fatalf("quick-change spent %d premium calls live, want 0", premium)
	}

	// Approve exactly as the API does, then require full acceptance.
	artifact, err := artifacts.PutNodeArtifact(id, "human_gate", "approval", []byte("approved"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGate(context.Background(), id, "human_gate", "deliver",
		"approve", artifact.Hash); err != nil {
		t.Fatal(err)
	}
	acceptDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(acceptDeadline) {
		item, err = store.WorkItem(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if item.State == "accepted" {
			forge.mu.Lock()
			opens := append([]PullRequestSpec(nil), forge.opens...)
			forge.mu.Unlock()
			t.Logf("accepted; draft PRs opened: %d; delegate dispatches: %v",
				len(opens), agents.dispatchSummary())
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("approved gate did not reach acceptance: %+v", item)
}

// newLiveSmokeRepo seeds a repository whose deterministic verification fails
// until the requested marker file exists, so the verifier gate is real.
func newLiveSmokeRepo(t *testing.T, root string) string {
	t.Helper()
	bare := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=smoke", "GIT_AUTHOR_EMAIL=s@example",
			"GIT_COMMITTER_NAME=smoke", "GIT_COMMITTER_EMAIL=s@example")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(root, "init", "--bare", bare)
	run(root, "clone", bare, repo)
	run(repo, "checkout", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(repo, "README.md"),
		[]byte("factory live smoke fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verify := "#!/bin/sh\n" +
		"test -f FACTORY-SMOKE.md || { echo 'verify failed: FACTORY-SMOKE.md is missing'; exit 1; }\n" +
		"grep -q 'factory smoke ok' FACTORY-SMOKE.md || { echo 'verify failed: marker line missing'; exit 1; }\n" +
		"echo 'verify ok'\n"
	if err := os.WriteFile(filepath.Join(repo, "verify.sh"), []byte(verify), 0o755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(repo, ".agents", "skills", "code-review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "# Code review skill\n\n" +
		"Review along two axes. Standards: correctness, safety, convention consistency. " +
		"Spec: the change does exactly what the request asked, nothing missing, nothing extra. " +
		"Only report findings you would block a merge on.\n"
	if err := os.WriteFile(skillPath, []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", ".")
	run(repo, "commit", "-m", "seed live smoke fixture")
	run(repo, "push", "-u", "origin", "trunk")
	run(repo, "remote", "set-head", "origin", "trunk")
	return repo
}

type liveSmokeAgents struct {
	t         *testing.T
	fileDir   string
	dispatchN int
	dispatch  []string
}

func (a *liveSmokeAgents) dispatchSummary() []string { return a.dispatch }

// hostPath translates a path for the delegate CLI. Windows executables run
// through WSL interop receive literal argv, so file paths must be translated
// with wslpath -w; native Linux CLIs take the path as is.
func hostPath(command, path string) string {
	if !strings.Contains(strings.ToLower(command), ".exe") &&
		!strings.Contains(strings.ToLower(command), ".js") {
		return path
	}
	out, err := exec.Command("wslpath", "-w", path).Output()
	if err != nil {
		return path
	}
	return strings.TrimSpace(string(out))
}

func (a *liveSmokeAgents) Delegate(ctx context.Context, request DelegateRequest) (DelegateResult, error) {
	envKey := "AIMEE_SMOKE_CMD_" + strings.ReplaceAll(
		strings.ToUpper(strings.TrimSpace(request.Delegate)), "-", "_")
	template := os.Getenv(envKey)
	if template == "" {
		return DelegateResult{}, fmt.Errorf("no %s template for delegate %q", envKey, request.Delegate)
	}
	args := strings.Fields(template)
	a.dispatchN++
	label := fmt.Sprintf("%02d-%s-%s", a.dispatchN, request.Delegate, request.Stage)
	a.dispatch = append(a.dispatch, request.Delegate+":"+request.Stage)

	stdinPrompt := true
	outFile := ""
	for i := range args {
		switch args[i] {
		case "{PROMPT}":
			args[i] = request.Prompt
			stdinPrompt = false
		case "{TASK_FILE}":
			task := filepath.Join(a.fileDir, label+"-task.md")
			if err := os.WriteFile(task, []byte(request.Prompt), 0o644); err != nil {
				return DelegateResult{}, err
			}
			args[i] = hostPath(args[0], task)
			stdinPrompt = false
		case "{OUT_FILE}":
			outFile = filepath.Join(a.fileDir, label+"-answer.md")
			args[i] = hostPath(args[0], outFile)
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(callCtx, args[0], args[1:]...)
	if request.Workdir != "" {
		cmd.Dir = request.Workdir
	}
	if stdinPrompt {
		cmd.Stdin = strings.NewReader(request.Prompt)
	}
	started := time.Now()
	a.t.Logf("dispatch %s (role=%s tools=%v workdir=%s)", label, request.Role, request.Tools, cmd.Dir)
	output, err := cmd.CombinedOutput()
	a.t.Logf("dispatch %s finished in %s (%d bytes, err=%v)", label,
		time.Since(started).Round(time.Second), len(output), err)
	// Keep a transcript per dispatch for the operator.
	_ = os.WriteFile(filepath.Join(a.fileDir, label+"-stdout.log"), output, 0o644)
	if err != nil {
		tail := output
		if len(tail) > 800 {
			tail = tail[len(tail)-800:]
		}
		return DelegateResult{}, fmt.Errorf("live delegate %s failed: %v: %s", request.Delegate, err, tail)
	}
	response := string(output)
	if outFile != "" {
		content, readErr := os.ReadFile(outFile)
		if readErr != nil || len(strings.TrimSpace(string(content))) == 0 {
			return DelegateResult{}, fmt.Errorf("live delegate %s produced no answer file", request.Delegate)
		}
		response = string(content)
	}
	return DelegateResult{Response: strings.TrimSpace(response), Agent: request.Delegate}, nil
}

func (a *liveSmokeAgents) DelegateGroup(ctx context.Context, requests []DelegateRequest) []DelegateGroupResult {
	return testDelegateGroup(ctx, requests, a.Delegate)
}
