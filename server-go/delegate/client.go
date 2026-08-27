// Package delegate is the shared caller-side contract for delegate execution.
// It is deliberately outside modules/: any peer that calls delegates exchanges
// this JSON over the bus without importing the delegates module's implementation.
// Independently exported callers must also be listed by
// scripts/export_c_repositories.py:go_process_shared_sources.
package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JBailes/aimee/server-go/bus"
)

const (
	EventKind      uint32 = 6657
	StageInvoke    uint32 = 1
	EventGroupPlan uint32 = 6678
	StageGroupPlan uint32 = 22
	WireVersion           = 3
)

type DelegateRequest struct {
	Role                 string
	Persona              string
	Delegate             string
	Participant          string
	Prompt               string
	Workdir              string
	Tools                bool
	WorkItemID           string
	Stage                string
	ExecutionVersion     string
	RetryTag             string
	MaxCostUSD           float64
	MaxTurnsCap          int
	ToolLoopTimeoutMSCap int
	ReplayOnly           bool
	DurableSlot          string
	ArtifactStage        string
	ArtifactHash         string
	ProvidedTarget       bool
	AcceptPartial        bool
}

type DelegateResult struct {
	Response          string
	Agent             string
	Participant       string
	CostUSD           float64
	CostUnknown       bool
	Partial           bool
	AvailabilityClass AvailabilityClass
	ResponseStarted   bool
}

type AgentClient interface {
	Delegate(context.Context, DelegateRequest) (DelegateResult, error)
}

type DelegateGroupResult struct {
	Participant       string
	Response          string
	CostUSD           float64
	CostUnknown       bool
	AvailabilityClass AvailabilityClass
	ResponseStarted   bool
	Err               error
}

type DelegateGroupClient interface {
	DelegateGroup(context.Context, []DelegateRequest) []DelegateGroupResult
}

var (
	ErrDelegateTerminal             = errors.New("delegate execution failed")
	ErrDelegateReplayUnavailable    = errors.New("delegate calls are stateless; replay-only execution is unavailable")
	ErrDelegateUnassignedExpired    = errors.New("delegate was not assigned")
	ErrDelegateCancelUnacknowledged = errors.New("delegate cancellation was not acknowledged")
	ErrDelegateCostLimitUnsupported = errors.New("delegate CLI execution cannot enforce a monetary cost limit")
	ErrDelegateCapacity             = errors.New("delegate capacity unavailable [aimee_err=concurrency_limit]")
	ErrDelegateCapacityDeadline     = errors.New("delegate capacity wait deadline exceeded [aimee_err=capacity_deadline]")
	ErrDelegateExecutionDeadline    = errors.New("delegate execution deadline exceeded [aimee_err=execution_deadline]")
)

// AvailabilityClass is the transport-owned recovery class for a delegate
// failure. Empty means the failure is not safe to retry by selecting another
// provider/profile. It is an alias so panel transport contracts can expose the
// field as a plain string without conversion noise.
type AvailabilityClass = string

const (
	AvailabilityClassNone                   = ""
	AvailabilityClassQuotaRateLimit         = "quota_rate_limit"
	AvailabilityClassCapacity               = "capacity"
	AvailabilityClassCapacityDeadline       = "capacity_deadline"
	AvailabilityClassAuthenticationSession  = "authentication_session"
	AvailabilityClassProviderCLIUnavailable = "provider_cli_unavailable"
	AvailabilityClassStartDeadline          = "start_deadline"

	// Compatibility names retained for older in-process callers. They alias the
	// canonical values so the wire accepts only the six approved classes.
	AvailabilityClassProviderQuota       = AvailabilityClassQuotaRateLimit
	AvailabilityClassAuthentication      = AvailabilityClassAuthenticationSession
	AvailabilityClassProviderUnavailable = AvailabilityClassProviderCLIUnavailable
	AvailabilityNone                     = AvailabilityClassNone
	AvailabilityProviderQuota            = AvailabilityClassProviderQuota
	AvailabilityQuotaRateLimit           = AvailabilityClassQuotaRateLimit
	AvailabilityCapacity                 = AvailabilityClassCapacity
	AvailabilityCapacityDeadline         = AvailabilityClassCapacityDeadline
	AvailabilityAuthentication           = AvailabilityClassAuthentication
	AvailabilityAuthenticationSession    = AvailabilityClassAuthenticationSession
	AvailabilityProviderCLIUnavailable   = AvailabilityClassProviderCLIUnavailable
	AvailabilityProviderCliUnavailable   = AvailabilityClassProviderCLIUnavailable
	AvailabilityProviderUnavailable      = AvailabilityClassProviderUnavailable
	AvailabilityStartDeadline            = AvailabilityClassStartDeadline
)

