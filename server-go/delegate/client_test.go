package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JBailes/aimee/server-go/bus"
)

type recordingCaller struct {
	request  []byte
	deadline time.Duration
	result   InvocationResult
	reply    []byte
	err      error
}

type groupRoutingCaller struct {
	mu               sync.Mutex
	plannedModels    []string
	planError        string
	invocationResult InvocationResult
	invokedModels    []string
	plan             GroupPlan
}

func (c *groupRoutingCaller) Call(_ context.Context, _, stage uint32, _ uint64,
	_ time.Duration, request []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stage == StageGroupPlan {
		if err := json.Unmarshal(request, &c.plan); err != nil {
			return nil, err
		}
		return json.Marshal(GroupPlanResult{Version: WireVersion, Models: c.plannedModels, Error: c.planError})
	}
	var invocation Invocation
	if err := json.Unmarshal(request, &invocation); err != nil {
		return nil, err
	}
	c.invokedModels = append(c.invokedModels, invocation.Model)
	result := c.invocationResult
	if result.Status == "" {
		result = InvocationResult{Version: WireVersion, Status: "done", Response: "ok"}
	}
	result.Agent = invocation.Model
	return json.Marshal(result)
}

func (c *recordingCaller) Call(_ context.Context, kind, stage uint32, _ uint64,
	deadline time.Duration, request []byte) ([]byte, error) {
	if kind != EventKind || stage != StageInvoke {
		panic("wrong delegate stage")
	}
	c.request = append([]byte(nil), request...)
	c.deadline = deadline
	if c.err != nil {
		return nil, c.err
	}
	if c.reply != nil {
		return c.reply, nil
	}
	return json.Marshal(c.result)
}

func TestDelegateIsUnboundedWhenNoDeadlineIsConfigured(t *testing.T) {
	caller := &recordingCaller{result: InvocationResult{Version: WireVersion, Status: "done", Response: "ok"}}
	client, err := NewClient(caller, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "qa", Prompt: "review"}); err != nil {
		t.Fatal(err)
	}
	if caller.deadline != 0 {
		t.Fatalf("bus deadline = %s, want unbounded", caller.deadline)
	}
	var wire Invocation
	if err := json.Unmarshal(caller.request, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ExecutionTimeoutMS != 0 {
		t.Fatalf("execution timeout = %d, want unbounded", wire.ExecutionTimeoutMS)
	}
}

func TestBusWireOmitsCallerLifecycleState(t *testing.T) {
	caller := &recordingCaller{result: InvocationResult{Version: WireVersion, Status: "done", Response: "ok"}}
	client := &BusClient{caller: caller, deadline: time.Second}
	_, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "security",
		Delegate: "codex", Prompt: "review this", Workdir: "/repo", Tools: true,
		WorkItemID: "wi-1", Stage: "gate", ExecutionVersion: "v7", RetryTag: "retry",
		DurableSlot: "seat-2", Participant: "opaque", ReplayOnly: false})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(caller.request, &fields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"work_item_id", "stage", "execution_version", "retry_tag",
		"durable_slot", "participant", "replay_only"} {
		if _, exists := fields[forbidden]; exists {
			t.Fatalf("caller state %q crossed the delegate wire", forbidden)
		}
	}
	for _, required := range []string{"role", "persona", "model", "prompt", "workdir", "tools",
		"execution_timeout_ms"} {
		if _, exists := fields[required]; !exists {
			t.Fatalf("delegate field %q missing", required)
		}
	}
}

func TestReplayOnlyDoesNotLaunchStatelessDelegate(t *testing.T) {
	client := &BusClient{caller: &recordingCaller{}, deadline: time.Second}
	_, err := client.Delegate(t.Context(), DelegateRequest{Role: "code", Persona: "engineer",
		Prompt: "work", ReplayOnly: true})
	if !errors.Is(err, ErrDelegateReplayUnavailable) {
		t.Fatalf("replay-only error = %v", err)
	}
	var execution *DelegateExecutionError
	if !errors.As(err, &execution) || !execution.Dispatched || execution.CostKnown {
		t.Fatalf("replay billing boundary = %#v", execution)
	}
}

