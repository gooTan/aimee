package delegates

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JBailes/aimee/server-go/bus"
	delegatecontract "github.com/JBailes/aimee/server-go/delegate"
	"golang.org/x/sys/unix"
)

const maxExecutorOutput = 16 << 20
const maxExecutorDiagnostic = 1024

const (
	watchdogArgument = "__aimee_delegate_watchdog"
	watchdogArgvEnv  = "AIMEE_DELEGATE_WATCHDOG_ARGV"
)

type Executor interface {
	Execute(context.Context, delegatecontract.Invocation) delegatecontract.InvocationResult
}

// NewDefaultHandler is the independently exported delegates process entry
// point. The shared multicall binary performs the same construction explicitly
// so it can report registry errors in its process log.
func NewDefaultHandler() bus.ModuleHandler {
	executor, err := NewRegistryExecutor(os.Getenv("AIMEE_HOME"))
	if err != nil {
		return NewHandler(nil)
	}
	return NewHandler(executor)
}

type agentEntry struct {
	Name             string   `json:"name"`
	Model            string   `json:"model"`
	Backend          string   `json:"backend"`
	Provider         string   `json:"provider"`
	CLIKind          string   `json:"cli_kind"`
	CLICmd           string   `json:"cli_cmd"`
	Enabled          *bool    `json:"enabled"`
	PrimaryOnly      bool     `json:"primary_only"`
	Roles            []string `json:"roles"`
	Personas         []string `json:"personas"`
	MaxParallel      *int     `json:"max_parallel"`
	Available        *bool    `json:"delegate_available"`
	ReasoningEffort  string   `json:"reasoning_effort"`
	CLIIdleTimeoutMS *int64   `json:"cli_idle_timeout_ms"`
}

type agentRegistry struct {
	DefaultAgent string       `json:"default_agent"`
	Agents       []agentEntry `json:"agents"`
	Models       []agentEntry `json:"models"`
}

// RegistryExecutor is the Go-owned delegate producer. It selects a configured
// agent and runs its argv directly; there is no shell, HTTP agent service, job
// row, status poller, or C agent loop in this path.
type RegistryExecutor struct {
	path     string
	mu       sync.Mutex
	registry agentRegistry
	stamp    int64
	limitMu  sync.Mutex
	limits   map[string]*agentLimiter
}

type agentLimiter struct {
	mu      sync.Mutex
	max     int
	inUse   int
	changed chan struct{}
}

func NewRegistryExecutor(home string) (*RegistryExecutor, error) {
	if home == "" {
		return nil, errors.New("AIMEE_HOME is required for delegate execution")
	}
	models := filepath.Join(home, "models.json")
	if _, err := os.Stat(models); errors.Is(err, os.ErrNotExist) {
		models = filepath.Join(home, "agents.json")
	}
	r := &RegistryExecutor{path: models, limits: make(map[string]*agentLimiter)}
	if _, err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RegistryExecutor) load() (agentRegistry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, err := os.Stat(r.path)
	if err != nil {
		return agentRegistry{}, fmt.Errorf("stat agent registry: %w", err)
	}
	if r.stamp == info.ModTime().UnixNano() && len(r.registry.Agents) > 0 {
		return r.registry, nil
	}
	body, err := os.ReadFile(r.path)
	if err != nil {
		return agentRegistry{}, fmt.Errorf("read agent registry: %w", err)
	}
	var registry agentRegistry
	if err := json.Unmarshal(body, &registry); err != nil {
		return agentRegistry{}, fmt.Errorf("decode agent registry: %w", err)
	}
	if len(registry.Agents) > 0 && len(registry.Models) > 0 {
		return agentRegistry{}, fmt.Errorf("decode agent registry: registry must use a single top-level key: both \"agents\" and \"models\" are populated")
	}
	if len(registry.Agents) == 0 && len(registry.Models) > 0 {
		registry.Agents = registry.Models
	}
	registry.Models = nil
	if len(registry.Agents) == 0 {
		return agentRegistry{}, errors.New("agent registry has no agents")
	}
	for _, agent := range registry.Agents {
		if agent.MaxParallel != nil && *agent.MaxParallel < 0 {
			return agentRegistry{}, fmt.Errorf("agent %q has negative max_parallel", agent.Name)
		}
		if agent.CLIIdleTimeoutMS != nil && (*agent.CLIIdleTimeoutMS < 0 || *agent.CLIIdleTimeoutMS > int64(24*time.Hour/time.Millisecond)) {
			return agentRegistry{}, fmt.Errorf("agent %q has invalid cli_idle_timeout_ms %d", agent.Name, *agent.CLIIdleTimeoutMS)
		}
		if strings.TrimSpace(agent.CLICmd) != "" && strings.TrimSpace(agent.CLIKind) == "" {
			return agentRegistry{}, fmt.Errorf("agent %q with cli_cmd requires cli_kind", agent.Name)
		}
	}
	r.registry, r.stamp = registry, info.ModTime().UnixNano()
	return registry, nil
}