type DelegateExecutionError struct {
	Err               error
	Dispatched        bool
	CostKnown         bool
	CostUSD           float64
	AvailabilityClass AvailabilityClass
	ResponseStarted   bool
}

func (e *DelegateExecutionError) Error() string { return e.Err.Error() }
func (e *DelegateExecutionError) Unwrap() error { return e.Err }

// Invocation is the complete delegate-owned wire request. Workflow replay,
// durable-slot, work-item and retry fields intentionally have no JSON mapping.
type Invocation struct {
	Version  int    `json:"version"`
	Role     string `json:"role"`
	Persona  string `json:"persona"`
	Model    string `json:"model,omitempty"`
	Prompt   string `json:"prompt"`
	Workdir  string `json:"workdir,omitempty"`
	Tools    bool   `json:"tools"`
	MaxTurns int    `json:"max_turns,omitempty"`
	// ExecutionTimeoutMS is an optional delegate-owned run bound. Zero means
	// unbounded; explicit caller context deadlines still propagate here.
	ExecutionTimeoutMS int64 `json:"execution_timeout_ms,omitempty"`
}

type InvocationResult struct {
	Version           int               `json:"version"`
	Status            string            `json:"status"`
	Response          string            `json:"response,omitempty"`
	Agent             string            `json:"agent,omitempty"`
	Error             string            `json:"error,omitempty"`
	CostUSD           float64           `json:"cost_usd,omitempty"`
	CostKnown         bool              `json:"cost_known"`
	AvailabilityClass AvailabilityClass `json:"availability_class,omitempty"`
	ResponseStarted   bool              `json:"response_started"`
}

// GroupPlan carries only delegate-selection inputs. Participant continuity is
// translated caller-side into an explicit model selector before this wire.
type GroupPlan struct {
	Version int             `json:"version"`
	Seats   []GroupPlanSeat `json:"seats"`
}

type GroupPlanSeat struct {
	Role    string `json:"role"`
	Persona string `json:"persona"`
	Model   string `json:"model,omitempty"`
}

type GroupPlanResult struct {
	Version int      `json:"version"`
	Models  []string `json:"models,omitempty"`
	// Error is a delegate-domain failure carried by a successful bus exchange.
	// The bus strips response bodies from non-OK transport replies, so load
	// classifications must travel in this versioned envelope.
	Error             string            `json:"error,omitempty"`
	AvailabilityClass AvailabilityClass `json:"availability_class,omitempty"`
}

// StageCaller is the delegate bus-call seam. The production implementation is
// bus.ConcurrentModuleCaller; exposing the narrow contract also lets integration
// tests cross the real module handler without opening a daemon socket.
type StageCaller interface {
	Call(context.Context, uint32, uint32, uint64, time.Duration, []byte) ([]byte, error)
}

type BusClient struct {
	caller   StageCaller
	deadline time.Duration
	trace    atomic.Uint64
}

func NewBusClient(caller *bus.ConcurrentModuleCaller, deadline time.Duration) (*BusClient, error) {
	return NewClient(caller, deadline)
}

func NewClient(caller StageCaller, deadline time.Duration) (*BusClient, error) {
	if caller == nil {
		return nil, errors.New("delegate bus caller is required")
	}
	return &BusClient{caller: caller, deadline: deadline}, nil
}