func TestCostLimitFailsBeforeDispatch(t *testing.T) {
	caller := &recordingCaller{}
	client := &BusClient{caller: caller, deadline: time.Second}
	_, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "security",
		Prompt: "review this", MaxCostUSD: 0.25})
	if !errors.Is(err, ErrDelegateCostLimitUnsupported) {
		t.Fatalf("cost limit error = %v", err)
	}
	if caller.request != nil {
		t.Fatal("an unenforceable cost-capped request was dispatched")
	}
}

func TestDelegateResultPreservesCallerLocalParticipant(t *testing.T) {
	caller := &recordingCaller{result: InvocationResult{Version: WireVersion, Status: "done", Response: "ok"}}
	client := &BusClient{caller: caller, deadline: time.Second}
	result, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "security",
		Prompt: "review this", Participant: "seat-security"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Participant != "seat-security" {
		t.Fatalf("participant = %q", result.Participant)
	}
}

func TestDispatchBoundaryClassifiesBusAndReplyFailures(t *testing.T) {
	request := DelegateRequest{Role: "review", Persona: "security", Prompt: "review"}
	for _, preDispatch := range []error{
		bus.ErrModuleCallCapabilityAbsent,
		bus.ErrModuleCallRejected,
		errors.Join(bus.ErrModuleCallNotDispatched, errors.New("ring full")),
	} {
		client := &BusClient{caller: &recordingCaller{err: preDispatch}, deadline: time.Second}
		_, err := client.Delegate(t.Context(), request)
		var execution *DelegateExecutionError
		if !errors.Is(err, preDispatch) || errors.As(err, &execution) {
			t.Fatalf("pre-dispatch %v classified as %#v", preDispatch, err)
		}
	}
	for _, postDispatch := range [][]byte{[]byte("not-json"), []byte(`{"version":2,"status":"working"}`)} {
		client := &BusClient{caller: &recordingCaller{reply: postDispatch}, deadline: time.Second}
		_, err := client.Delegate(t.Context(), request)
		var execution *DelegateExecutionError
		if !errors.As(err, &execution) || !execution.Dispatched || execution.CostKnown {
			t.Fatalf("post-dispatch reply %q classified as %#v", postDispatch, err)
		}
	}
	deadlineCtx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	deadlineClient := &BusClient{caller: &recordingCaller{}, deadline: time.Second}
	result, err := deadlineClient.Delegate(deadlineCtx, request)
	if err == nil || result.AvailabilityClass != AvailabilityClassStartDeadline {
		t.Fatalf("pre-dispatch deadline was not classified: result=%+v err=%v", result, err)
	}
	transportDeadline := &BusClient{caller: &recordingCaller{err: bus.ErrModuleCallDeadline}, deadline: time.Second}
	result, err = transportDeadline.Delegate(t.Context(), request)
	if err == nil || result.AvailabilityClass != AvailabilityClassNone {
		t.Fatalf("post-dispatch reply loss was classified as retryable: result=%+v err=%v", result, err)
	}
}

func TestClassifyAvailability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		began bool
		want  AvailabilityClass
	}{
		{name: "quota diagnostic", err: errors.New("provider quota exceeded")},
		{name: "rate limit diagnostic", err: errors.New("aimee_err=rate_limit")},
		{name: "capacity", err: ErrDelegateCapacity, want: AvailabilityClassCapacity},
		{name: "capacity deadline diagnostic", err: errors.New("aimee_err=capacity_deadline"), want: AvailabilityClassCapacity},
		{name: "authentication diagnostic", err: errors.New("authentication failed")},
		{name: "session outage", err: errors.New("session unavailable")},
		{name: "provider diagnostic", err: errors.New("provider unavailable")},
		{name: "missing cli", err: errors.New("no enabled delegate CLI is configured")},
		{name: "deadline", err: ErrDelegateExecutionDeadline},
		{name: "response started", err: errors.New("quota exceeded"), began: true},
		{name: "replay", err: ErrDelegateReplayUnavailable},
		{name: "terminal", err: ErrDelegateTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAvailability(tc.err, tc.began); got != tc.want {
				t.Fatalf("ClassifyAvailability(%v, %v) = %q, want %q", tc.err, tc.began, got, tc.want)
			}
		})
	}
}

func TestClassifyProviderAvailabilityRecognizesClaudeSessionLimit(t *testing.T) {
	if got := ClassifyProviderAvailability(errors.New("You've hit your session limit · resets 1:20am"), false); got != AvailabilityClassQuotaRateLimit {
		t.Fatalf("class=%q, want %q", got, AvailabilityClassQuotaRateLimit)
	}
}