func enabled(a agentEntry) bool { return a.Enabled == nil || *a.Enabled }

func available(a agentEntry) bool { return a.Available == nil || *a.Available }

func containsOrWildcard(values []string, wanted string, emptyMeansAll bool) bool {
	if len(values) == 0 {
		return emptyMeansAll
	}
	for _, value := range values {
		if value == "all" || value == wanted {
			return true
		}
	}
	return false
}

func eligible(a agentEntry, role, persona string) bool {
	return containsOrWildcard(a.Roles, role, false) && containsOrWildcard(a.Personas, persona, true)
}

func agentMaxParallel(a agentEntry) int {
	if a.MaxParallel == nil {
		return 3 // AGENT_DEFAULT_MAX_PARALLEL
	}
	return *a.MaxParallel // zero is the configured unlimited form
}

func (r *RegistryExecutor) limiter(agent agentEntry) *agentLimiter {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if r.limits == nil {
		r.limits = make(map[string]*agentLimiter)
	}
	limiter := r.limits[agent.Name]
	if limiter == nil {
		limiter = &agentLimiter{changed: make(chan struct{})}
		r.limits[agent.Name] = limiter
	}
	limiter.mu.Lock()
	max := agentMaxParallel(agent)
	if limiter.max != max {
		limiter.max = max
		close(limiter.changed)
		limiter.changed = make(chan struct{})
	}
	limiter.mu.Unlock()
	return limiter
}

func (l *agentLimiter) acquire(ctx context.Context) (func(), error) {
	for {
		l.mu.Lock()
		if l.max <= 0 || l.inUse < l.max {
			l.inUse++
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					l.inUse--
					close(l.changed)
					l.changed = make(chan struct{})
					l.mu.Unlock()
				})
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, errors.Join(delegatecontract.ErrDelegateCapacityDeadline, ctx.Err())
			}
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (l *agentLimiter) occupancy() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inUse
}

func selectorUnbound(selector string) bool {
	return selector == "" || selector == "$random"
}

func agentMatchesSelector(agent agentEntry, selector string) bool {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return false
	}
	if selector == agent.Name || selector == agent.Model {
		return true
	}
	if strings.TrimSpace(agent.Model) == "" {
		return false
	}
	model := strings.TrimSpace(agent.Model)
	if cli := strings.TrimSpace(agent.CLIKind); cli != "" && selector == cli+":"+model {
		return true
	}
	if provider := strings.TrimSpace(agent.Provider); provider != "" && selector == provider+":"+model {
		return true
	}
	return false
}

type groupCandidate struct {
	agent    agentEntry
	provider string
	model    string
	active   int
	max      int
}

func groupCandidateLess(a, b groupCandidate, providerUses, modelUses, agentUses map[string]int) bool {
	as := providerUses[a.provider] + modelUses[a.model]
	bs := providerUses[b.provider] + modelUses[b.model]
	if as != bs {
		return as < bs
	}
	if agentUses[a.agent.Name] != agentUses[b.agent.Name] {
		return agentUses[a.agent.Name] < agentUses[b.agent.Name]
	}
	return a.agent.Name < b.agent.Name
}