func (c *BusClient) Delegate(ctx context.Context, request DelegateRequest) (DelegateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.ReplayOnly {
		// Replay identity remains caller-owned by contract. Tell the workflow
		// engine that this request represents an earlier dispatch so its durable
		// reservation recovery decides whether a fresh independent call is safe.
		return DelegateResult{}, &DelegateExecutionError{Err: ErrDelegateReplayUnavailable,
			Dispatched: true, CostKnown: false}
	}
	if request.MaxCostUSD > 0 {
		// CLI agents do not expose a trustworthy live dollar meter. Refuse before
		// dispatch instead of accepting a ceiling the producer cannot enforce.
		return DelegateResult{}, ErrDelegateCostLimitUnsupported
	}
	if request.Role == "" || request.Persona == "" || request.Prompt == "" {
		return DelegateResult{}, errors.New("delegate role, persona, and prompt are required")
	}
	deadline := c.deadline
	if contextDeadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(contextDeadline)
		if remaining <= 0 {
			err := errors.Join(ErrDelegateTerminal, context.DeadlineExceeded)
			return DelegateResult{AvailabilityClass: AvailabilityClassStartDeadline}, &DelegateExecutionError{
				Err: err, AvailabilityClass: AvailabilityClassStartDeadline}
		}
		if deadline <= 0 || remaining < deadline {
			deadline = remaining
		}
	}
	if request.ToolLoopTimeoutMSCap > 0 {
		cap := time.Duration(request.ToolLoopTimeoutMSCap) * time.Millisecond
		if deadline <= 0 || cap < deadline {
			deadline = cap
		}
	}
	executionTimeoutMS := int64(0)
	if deadline > 0 {
		executionTimeoutMS = max(1, deadline.Milliseconds())
	}
	wire := Invocation{Version: WireVersion, Role: request.Role, Persona: request.Persona,
		Model: request.Delegate, Prompt: request.Prompt, Workdir: request.Workdir,
		Tools: request.Tools, MaxTurns: request.MaxTurnsCap,
		ExecutionTimeoutMS: executionTimeoutMS}
	body, err := json.Marshal(wire)
	if err != nil {
		return DelegateResult{}, err
	}
	reply, err := c.caller.Call(ctx, EventKind, StageInvoke, c.trace.Add(1), deadline, body)
	if err != nil {
		err = classifyDelegateError(err)
		if errors.Is(err, bus.ErrModuleCallCapabilityAbsent) ||
			errors.Is(err, bus.ErrModuleCallRejected) ||
			errors.Is(err, bus.ErrModuleCallNotDispatched) {
			if errors.Is(err, bus.ErrModuleCallCapabilityAbsent) {
				return DelegateResult{AvailabilityClass: AvailabilityClassProviderCLIUnavailable}, err
			}
			return DelegateResult{}, err
		}
		// Once handed to the bus, loss of the reply cannot prove the provider did
		// not run. Preserve that uncertainty at the caller's billing boundary.
		if errors.Is(err, bus.ErrModuleCallDeadline) {
			err = errors.Join(err, context.DeadlineExceeded)
		}
		return DelegateResult{}, &DelegateExecutionError{Err: err, Dispatched: true, CostKnown: false,
			AvailabilityClass: AvailabilityClassOf(err)}
	}
	var result InvocationResult
	if err := json.Unmarshal(reply, &result); err != nil {
		return DelegateResult{}, &DelegateExecutionError{Err: fmt.Errorf("decode delegate result: %w", err),
			Dispatched: true, CostKnown: false}
	}
	if result.Version != WireVersion || (result.Status != "done" && result.Status != "failed") {
		return DelegateResult{}, &DelegateExecutionError{
			Err:        errors.New("delegate module returned an invalid terminal result"),
			Dispatched: true, CostKnown: false}
	}
	if result.Status == "failed" {
		detail := result.Error
		if detail == "" {
			detail = ErrDelegateTerminal.Error()
		}
		failure := classifyDelegateError(errors.New(detail))
		if !errors.Is(failure, ErrDelegateCapacity) && !IsCapacityDeadline(failure) && !IsExecutionDeadline(failure) {
			failure = fmt.Errorf("%w: %s", ErrDelegateTerminal, detail)
		}
		// Older producers did not carry the explicit bit but did include the
		// partial response. Keep that wire version backward-compatible; current
		// producers set the bit only after finalOutput returns usable text.
		responseStarted := result.ResponseStarted || strings.TrimSpace(result.Response) != ""
		availability := AvailabilityClassNone
		if !responseStarted {
			availability = validAvailabilityClass(result.AvailabilityClass)
		}
		return DelegateResult{AvailabilityClass: availability, ResponseStarted: responseStarted}, &DelegateExecutionError{Err: failure,
			Dispatched: true, CostKnown: result.CostKnown, CostUSD: result.CostUSD,
			AvailabilityClass: availability, ResponseStarted: responseStarted}
	}
	participant := request.Participant
	if participant == "" {
		participant = result.Agent
	}
	responseStarted := result.ResponseStarted || strings.TrimSpace(result.Response) != ""
	return DelegateResult{Response: result.Response, Agent: result.Agent, Participant: participant,
		CostUSD: result.CostUSD, CostUnknown: !result.CostKnown,
		AvailabilityClass: AvailabilityClassNone, ResponseStarted: responseStarted}, nil
}