func TestDelegateCarriesAvailabilityClass(t *testing.T) {
	client := &BusClient{caller: &recordingCaller{result: InvocationResult{Version: WireVersion, Status: "failed",
		Error: "provider unavailable", AvailabilityClass: AvailabilityClassProviderUnavailable}}, deadline: time.Second}
	result, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "qa", Prompt: "review"})
	if err == nil || result.AvailabilityClass != AvailabilityClassProviderUnavailable {
		t.Fatalf("single delegate lost availability class: result=%+v err=%v", result, err)
	}

	group := &BusClient{caller: &groupRoutingCaller{planError: ErrDelegateCapacity.Error()}, deadline: time.Second}
	results := group.DelegateGroup(t.Context(), []DelegateRequest{{Role: "review", Persona: "qa", Prompt: "review"}})
	if len(results) != 1 || results[0].AvailabilityClass != AvailabilityClassCapacity {
		t.Fatalf("group planning lost availability class: %+v", results)
	}

	partial := &BusClient{caller: &recordingCaller{result: InvocationResult{Version: WireVersion, Status: "failed",
		Response: "partial output", Error: "provider unavailable", AvailabilityClass: AvailabilityClassProviderUnavailable, ResponseStarted: true}}, deadline: time.Second}
	result, err = partial.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "qa", Prompt: "review"})
	if err == nil || result.AvailabilityClass != AvailabilityClassNone || !result.ResponseStarted {
		t.Fatalf("partial response was classified as unavailable: result=%+v err=%v", result, err)
	}
}

func TestDelegateCarriesEveryAvailabilityClass(t *testing.T) {
	classes := []AvailabilityClass{
		AvailabilityClassQuotaRateLimit,
		AvailabilityClassCapacity,
		AvailabilityClassCapacityDeadline,
		AvailabilityClassAuthenticationSession,
		AvailabilityClassProviderCLIUnavailable,
		AvailabilityClassStartDeadline,
	}
	for _, class := range classes {
		t.Run(class, func(t *testing.T) {
			client := &BusClient{caller: &recordingCaller{result: InvocationResult{Version: WireVersion,
				Status: "failed", Error: "transport failure", AvailabilityClass: class}}, deadline: time.Second}
			result, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "qa", Prompt: "review"})
			if err == nil || result.AvailabilityClass != class || result.ResponseStarted {
				t.Fatalf("metadata was not preserved: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestDelegateGroupCarriesEveryAvailabilityClass(t *testing.T) {
	classes := []AvailabilityClass{
		AvailabilityClassQuotaRateLimit,
		AvailabilityClassCapacity,
		AvailabilityClassCapacityDeadline,
		AvailabilityClassAuthenticationSession,
		AvailabilityClassProviderCLIUnavailable,
		AvailabilityClassStartDeadline,
	}
	for _, class := range classes {
		t.Run(class, func(t *testing.T) {
			client := &BusClient{caller: &groupRoutingCaller{plannedModels: []string{"agent"},
				invocationResult: InvocationResult{Version: WireVersion, Status: "failed", Error: "transport failure", AvailabilityClass: class}}, deadline: time.Second}
			results := client.DelegateGroup(t.Context(), []DelegateRequest{{Role: "review", Persona: "qa", Prompt: "review"}})
			if len(results) != 1 || results[0].AvailabilityClass != class || results[0].ResponseStarted {
				t.Fatalf("group metadata was not preserved: %+v", results)
			}
		})
	}
}

func TestDelegateGroupPlansDiversityAndKeepsParticipantContinuityLocal(t *testing.T) {
	caller := &groupRoutingCaller{plannedModels: []string{"review-a", "review-b"}}
	client := &BusClient{caller: caller, deadline: time.Second}
	requests := []DelegateRequest{
		{Role: "review", Persona: "security", Prompt: "one"},
		{Role: "review", Persona: "qa", Prompt: "two", Participant: "review-b"},
	}
	results := client.DelegateGroup(t.Context(), requests)
	if len(results) != 2 || results[0].Participant != "review-a" ||
		results[1].Participant != "review-b" {
		t.Fatalf("group results = %+v", results)
	}
	if caller.plan.Seats[1].Model != "review-b" {
		t.Fatalf("participant continuity crossed as %+v instead of a local model pin", caller.plan.Seats[1])
	}
	for _, forbidden := range []string{"participant", "durable_slot", "work_item_id"} {
		body, _ := json.Marshal(caller.plan)
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("caller field %q crossed group wire: %s", forbidden, body)
		}
	}
	slices.Sort(caller.invokedModels)
	if !slices.Equal(caller.invokedModels, []string{"review-a", "review-b"}) {
		t.Fatalf("invoked models = %v", caller.invokedModels)
	}
}

func TestDelegateGroupPreservesCapacityFromTheModuleBoundary(t *testing.T) {
	caller := &groupRoutingCaller{planError: "group saturated: " + ErrDelegateCapacity.Error()}
	client := &BusClient{caller: caller, deadline: time.Second}
	results := client.DelegateGroup(t.Context(), []DelegateRequest{{Role: "review", Persona: "qa", Prompt: "review"}})
	if len(results) != 1 || !errors.Is(results[0].Err, ErrDelegateCapacity) ||
		!IsCapacityBackpressure(results[0].Err) || errors.Is(results[0].Err, ErrDelegateTerminal) {
		t.Fatalf("group capacity lost its load classification: %+v", results)
	}
}

func TestDelegateResultPreservesCapacityWaitDeadline(t *testing.T) {
	caller := &recordingCaller{result: InvocationResult{Version: WireVersion, Status: "failed",
		Error: ErrDelegateCapacityDeadline.Error()}}
	client := &BusClient{caller: caller, deadline: time.Second}
	_, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "qa", Prompt: "review"})
	if !errors.Is(err, ErrDelegateCapacityDeadline) || !errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrDelegateTerminal) {
		t.Fatalf("capacity wait deadline was collapsed into execution failure: %v", err)
	}
}

