package delegates

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	delegatecontract "github.com/JBailes/aimee/server-go/delegate"
)

const maxACPLine = maxExecutorOutput

type acpLine struct {
	text string
	err  error
}

type acpTransport struct {
	ctx          context.Context
	cancel       context.CancelFunc
	closeCommand func()
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	lines        <-chan acpLine
	wait         <-chan error
	idle         *time.Timer
	idleTimeout  time.Duration
	stderr       *limitedBuffer
	closeOnce    sync.Once
}

func startACPTransport(ctx context.Context, cancel context.CancelFunc, closeCommand func(), cmd *exec.Cmd, idleTimeout time.Duration) (*acpTransport, error) {
	stderr := &limitedBuffer{remaining: maxExecutorOutput}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		if closeCommand != nil {
			closeCommand()
		}
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		if closeCommand != nil {
			closeCommand()
		}
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		if closeCommand != nil {
			closeCommand()
		}
		return nil, err
	}
	lines := make(chan acpLine, 8)
	wait := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), maxACPLine)
		for scanner.Scan() {
			b := scanner.Bytes()
			cp := make([]byte, len(b))
			copy(cp, b)
			select {
			case lines <- acpLine{text: string(cp)}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- acpLine{err: err}:
			case <-ctx.Done():
				return
			}
		} else {
			select {
			case lines <- acpLine{err: io.EOF}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wait <- cmd.Wait()
	}()
	var idle *time.Timer
	if idleTimeout > 0 {
		idle = time.NewTimer(idleTimeout)
	}
	return &acpTransport{
		ctx:          ctx,
		cancel:       cancel,
		closeCommand: closeCommand,
		cmd:          cmd,
		stdin:        stdin,
		lines:        lines,
		wait:         wait,
		idle:         idle,
		idleTimeout:  idleTimeout,
		stderr:       stderr,
	}, nil
}