// PlanGroup chooses the complete seat assignment before any provider work is
// launched. It preserves role/persona eligibility, primary exclusion,
// provider/model diversity, current occupancy, explicit pins and per-agent
// capacity. Returned agent names are opaque participant handles to callers.
func (r *RegistryExecutor) PlanGroup(ctx context.Context,
	seats []delegatecontract.GroupPlanSeat) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	registry, err := r.load()
	if err != nil {
		return nil, err
	}
	candidates := make([]groupCandidate, 0, len(registry.Agents))
	bySelector := make(map[string]groupCandidate)
	for _, agent := range registry.Agents {
		if !enabled(agent) || !available(agent) || agent.PrimaryOnly || strings.TrimSpace(agent.CLICmd) == "" {
			continue
		}
		provider, model := strings.TrimSpace(agent.Provider), strings.TrimSpace(agent.Model)
		if provider == "" {
			provider = agent.Name
		}
		if model == "" {
			model = agent.Name
		}
		candidate := groupCandidate{agent: agent, provider: provider, model: model,
			active: r.limiter(agent).occupancy(), max: agentMaxParallel(agent)}
		candidates = append(candidates, candidate)
		bySelector[agent.Name] = candidate
		if strings.TrimSpace(agent.Model) != "" {
			bySelector[agent.Model] = candidate
			if cli := strings.TrimSpace(agent.CLIKind); cli != "" {
				bySelector[cli+":"+strings.TrimSpace(agent.Model)] = candidate
			}
			if providerKey := strings.TrimSpace(agent.Provider); providerKey != "" {
				bySelector[providerKey+":"+strings.TrimSpace(agent.Model)] = candidate
			}
		}
	}
	models := make([]string, len(seats))
	providerUses, modelUses, agentUses := map[string]int{}, map[string]int{}, map[string]int{}
	assign := func(index int, candidate groupCandidate) error {
		if candidate.max > 0 && candidate.active+agentUses[candidate.agent.Name] >= candidate.max {
			return fmt.Errorf("%w: delegate group exceeds %s max_parallel=%d",
				delegatecontract.ErrDelegateCapacity, candidate.agent.Name, candidate.max)
		}
		models[index] = candidate.agent.Name
		providerUses[candidate.provider]++
		modelUses[candidate.model]++
		agentUses[candidate.agent.Name]++
		return nil
	}
	for i, seat := range seats {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if selectorUnbound(seat.Model) {
			continue
		}
		candidate, exists := bySelector[seat.Model]
		if !exists || !eligible(candidate.agent, seat.Role, seat.Persona) {
			return nil, fmt.Errorf("delegate model %q is not eligible for %s/%s", seat.Model, seat.Role, seat.Persona)
		}
		if err := assign(i, candidate); err != nil {
			return nil, err
		}
	}
	for i, seat := range seats {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if models[i] != "" {
			continue
		}
		best := -1
		hasEligible := false
		for j, candidate := range candidates {
			if !eligible(candidate.agent, seat.Role, seat.Persona) {
				continue
			}
			hasEligible = true
			if candidate.max > 0 && candidate.active+agentUses[candidate.agent.Name] >= candidate.max {
				continue
			}
			if best < 0 || groupCandidateLess(candidate, candidates[best],
				providerUses, modelUses, agentUses) {
				best = j
			}
		}
		if best < 0 {
			if hasEligible {
				// Eligible candidates filtered out only by occupancy are retryable load.
				return nil, fmt.Errorf("%w: delegate group cannot fill seat %d (%s/%s) within enabled capacity",
					delegatecontract.ErrDelegateCapacity, i+1, seat.Role, seat.Persona)
			}
			// No eligible healthy candidate is reachability, deliberately not capacity.
			return nil, fmt.Errorf("delegate group has no eligible healthy backend for seat %d (%s/%s)",
				i+1, seat.Role, seat.Persona)
		}
		if err := assign(i, candidates[best]); err != nil {
			return nil, err
		}
	}
	return models, nil
}

func selectAgent(registry agentRegistry, model, role, persona string) (agentEntry, error) {
	name := strings.TrimSpace(model)
	explicit := name != "" && name != "$random"
	if name == "" || name == "$random" {
		name = strings.TrimSpace(registry.DefaultAgent)
	}
	if name != "" {
		for _, a := range registry.Agents {
			if enabled(a) && available(a) && !a.PrimaryOnly && strings.TrimSpace(a.CLICmd) != "" && eligible(a, role, persona) &&
				agentMatchesSelector(a, name) {
				return a, nil
			}
		}
		if explicit {
			return agentEntry{}, fmt.Errorf("delegate model %q is not an enabled CLI agent", name)
		}
	}
	for _, a := range registry.Agents {
		if enabled(a) && available(a) && !a.PrimaryOnly && strings.TrimSpace(a.CLICmd) != "" &&
			eligible(a, role, persona) {
			return a, nil
		}
	}
	return agentEntry{}, errors.New("no enabled delegate CLI is configured")
}