func TestDelegateResultPreservesExecutionDeadline(t *testing.T) {
	caller := &recordingCaller{result: InvocationResult{Version: WireVersion, Status: "failed",
		Error: ErrDelegateExecutionDeadline.Error()}}
	client := &BusClient{caller: caller, deadline: time.Second}
	_, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "qa", Prompt: "review"})
	if !errors.Is(err, ErrDelegateExecutionDeadline) || !errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrDelegateCapacityDeadline) || errors.Is(err, ErrDelegateTerminal) {
		t.Fatalf("execution deadline was collapsed into another failure: %v", err)
	}
}

func TestDelegateCallerCancellationIsNotRetypedAsDeadline(t *testing.T) {
	caller := &recordingCaller{err: context.Canceled}
	client := &BusClient{caller: caller, deadline: time.Second}
	_, err := client.Delegate(t.Context(), DelegateRequest{Role: "review", Persona: "qa", Prompt: "review"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		IsCapacityDeadline(err) || IsExecutionDeadline(err) {
		t.Fatalf("caller cancellation was retyped at the module/client boundary: %v", err)
	}
}

func TestSafeDiagnosticRedactsCredentialForms(t *testing.T) {
	tests := map[string]string{
		"authorization": "Authorization: Bearer header-secret",
		"cookie":        "Cookie: session=cookie-secret",
		"url userinfo":  "https://user:url-secret@example.test/path",
		"token field":   `{"access_token":"json-secret"}`,
		"aws key":       "AKIA1234567890ABCDEF",
		"jwt":           "eyJheader.payload.signature",
		"private key":   "-----BEGIN PRIVATE KEY-----\nprivate-secret\n-----END PRIVATE KEY-----",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			output := SafeDiagnostic(input)
			for _, secret := range []string{"header-secret", "cookie-secret", "url-secret",
				"json-secret", "AKIA1234567890ABCDEF", "eyJheader.payload.signature", "private-secret"} {
				if strings.Contains(output, secret) {
					t.Fatalf("secret %q survived redaction: %q", secret, output)
				}
			}
			if !strings.Contains(output, "[REDACTED") {
				t.Fatalf("redaction marker missing: %q", output)
			}
		})
	}
}

func TestSafeDiagnosticPreservesWireClassificationSlugs(t *testing.T) {
	for _, err := range []error{ErrDelegateCapacity, ErrDelegateCapacityDeadline, ErrDelegateExecutionDeadline} {
		if got := SafeDiagnostic(err.Error()); got != err.Error() {
			t.Fatalf("wire classification slug changed: got %q want %q", got, err)
		}
	}
}