func (c *BusClient) DelegateGroup(ctx context.Context, requests []DelegateRequest) []DelegateGroupResult {
	if ctx == nil {
		ctx = context.Background()
	}
	out := make([]DelegateGroupResult, len(requests))
	if len(requests) == 0 {
		return out
	}
	planned := append([]DelegateRequest(nil), requests...)
	planAvailability := AvailabilityClassNone
	plan := GroupPlan{Version: WireVersion, Seats: make([]GroupPlanSeat, len(planned))}
	for i := range planned {
		model := planned[i].Delegate
		if planned[i].Participant != "" && (model == "" || model == "$random") {
			model = planned[i].Participant
		}
		plan.Seats[i] = GroupPlanSeat{Role: planned[i].Role, Persona: planned[i].Persona, Model: model}
	}
	body, err := json.Marshal(plan)
	if err == nil {
		var reply []byte
		reply, err = c.caller.Call(ctx, EventGroupPlan, StageGroupPlan, c.trace.Add(1), c.deadline, body)
		if err == nil {
			var result GroupPlanResult
			if decodeErr := json.Unmarshal(reply, &result); decodeErr != nil || result.Version != WireVersion {
				err = errors.New("delegate module returned an invalid group plan")
			} else if strings.TrimSpace(result.Error) != "" {
				planAvailability = validAvailabilityClass(result.AvailabilityClass)
				err = errors.New(result.Error)
			} else if len(result.Models) != len(planned) {
				err = errors.New("delegate module returned an invalid group plan")
			} else {
				for i := range planned {
					if strings.TrimSpace(result.Models[i]) == "" {
						err = errors.New("delegate module returned an empty group assignment")
						break
					}
					planned[i].Delegate = result.Models[i]
				}
			}
		}
	}
	if err != nil {
		err = classifyDelegateError(err)
		availability := planAvailability
		if availability == AvailabilityClassNone {
			availability = AvailabilityClassOf(err)
		}
		for i := range out {
			out[i] = DelegateGroupResult{Participant: requests[i].Participant, AvailabilityClass: availability, Err: err}
		}
		return out
	}
	done := make(chan int, len(requests))
	for i := range planned {
		go func(i int) {
			result, err := c.Delegate(ctx, planned[i])
			out[i] = DelegateGroupResult{Participant: result.Participant, Response: result.Response,
				CostUSD: result.CostUSD, CostUnknown: result.CostUnknown,
				AvailabilityClass: result.AvailabilityClass, ResponseStarted: result.ResponseStarted, Err: err}
			done <- i
		}(i)
	}
	for range requests {
		<-done
	}
	return out
}