func splitCommand(command string) ([]string, error) {
	var out []string
	var word strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	for _, ch := range strings.TrimSpace(command) {
		if escaped {
			word.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case ' ', '\t', '\n':
			flush()
		case ';', '|', '&', '<', '>', '`':
			return nil, errors.New("delegate CLI command contains a shell operator")
		default:
			word.WriteRune(ch)
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("delegate CLI command has an unterminated escape or quote")
	}
	flush()
	if len(out) == 0 {
		return nil, errors.New("delegate CLI command is empty")
	}
	return out, nil
}

func executorArgv(agent agentEntry, request delegatecontract.Invocation, prompt string) ([]string, error) {
	argv, err := splitCommand(agent.CLICmd)
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(agent.CLIKind))
	switch kind {
	case "codex":
		if !request.Tools && RoleIsWrite(request.Role) {
			return nil, errors.New("codex CLI cannot guarantee a tools-disabled invocation")
		}
		argv = append(argv, "exec", "--ephemeral", "--json", "--skip-git-repo-check", "--color", "never")
		sandboxMode := "read-only"
		if RoleIsWrite(request.Role) {
			sandboxMode = "workspace-write"
		}
		approvalPolicy := "on-request"
		argv = append(argv, "-c", fmt.Sprintf("approval_policy=%q", approvalPolicy))
		argv = append(argv, "-c", `approvals_reviewer="auto_review"`)
		argv = append(argv, "-c", fmt.Sprintf("sandbox_mode=%q", sandboxMode))
		if agent.Model != "" {
			argv = append(argv, "--model", agent.Model)
		}
		argv = append(argv, "-c", `plugins."aimee@local".mcp_servers.aimee.default_tools_approval_mode="approve"`)
		argv = append(argv, "-")
	case "claude", "claude-code":
		argv = append(argv, "-p", "--output-format", "stream-json", "--verbose")
		if agent.Model != "" {
			argv = append(argv, "--model", agent.Model)
		}
		if !request.Tools {
			// --tools "" disables built-ins; strict MCP also ignores user/project
			// MCP settings, which otherwise remain callable in a tools-off turn.
			argv = append(argv, "--tools", "", "--strict-mcp-config")
		} else {
			argv = append(argv, "--mcp-config",
				`{"mcpServers":{"aimee":{"command":"aimee","args":["mcp-serve"]}}}`)
			if RoleIsWrite(request.Role) {
				argv = append(argv, "--allowedTools", "mcp__aimee")
			} else {
				argv = append(argv, "--allowedTools", "Read", "Glob", "Grep", "mcp__aimee")
			}
		}
	case "acp":
		return argv, nil
	case "agy":
		if !request.Tools {
			return nil, errors.New("agy CLI cannot guarantee a tools-disabled invocation")
		}
		argv = append(argv, "-p", prompt, "--output-format", "stream-json",
			"--disable-slash-commands", "--mode", "plan", "--sandbox",
			"--dangerously-skip-permissions")
		if agent.Model != "" {
			argv = append(argv, "--model", agent.Model)
		}
	case "", "generic":
		// A generic adapter consumes the prompt on stdin and writes its final
		// response on stdout. This keeps custom CLIs argv-only and testable.
		if !RoleIsWrite(request.Role) {
			return nil, errors.New("generic delegate CLIs cannot prove read-only role isolation")
		}
		if !request.Tools {
			return nil, errors.New("generic delegate CLIs cannot guarantee a tools-disabled invocation")
		}
		if request.MaxTurns > 0 {
			return nil, errors.New("generic delegate CLIs cannot enforce a maximum turn count")
		}
	default:
		return nil, fmt.Errorf("unsupported delegate cli_kind %q", agent.CLIKind)
	}
	return argv, nil
}

type limitedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	remaining int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.remaining <= 0 {
		return n, nil
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	_, _ = b.Buffer.Write(p)
	b.remaining -= len(p)
	return n, nil
}

func (b *limitedBuffer) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.mu.Lock()
			toWrite := n
			if toWrite > b.remaining {
				toWrite = b.remaining
			}
			if toWrite > 0 {
				_, _ = b.Buffer.Write(buf[:toWrite])
				b.remaining -= toWrite
			}
			b.mu.Unlock()
			total += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func (b *limitedBuffer) BytesCopy() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.Buffer.Bytes()...)
}

// turnMonitor enforces MaxTurns from observable JSONL events. Claude emits one
// assistant event per model turn. Codex exec exposes one initial model turn and
// completed non-reasoning/non-final items for tool work; counting each such
// item as another turn is conservative when tools run in parallel, so it may
// stop early but can never permit more model continuations than the cap.
type turnMonitor struct {
	mu       sync.Mutex
	kind     string
	max      int
	turns    int
	pending  []byte
	exceeded bool
	cancel   context.CancelFunc
	output   io.Writer
}

func newTurnMonitor(kind string, max int, cancel context.CancelFunc, output io.Writer) *turnMonitor {
	turns := 0
	if strings.EqualFold(strings.TrimSpace(kind), "codex") && max > 0 {
		turns = 1
	}
	return &turnMonitor{kind: strings.ToLower(strings.TrimSpace(kind)), max: max,
		turns: turns, cancel: cancel, output: output}
}

func (m *turnMonitor) Write(p []byte) (int, error) {
	n, err := m.output.Write(p)
	m.mu.Lock()
	m.pending = append(m.pending, p...)
	for {
		end := bytes.IndexByte(m.pending, '\n')
		if end < 0 {
			break
		}
		line := append([]byte(nil), m.pending[:end]...)
		m.pending = m.pending[end+1:]
		m.observe(line)
	}
	m.mu.Unlock()
	return n, err
}