func (t *acpTransport) send(id int, method string, params any) error {
	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	for written := 0; written < len(data); {
		n, err := t.stdin.Write(data[written:])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	return nil
}

func (t *acpTransport) sendRaw(raw []byte) error {
	data := make([]byte, 0, len(raw)+1)
	data = append(data, raw...)
	data = append(data, '\n')
	for written := 0; written < len(data); {
		n, err := t.stdin.Write(data[written:])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	return nil
}

func (t *acpTransport) nextLine(limit time.Duration) (string, error) {
	var idleC <-chan time.Time
	if t.idle != nil {
		idleC = t.idle.C
	}
	var limitC <-chan time.Time
	var limitTimer *time.Timer
	if limit > 0 {
		limitTimer = time.NewTimer(limit)
		defer limitTimer.Stop()
		limitC = limitTimer.C
	}
	select {
	case <-t.ctx.Done():
		return "", t.ctx.Err()
	case <-idleC:
		return "", errors.New("ACP delegate idle timeout")
	case <-limitC:
		return "", errors.New("ACP handshake timeout")
	case line, ok := <-t.lines:
		if !ok {
			return "", io.EOF
		}
		if line.err != nil {
			return "", line.err
		}
		if t.idle != nil {
			if !t.idle.Stop() {
				select {
				case <-t.idle.C:
				default:
				}
			}
			t.idle.Reset(t.idleTimeout)
		}
		return line.text, nil
	}
}

func (t *acpTransport) waitResponse(id int, limit time.Duration) (acpResponse, error) {
	var deadline time.Time
	if limit > 0 {
		deadline = time.Now().Add(limit)
	}
	for {
		var remaining time.Duration
		if !deadline.IsZero() {
			remaining = time.Until(deadline)
			if remaining <= 0 {
				return acpResponse{}, errors.New("ACP handshake timeout")
			}
		}
		line, err := t.nextLine(remaining)
		if err != nil {
			return acpResponse{}, err
		}
		var resp acpResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		if len(resp.ID) == 0 || string(resp.ID) == "null" {
			continue
		}
		var got int
		if err := json.Unmarshal(resp.ID, &got); err != nil {
			var f float64
			if err2 := json.Unmarshal(resp.ID, &f); err2 != nil {
				continue
			}
			if int(f) != id {
				continue
			}
		} else if got != id {
			continue
		}
		return resp, nil
	}
}

func acpResponseError(resp acpResponse) error {
	if resp.Error == nil {
		return nil
	}
	msg := resp.Error.Message
	if msg == "" {
		msg = "unknown"
	}
	return errors.New("acp error: " + msg)
}

func (t *acpTransport) close() {
	t.closeOnce.Do(func() {
		if t.stdin != nil {
			_ = t.stdin.Close()
		}
		if t.cancel != nil {
			t.cancel()
		}
		if t.closeCommand != nil {
			t.closeCommand()
		}
		select {
		case <-t.wait:
		case <-time.After(5 * time.Second):
		}
		if t.idle != nil {
			t.idle.Stop()
		}
	})
}

func (r *RegistryExecutor) executeACP(ctx context.Context, runCancel context.CancelFunc, closeCommand func(), cmd *exec.Cmd, agent agentEntry, request delegatecontract.Invocation, prompt string) delegatecontract.InvocationResult {
	result := delegatecontract.InvocationResult{Version: delegatecontract.WireVersion, Status: "failed", Agent: agent.Name}
	var transport *acpTransport
	fail := func(err error, detail string, started bool) delegatecontract.InvocationResult {
		result.ResponseStarted = started
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Error = delegatecontract.ErrDelegateExecutionDeadline.Error()
			result.AvailabilityClass = delegatecontract.ClassifyProviderAvailability(delegatecontract.ErrDelegateExecutionDeadline, started)
			return result
		}
		if strings.TrimSpace(detail) == "" && err != nil {
			detail = err.Error()
		}
		detail = delegatecontract.SafeDiagnostic(strings.TrimSpace(detail))
		if transport != nil && transport.stderr != nil {
			if s := strings.TrimSpace(transport.stderr.String()); s != "" {
				detail = detail + ": " + delegatecontract.SafeDiagnostic(s)
			}
		}
		if strings.TrimSpace(detail) == "" && err != nil {
			detail = delegatecontract.SafeDiagnostic(err.Error())
		}
		result.Error = detail
		result.AvailabilityClass = delegatecontract.ClassifyProviderAvailability(err, started)
		return result
	}
	var idleTimeout time.Duration
	if agent.CLIIdleTimeoutMS != nil {
		idleTimeout = time.Duration(*agent.CLIIdleTimeoutMS) * time.Millisecond
	}
	var err error
	transport, err = startACPTransport(ctx, runCancel, closeCommand, cmd, idleTimeout)
	if err != nil {
		return fail(err, err.Error(), false)
	}
	defer transport.close()
	// id1 initialize
	initParams := map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  true,
				"writeTextFile": RoleIsWrite(request.Role),
			},
		},
	}
	if err := transport.send(1, "initialize", initParams); err != nil {
		return fail(err, err.Error(), false)
	}
	if _, err := transport.waitResponse(1, 5*time.Second); err != nil {
		return fail(err, err.Error(), false)
	}
	// id2 session/new
	cwd := strings.TrimSpace(request.Workdir)
	if cwd == "" {
		cwd = "."
	}
	newParams := map[string]any{
		"cwd": cwd,
		"mcpServers": []map[string]any{{
			"name": "aimee", "command": "aimee", "args": []string{"mcp-serve"},
			"env": []any{},
		}},
	}
	if err := transport.send(2, "session/new", newParams); err != nil {
		return fail(err, err.Error(), false)
	}
	resp2, err := transport.waitResponse(2, 10*time.Second)
	if err != nil {
		return fail(err, err.Error(), false)
	}
	sessionID := ""
	if resp2.Error == nil && len(resp2.Result) > 0 && string(resp2.Result) != "null" {
		var parsed struct {
			SessionID *string `json:"sessionId"`
		}
		if e := json.Unmarshal(resp2.Result, &parsed); e == nil && parsed.SessionID != nil {
			sessionID = *parsed.SessionID
		}
	}
	// id4 session/set_model
	if model := strings.TrimSpace(agent.Model); model != "" {
		params := map[string]any{
			"sessionId": sessionID,
			"modelId":   model,
		}
		if err := transport.send(4, "session/set_model", params); err != nil {
			return fail(err, fmt.Sprintf("agent did not accept pinned model '%s' (session/set_model): %v", model, err), false)
		}
		resp4, err := transport.waitResponse(4, 10*time.Second)
		if err != nil {
			return fail(err, fmt.Sprintf("agent did not accept pinned model '%s' (session/set_model): %v", model, err), false)
		}
		if acpResponseError(resp4) != nil || len(resp4.Result) == 0 {
			return fail(acpResponseError(resp4), fmt.Sprintf("agent did not accept pinned model '%s' (session/set_model)", model), false)
		}
	}
	// id5 session/set_config_option
	if effort := strings.TrimSpace(agent.ReasoningEffort); effort != "" {
		params := map[string]any{
			"sessionId": sessionID,
			"configId":  "effort",
			"value":     effort,
		}
		if err := transport.send(5, "session/set_config_option", params); err != nil {
			return fail(err, fmt.Sprintf("agent did not accept reasoning effort '%s' (session/set_config_option): %v", effort, err), false)
		}
		resp5, err := transport.waitResponse(5, 10*time.Second)
		if err != nil {
			return fail(err, fmt.Sprintf("agent did not accept reasoning effort '%s' (session/set_config_option): %v", effort, err), false)
		}
		if acpResponseError(resp5) != nil || len(resp5.Result) == 0 {
			return fail(acpResponseError(resp5), fmt.Sprintf("agent did not accept reasoning effort '%s' (session/set_config_option)", effort), false)
		}
	}
	// id3 session/prompt
	promptParams := map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]string{
			{"type": "text", "text": prompt},
		},
	}
	if err := transport.send(3, "session/prompt", promptParams); err != nil {
		return fail(err, err.Error(), false)
	}
	state := acpTurnState{promptID: 3}
	for {
		line, err := transport.nextLine(0)
		if err != nil {
			return fail(err, err.Error(), strings.TrimSpace(state.Text()) != "")
		}
		handled, resp := acpServeClientRequestGated(line, request.Workdir, RoleIsWrite(request.Role))
		if handled {
			if resp != nil {
				if err := transport.sendRaw(resp); err != nil {
					return fail(err, err.Error(), strings.TrimSpace(state.Text()) != "")
				}
			}
			continue
		}
		if err := acpTurnConsume(line, &state); err != nil {
			return fail(err, err.Error(), strings.TrimSpace(state.Text()) != "")
		}
		if state.Done() {
			if strings.TrimSpace(state.Text()) == "" {
				return fail(errors.New("delegate CLI returned no final response"), "delegate CLI returned no final response", false)
			}
			result.Status = "done"
			result.Response = state.Text()
			result.ResponseStarted = true
			result.Error = ""
			result.AvailabilityClass = ""
			return result
		}
	}
}