var diagnosticRedactions = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)(authorization\s*:\s*(?:bearer|basic)\s+)[^\s,;]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(cookie\s*:\s*)[^\r\n]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`), `${1}[REDACTED]@`},
	{regexp.MustCompile(`(?i)("?(?:api[_-]?key|access[_-]?token|token|password|secret)"?\s*[:=]\s*)"(?:\\.|[^"\\])*"`), `${1}"[REDACTED]"`},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|token|password|secret)\s*[:=]\s*)'(?:\\.|[^'\\])*'`), `${1}'[REDACTED]'`},
	{regexp.MustCompile(`(?im)("?(?:api[_-]?key|access[_-]?token|token|password|secret)"?\s*[:=]\s*)"(?:\\.|[^"\\\r\n])*(?:\\)?$`), `${1}"[REDACTED]`},
	{regexp.MustCompile(`(?im)((?:api[_-]?key|access[_-]?token|token|password|secret)\s*[:=]\s*)'(?:\\.|[^'\\\r\n])*(?:\\)?$`), `${1}'[REDACTED]`},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|token|password|secret)["']?\s*[:=]\s*["']?)[^\s,"';}]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), `[REDACTED_AWS_ACCESS_KEY]`},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`), `[REDACTED_JWT]`},
	{regexp.MustCompile(`(?s)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?-----END [^-\r\n]*PRIVATE KEY-----`), `[REDACTED_PRIVATE_KEY]`},
}

func SafeDiagnostic(detail string) string {
	for _, redaction := range diagnosticRedactions {
		detail = redaction.pattern.ReplaceAllString(detail, redaction.replacement)
	}
	return detail
}

func SafeDiagnosticSummary(detail string, maxBytes int) string {
	detail = strings.Join(strings.Fields(SafeDiagnostic(detail)), " ")
	if maxBytes <= 0 || len(detail) <= maxBytes {
		return detail
	}
	const marker = " … [truncated] … "
	if maxBytes <= len(marker) {
		return strings.ToValidUTF8(detail[:maxBytes], "")
	}
	available := maxBytes - len(marker)
	prefix := available * 3 / 5
	return strings.ToValidUTF8(detail[:prefix], "") + marker + strings.ToValidUTF8(detail[len(detail)-(available-prefix):], "")
}

func IsCapacityBackpressure(err error) bool {
	if err == nil {
		return false
	}
	detail := err.Error()
	return strings.Contains(detail, "aimee_err=concurrency_limit") ||
		strings.Contains(detail, "is rate-limited; retry in")
}

// IsCapacityDeadline recognizes both the in-process sentinel and its stable
// bus-safe slug. Module status errors cross a process boundary and therefore
// cannot preserve Go error identity without this explicit wire vocabulary.
func IsCapacityDeadline(err error) bool {
	return err != nil && (errors.Is(err, ErrDelegateCapacityDeadline) ||
		strings.Contains(err.Error(), "aimee_err=capacity_deadline"))
}

func IsExecutionDeadline(err error) bool {
	return err != nil && (errors.Is(err, ErrDelegateExecutionDeadline) ||
		strings.Contains(err.Error(), "aimee_err=execution_deadline"))
}

func validAvailabilityClass(class AvailabilityClass) AvailabilityClass {
	switch class {
	case AvailabilityClassQuotaRateLimit, AvailabilityClassCapacity,
		AvailabilityClassCapacityDeadline, AvailabilityClassAuthenticationSession,
		AvailabilityClassProviderCLIUnavailable, AvailabilityClassStartDeadline:
		return class
	default:
		return AvailabilityClassNone
	}
}

// AvailabilityClassOf returns only transport metadata carried by a delegate
// execution error. It deliberately does not infer provider availability from
// arbitrary terminal diagnostics; only the typed capacity sentinels are safe
// to derive locally.
func AvailabilityClassOf(err error) AvailabilityClass {
	if err == nil {
		return AvailabilityClassNone
	}
	var execution *DelegateExecutionError
	if errors.As(err, &execution) {
		if class := validAvailabilityClass(execution.AvailabilityClass); class != AvailabilityClassNone {
			return class
		}
	}
	if errors.Is(err, bus.ErrModuleCallCapabilityAbsent) {
		return AvailabilityClassProviderCLIUnavailable
	}
	if IsCapacityDeadline(err) {
		return AvailabilityClassCapacityDeadline
	}
	if errors.Is(err, ErrDelegateCapacity) {
		return AvailabilityClassCapacity
	}
	return AvailabilityClassNone
}

// CarriedAvailabilityClass is an explicit-name alias for callers that prefer
// to document that the class came from the transport envelope.
func CarriedAvailabilityClass(err error) AvailabilityClass { return AvailabilityClassOf(err) }

// ClassifyProviderAvailability is used only by the delegate producer, which
// owns CLI/provider diagnostics. The client-side ClassifyAvailability below
// never infers a class from arbitrary terminal text.
func ClassifyProviderAvailability(err error, responseStarted bool) AvailabilityClass {
	if err == nil || responseStarted || errors.Is(err, ErrDelegateReplayUnavailable) {
		return AvailabilityClassNone
	}
	if errors.Is(err, bus.ErrModuleCallCapabilityAbsent) {
		return AvailabilityClassProviderCLIUnavailable
	}
	if errors.Is(err, ErrDelegateCapacity) || IsCapacityDeadline(err) {
		if IsCapacityDeadline(err) {
			return AvailabilityClassCapacityDeadline
		}
		return AvailabilityClassCapacity
	}
	detail := strings.ToLower(err.Error())
	if strings.Contains(detail, "capacity deadline") || strings.Contains(detail, "capacity-deadline") ||
		strings.Contains(detail, "aimee_err=capacity_deadline") {
		return AvailabilityClassCapacityDeadline
	}
	if strings.Contains(detail, "aimee_err=concurrency_limit") ||
		strings.Contains(detail, "capacity unavailable") || strings.Contains(detail, "capacity saturated") {
		return AvailabilityClassCapacity
	}
	if strings.Contains(detail, "quota") || strings.Contains(detail, "rate limit") ||
		strings.Contains(detail, "rate-limit") || strings.Contains(detail, "rate_limit") ||
		strings.Contains(detail, "rate_limited") || strings.Contains(detail, "rate limited") ||
		strings.Contains(detail, "hit your session limit") ||
		strings.Contains(detail, "too many requests") || strings.Contains(detail, "429") ||
		strings.Contains(detail, "credits exhausted") || strings.Contains(detail, "throttled") {
		return AvailabilityClassQuotaRateLimit
	}
	if strings.Contains(detail, "authentication") || strings.Contains(detail, "authorization") ||
		strings.Contains(detail, "unauthorized") ||
		strings.Contains(detail, "invalid api key") || strings.Contains(detail, "api key") ||
		strings.Contains(detail, "login") || strings.Contains(detail, "credential") ||
		strings.Contains(detail, "session expired") || strings.Contains(detail, "expired session") || strings.Contains(detail, "expired-session") ||
		strings.Contains(detail, "session unavailable") || strings.Contains(detail, "session is unavailable") || strings.Contains(detail, "session outage") ||
		strings.Contains(detail, "session not found") || strings.Contains(detail, "no active session") ||
		strings.Contains(detail, "token expired") || strings.Contains(detail, "invalid token") ||
		strings.Contains(detail, "credentials") {
		return AvailabilityClassAuthenticationSession
	}
	if strings.Contains(detail, "provider unavailable") || strings.Contains(detail, "provider is unavailable") ||
		strings.Contains(detail, "unavailable provider") ||
		strings.Contains(detail, "provider not found") || strings.Contains(detail, "cli unavailable") ||
		strings.Contains(detail, "command not found") || strings.Contains(detail, "executable file not found") ||
		strings.Contains(detail, "no such file or directory") || strings.Contains(detail, "not installed") ||
		strings.Contains(detail, "missing provider") || strings.Contains(detail, "no enabled delegate cli") ||
		strings.Contains(detail, "no enabled cli") || strings.Contains(detail, "not an enabled cli agent") ||
		strings.Contains(detail, "cli not found") || strings.Contains(detail, "provider not configured") ||
		strings.Contains(detail, "no eligible healthy backend") {
		return AvailabilityClassProviderCLIUnavailable
	}
	if IsExecutionDeadline(err) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(detail, "execution deadline exceeded") {
		return AvailabilityClassStartDeadline
	}
	return AvailabilityClassNone
}

// ClassifyAvailability returns only transport-carried metadata and typed
// capacity. It is safe to use at the client boundary because it does not
// inspect arbitrary terminal error text.
func ClassifyAvailability(err error, responseStarted bool) AvailabilityClass {
	if responseStarted {
		return AvailabilityClassNone
	}
	class := AvailabilityClassOf(err)
	// The pre-v3 diagnostic slug was capacity-wide. Preserve that helper's
	// historical result for callers that still pass the raw slug; typed wire
	// failures use the distinct capacity_deadline class above.
	if class == AvailabilityClassCapacityDeadline && !errors.Is(err, ErrDelegateCapacityDeadline) {
		return AvailabilityClassCapacity
	}
	return class
}

// classifyDelegateError preserves the specific typed deadline sentinel even
// though both deadline classes intentionally unwrap to context.DeadlineExceeded.
func classifyDelegateError(err error) error {
	switch {
	case err == nil:
		return nil
	case IsCapacityDeadline(err):
		return errors.Join(ErrDelegateCapacityDeadline, context.DeadlineExceeded, err)
	case IsCapacityBackpressure(err):
		return errors.Join(ErrDelegateCapacity, err)
	case IsExecutionDeadline(err):
		return errors.Join(ErrDelegateExecutionDeadline, context.DeadlineExceeded, err)
	default:
		return err
	}
}