func (m *turnMonitor) observe(line []byte) {
	if m.max <= 0 || m.exceeded {
		return
	}
	var item map[string]any
	if json.Unmarshal(line, &item) != nil {
		return
	}
	switch m.kind {
	case "claude", "claude-code":
		if item["type"] == "assistant" {
			m.turns++
		}
	case "codex":
		if item["type"] == "item.completed" {
			nested, _ := item["item"].(map[string]any)
			typeName, _ := nested["type"].(string)
			if typeName != "agent_message" && typeName != "reasoning" {
				m.turns++
			}
		}
	}
	if m.turns > m.max {
		m.exceeded = true
		m.cancel()
	}
}

func (m *turnMonitor) Exceeded() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exceeded
}

func preResponseFailureDetail(kind string, output []byte, err error) string {
	if final := finalOutput(kind, output); strings.TrimSpace(final) != "" {
		return final
	}
	if detail := streamErrorDetail(kind, output); detail != "" {
		return detail
	}
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return detail
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func streamErrorDetail(kind string, output []byte) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxExecutorOutput)
	var detail string
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) != nil {
			continue
		}
		if kind == "agy" && item["event"] == "error" {
			if value, ok := item["message"].(string); ok {
				detail = value
			}
		}
	}
	return strings.TrimSpace(detail)
}

func streamFailureDetail(kind string, output []byte) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "claude" && kind != "claude-code" && kind != "agy" {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxExecutorOutput)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) != nil {
			continue
		}
		if kind == "agy" {
			if item["event"] != "result" {
				continue
			}
			nested, _ := item["result"].(map[string]any)
			status, _ := nested["status"].(string)
			if strings.EqualFold(strings.TrimSpace(status), "SUCCESS") {
				continue
			}
			if detail, _ := nested["error"].(string); strings.TrimSpace(detail) != "" {
				return detail
			}
			if status == "" {
				status = "unknown"
			}
			return "agy result status " + status
		}
		if item["type"] != "result" {
			continue
		}
		result, _ := item["result"].(string)
		failed, _ := item["is_error"].(bool)
		if subtype, _ := item["subtype"].(string); strings.HasPrefix(subtype, "error_") {
			failed = true
		}
		if status, ok := item["api_error_status"]; ok && status != nil {
			value, numeric := status.(float64)
			failed = failed || !numeric || value != 0
		}
		if reason, _ := item["terminal_reason"].(string); reason != "" && reason != "completed" {
			failed = true
		}
		if claudeLegacyTerminalResult(result) {
			failed = true
		}
		if failed {
			if strings.TrimSpace(result) != "" {
				return result
			}
			return "Claude CLI returned a terminal error"
		}
	}
	return ""
}

func claudeLegacyTerminalResult(result string) bool {
	result = strings.ToLower(strings.TrimSpace(result))
	for _, prefix := range []string{
		"login expired",
		"you have hit your session limit",
		"you've hit your session limit",
		"you have hit your usage limit",
		"you've hit your usage limit",
		"you have reached your usage limit",
		"you've reached your usage limit",
		"reached your usage limit",
		"usage limit for this billing cycle",
	} {
		if strings.HasPrefix(result, prefix) {
			return true
		}
	}
	return false
}

func finalOutput(kind string, output []byte) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" || kind == "generic" {
		return strings.TrimSpace(string(output))
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxExecutorOutput)
	var final string
	var agyStepResponse string
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) != nil {
			continue
		}
		if kind == "claude" || kind == "claude-code" {
			if item["type"] == "result" {
				if value, ok := item["result"].(string); ok {
					final = value
				}
			}
			continue
		}
		if kind == "codex" && item["type"] == "item.completed" {
			if nested, ok := item["item"].(map[string]any); ok && nested["type"] == "agent_message" {
				if value, ok := nested["text"].(string); ok {
					final = value
				}
			}
		}
		if kind == "agy" {
			if item["event"] == "step_update" {
				nested, _ := item["step_update"].(map[string]any)
				if nested["step_type"] == "agent_response" {
					if value, ok := nested["text_delta"].(string); ok && strings.TrimSpace(value) != "" {
						agyStepResponse += value
					}
				}
			}
			if item["event"] == "result" {
				if nested, ok := item["result"].(map[string]any); ok {
					if value, ok := nested["response"].(string); ok && strings.TrimSpace(value) != "" {
						final = value
					}
				}
			}
		}
	}
	if kind == "agy" && strings.TrimSpace(final) == "" {
		final = agyStepResponse
	}
	return strings.TrimSpace(final)
}

func outputResponseStarted(kind string, output []byte) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" || kind == "generic" {
		return strings.TrimSpace(string(output)) != ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxExecutorOutput)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) != nil {
			continue
		}
		switch kind {
		case "claude", "claude-code":
			if item["type"] == "assistant" && item["is_api_error_message"] != true {
				return true
			}
		case "codex":
			if item["type"] == "item.completed" {
				nested, _ := item["item"].(map[string]any)
				if nested["type"] == "agent_message" {
					return true
				}
			}
		case "agy":
			if item["event"] == "step_update" {
				nested, _ := item["step_update"].(map[string]any)
				if nested["step_type"] == "agent_response" {
					if value, ok := nested["text_delta"].(string); ok && strings.TrimSpace(value) != "" {
						return true
					}
				}
			}
			if item["event"] == "result" {
				if nested, ok := item["result"].(map[string]any); ok {
					if value, ok := nested["response"].(string); ok && strings.TrimSpace(value) != "" {
						return true
					}
				}
			}
		}
	}
	return false
}

// RunWatchdog handles the private re-exec mode used by executorCommand. Every
// delegates process entry point calls this before parsing normal arguments.
func RunWatchdog(args []string) (bool, int) {
	if len(args) != 2 || args[1] != watchdogArgument {
		return false, 0
	}
	var argv []string
	if json.Unmarshal([]byte(os.Getenv(watchdogArgvEnv)), &argv) != nil || len(argv) == 0 {
		return true, 125
	}
	control := os.NewFile(3, "delegate-producer-lifecycle")
	if control == nil {
		return true, 125
	}
	defer control.Close()
	syscall.CloseOnExec(int(control.Fd()))
	return true, runWatchdog(control, argv, os.Stdin, os.Stdout, os.Stderr)
}

func runWatchdog(control io.Reader, argv []string, stdin io.Reader,
	stdout, stderr io.Writer) int {
	// Reparent grandchildren here so killing the process group cannot leave
	// zombies behind after the CLI leader exits.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		_, _ = fmt.Fprintf(stderr, "configure delegate watchdog: %v\n", err)
		return 125
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	// The CLI leads a fresh group. Pdeathsig covers an unexpected watchdog
	// failure; the producer pipe below covers module death and kills the group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(stderr, "start delegate CLI: %v\n", err)
		return 125
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	producerGone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, control)
		close(producerGone)
	}()
	select {
	case err := <-done:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		terminateWatchdogChildren()
		if err == nil {
			return 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() >= 0 {
			return exit.ExitCode()
		}
		return 125
	case <-producerGone:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		terminateWatchdogChildren()
		return 125
	}
}

func terminateWatchdogChildren() {
	childrenPath := fmt.Sprintf("/proc/%d/task/%d/children", os.Getpid(), os.Getpid())
	for {
		children, err := os.ReadFile(childrenPath)
		if err != nil || len(strings.Fields(string(children))) == 0 {
			return
		}
		for _, field := range strings.Fields(string(children)) {
			if pid, parseErr := strconv.Atoi(field); parseErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		var status syscall.WaitStatus
		if _, err = syscall.Wait4(-1, &status, 0, nil); errors.Is(err, syscall.ECHILD) {
			return
		} else if err != nil && !errors.Is(err, syscall.EINTR) {
			return
		}
	}
}

func executorCommand(ctx context.Context, argv []string) (*exec.Cmd, func(), error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	encoded, err := json.Marshal(argv)
	if err != nil {
		_ = controlRead.Close()
		_ = controlWrite.Close()
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, executable, watchdogArgument)
	cmd.Env = append(os.Environ(), watchdogArgvEnv+"="+string(encoded))
	cmd.ExtraFiles = []*os.File{controlRead}
	var closeOnce sync.Once
	closeControl := func() {
		closeOnce.Do(func() {
			_ = controlWrite.Close()
			_ = controlRead.Close()
		})
	}
	cmd.Cancel = func() error {
		closeControl()
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd, closeControl, nil
}

func prepareReadOnlyCodexHome(registryHome string) (string, error) {
	source := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if source == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		source = filepath.Join(userHome, ".codex")
	}
	base := filepath.Join(registryHome, "runtime", "codex-homes")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	scratch, err := os.MkdirTemp(base, "read-only-")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = os.RemoveAll(scratch)
		return "", err
	}
	for _, name := range []string{"auth.json", "config.toml"} {
		body, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			return fail(fmt.Errorf("prepare Codex sandbox %s: %w", name, err))
		}
		if err := os.WriteFile(filepath.Join(scratch, name), body, 0o600); err != nil {
			return fail(err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(source, "hooks.json")); err == nil {
		if err := os.WriteFile(filepath.Join(scratch, "hooks.json"), body, 0o600); err != nil {
			return fail(err)
		}
	}
	for _, name := range []string{"plugins", "skills"} {
		if _, err := os.Stat(filepath.Join(source, name)); err == nil {
			if err := os.Symlink(filepath.Join(source, name), filepath.Join(scratch, name)); err != nil {
				return fail(err)
			}
		}
	}
	for _, name := range []string{"cache", "sessions", "shell_snapshots", "tmp", "log"} {
		if err := os.Mkdir(filepath.Join(scratch, name), 0o700); err != nil {
			return fail(err)
		}
	}
	return scratch, nil
}

func executorReadOnlyCodexCommand(ctx context.Context, argv []string, registryHome, workdir string) (*exec.Cmd, func(), error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, nil, fmt.Errorf("read-only Codex isolation requires bwrap: %w", err)
	}
	scratch, err := prepareReadOnlyCodexHome(registryHome)
	if err != nil {
		return nil, nil, err
	}
	wrapped := []string{bwrap, "--die-with-parent", "--new-session", "--ro-bind", "/", "/",
		"--dev-bind", "/dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp",
		"--bind", scratch, scratch, "--setenv", "CODEX_HOME", scratch, "--chdir", workdir, "--"}
	wrapped = append(wrapped, argv...)
	cmd, closeCommand, err := executorCommand(ctx, wrapped)
	if err != nil {
		_ = os.RemoveAll(scratch)
		return nil, nil, err
	}
	var closeOnce sync.Once
	return cmd, func() {
		closeOnce.Do(func() {
			closeCommand()
			_ = os.RemoveAll(scratch)
		})
	}, nil
}

func (r *RegistryExecutor) Execute(ctx context.Context, request delegatecontract.Invocation) delegatecontract.InvocationResult {
	result := delegatecontract.InvocationResult{Version: delegatecontract.WireVersion, Status: "failed"}
	fail := func(err error, detail string) delegatecontract.InvocationResult {
		result.Error = detail
		result.AvailabilityClass = delegatecontract.ClassifyProviderAvailability(err, false)
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	registry, err := r.load()
	if err != nil {
		return fail(err, err.Error())
	}
	agent, err := selectAgent(registry, request.Model, request.Role, request.Persona)
	if err != nil {
		return fail(err, err.Error())
	}
	result.Agent = agent.Name
	release, err := r.limiter(agent).acquire(ctx)
	if err != nil {
		return fail(err, err.Error())
	}
	defer release()
	if RoleIsWrite(request.Role) && strings.TrimSpace(request.Workdir) == "" {
		return fail(errors.New("write-capable delegate requires an isolated worktree"), "write-capable delegate requires an isolated worktree")
	}
	readOnlyCodex := strings.EqualFold(strings.TrimSpace(agent.CLIKind), "codex") && !RoleIsWrite(request.Role)
	if readOnlyCodex && strings.TrimSpace(request.Workdir) == "" {
		return fail(errors.New("read-only codex delegate requires an isolated checkout"), "read-only codex delegate requires an isolated checkout")
	}
	if request.Workdir != "" {
		if !filepath.IsAbs(request.Workdir) {
			return fail(errors.New("delegate workdir must be absolute"), "delegate workdir must be absolute")
		}
		root, err := gitCheckoutRoot(request.Workdir)
		if err != nil {
			return fail(err, "delegate workdir is not a git checkout")
		}
		request.Workdir = root
	}
	prompt := request.Prompt
	if persona := strings.TrimSpace(request.Persona); persona != "" {
		prompt = "You are acting as " + persona + ".\n\n" + prompt
	}
	argv, err := executorArgv(agent, request, prompt)
	if err != nil {
		return fail(err, err.Error())
	}
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	var cmd *exec.Cmd
	var closeCommand func()
	if readOnlyCodex {
		cmd, closeCommand, err = executorReadOnlyCodexCommand(runCtx, argv, filepath.Dir(r.path), request.Workdir)
	} else {
		cmd, closeCommand, err = executorCommand(runCtx, argv)
	}
	if err != nil {
		return fail(err, err.Error())
	}
	defer closeCommand()
	if request.Workdir != "" {
		cmd.Dir = request.Workdir
	}
	if strings.EqualFold(strings.TrimSpace(agent.CLIKind), "acp") {
		return r.executeACP(ctx, runCancel, closeCommand, cmd, agent, request, prompt)
	}
	if !strings.EqualFold(strings.TrimSpace(agent.CLIKind), "agy") {
		cmd.Stdin = strings.NewReader(prompt)
	}
	output := &limitedBuffer{remaining: maxExecutorOutput}
	monitor := newTurnMonitor(agent.CLIKind, request.MaxTurns, runCancel, output)
	// Tool-call observability reuses the existing structured stream signal.
	// For provider-CLI adapters the stream itself is the tool-call signal:
	//   claude:  assistant tool_use blocks (started only — no completion)
	//   codex:   item.completed tool items (completed only)
	//   agy:     tool_call/tool_result events when present, else item.completed
	//   acp:     session/update tool_call / tool_call_update (both phases)
	// Raw arguments/results are never stored; only name/call_id/status/elapsed.
	// If the protocol lacks a phase, the honest normalized behavior is a single
	// event for the phase that is exposed, documented here and in tests.
	col := newToolCollector()
	col.setWorkflow(request.Workflow)
	col.setContext(runCtx)
	defer col.close()
	kindLower := strings.ToLower(strings.TrimSpace(agent.CLIKind))
	var stdoutWriter io.Writer = output
	// Build the stacked writer: tool collector sees the raw NDJSON before the
	// turn monitor counts it, so neither parser misses a line.
	if kindLower == "claude" || kindLower == "claude-code" || kindLower == "codex" || kindLower == "agy" {
		tw := &toolEventStreamWriter{kind: kindLower, col: col, underlying: output}
		if request.MaxTurns > 0 {
			// monitor wraps output; tool writer wraps monitor so both see the stream.
			tw.underlying = monitor
			stdoutWriter = tw
		} else {
			stdoutWriter = tw
		}
	} else if request.MaxTurns > 0 {
		stdoutWriter = monitor
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		result.ResponseStarted = outputResponseStarted(agent.CLIKind, output.BytesCopy())
		result.ToolEvents = col.result()
		if monitor.Exceeded() {
			detail := fmt.Sprintf("delegate maximum turn count exceeded (%d)", request.MaxTurns)
			result.Error = detail
			result.AvailabilityClass = delegatecontract.ClassifyProviderAvailability(errors.New(detail), result.ResponseStarted)
			return result
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Error = delegatecontract.ErrDelegateExecutionDeadline.Error()
			if detail := strings.TrimSpace(output.String()); detail != "" {
				result.Error += ": " + delegatecontract.SafeDiagnosticSummary(detail, maxExecutorDiagnostic)
			}
			result.AvailabilityClass = delegatecontract.ClassifyProviderAvailability(delegatecontract.ErrDelegateExecutionDeadline, result.ResponseStarted)
			return result
		}
		detail := preResponseFailureDetail(agent.CLIKind, output.BytesCopy(), err)
		result.Error = delegatecontract.SafeDiagnosticSummary(detail, maxExecutorDiagnostic)
		result.AvailabilityClass = delegatecontract.ClassifyProviderAvailability(errors.New(detail), result.ResponseStarted)
		return result
	}
	outputBytes := output.BytesCopy()
	if detail := streamFailureDetail(agent.CLIKind, outputBytes); detail != "" {
		result.ResponseStarted = outputResponseStarted(agent.CLIKind, outputBytes)
		result.ToolEvents = col.result()
		result.Error = delegatecontract.SafeDiagnosticSummary(detail, maxExecutorDiagnostic)
		result.AvailabilityClass = delegatecontract.ClassifyProviderAvailability(errors.New(detail), result.ResponseStarted)
		return result
	}
	response := finalOutput(agent.CLIKind, outputBytes)
	if response == "" {
		result.ResponseStarted = outputResponseStarted(agent.CLIKind, outputBytes)
		result.ToolEvents = col.result()
		detail := preResponseFailureDetail(agent.CLIKind, outputBytes, errors.New("delegate CLI returned no final response"))
		if detail == "" {
			detail = "delegate CLI returned no final response"
		} else {
			detail = "delegate CLI returned no final response: " + detail
		}
		result.Error = delegatecontract.SafeDiagnosticSummary(detail, maxExecutorDiagnostic)
		result.AvailabilityClass = delegatecontract.ClassifyProviderAvailability(errors.New(detail), result.ResponseStarted)
		return result
	}
	result.Status, result.Response = "done", response
	result.ResponseStarted = true
	result.ToolEvents = col.result()
	return result
}

func gitCheckoutRoot(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(current); err != nil || !info.IsDir() {
		if err != nil {
			return "", err
		}
		return "", errors.New("delegate workdir is not a directory")
	}
	for {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("no git checkout contains delegate workdir")
		}
		current = parent
	}
}

// cancelledContext bridges the bus runtime's cancellation bit into os/exec.
func cancelledContext(invocationCancelled func() bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if invocationCancelled() {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

var _ io.Writer = (*limitedBuffer)(nil)
