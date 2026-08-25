package delegates

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// JSON-RPC envelope helpers preserve request id as json.RawMessage.

type acpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type acpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpRPCError    `json:"error,omitempty"`
}

func acpErrorResponse(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 || string(id) == "null" {
		id = json.RawMessage("null")
	}
	resp := acpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &acpRPCError{Code: code, Message: message},
	}
	b, _ := json.Marshal(resp)
	return b
}

func acpResultResponse(id json.RawMessage, result any) []byte {
	if len(id) == 0 || string(id) == "null" {
		id = json.RawMessage("null")
	}
	var raw json.RawMessage
	if result == nil {
		raw = json.RawMessage("null")
	} else {
		raw, _ = json.Marshal(result)
	}
	resp := acpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  raw,
	}
	b, _ := json.Marshal(resp)
	return b
}

// acpTurnState is the bounded accumulator for a single ACP turn.
// maxExecutorOutput bounds the accumulated text.
type acpTurnState struct {
	promptID  int
	buf       []byte
	toolCalls int
	done      bool
	hadError  bool
	errMsg    string
}

func (s *acpTurnState) appendText(delta string) {
	if delta == "" || s == nil {
		return
	}
	if len(s.buf) >= maxExecutorOutput {
		return
	}
	remaining := maxExecutorOutput - len(s.buf)
	if len(delta) > remaining {
		delta = delta[:remaining]
	}
	s.buf = append(s.buf, delta...)
}

func (s *acpTurnState) Text() string {
	if s == nil {
		return ""
	}
	return string(s.buf)
}

func (s *acpTurnState) Done() bool {
	if s == nil {
		return false
	}
	return s.done
}

func (s *acpTurnState) ToolCalls() int {
	if s == nil {
		return 0
	}
	return s.toolCalls
}

func (s *acpTurnState) HadError() bool {
	if s == nil {
		return false
	}
	return s.hadError
}

func (s *acpTurnState) Error() string {
	if s == nil {
		return ""
	}
	return s.errMsg
}

// acpTurnConsume consumes one newline-delimited JSON line into the turn state.
// It implements the exact semantics of src/server/cli_acp.c:acp_turn_consume.
func acpTurnConsume(line string, st *acpTurnState) error {
	if st == nil {
		return errors.New("nil turn state")
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	var base struct {
		Method *string         `json:"method"`
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *acpRPCError    `json:"error"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(trimmed), &base); err != nil {
		return nil
	}
	if base.Method != nil {
		method := *base.Method
		switch method {
		case "text/delta":
			if len(base.Params) > 0 {
				var p struct {
					Content *string `json:"content"`
				}
				if err := json.Unmarshal(base.Params, &p); err == nil && p.Content != nil {
					st.appendText(*p.Content)
				}
			}
		case "session/update":
			if len(base.Params) > 0 {
				var p struct {
					Update json.RawMessage `json:"update"`
				}
				if err := json.Unmarshal(base.Params, &p); err == nil && len(p.Update) > 0 {
					var upd struct {
						SessionUpdate *string         `json:"sessionUpdate"`
						Type          *string         `json:"type"`
						Content       json.RawMessage `json:"content"`
						Title         *string         `json:"title"`
						Name          *string         `json:"name"`
					}
					if err := json.Unmarshal(p.Update, &upd); err == nil {
						kind := ""
						if upd.SessionUpdate != nil {
							kind = *upd.SessionUpdate
						} else if upd.Type != nil {
							kind = *upd.Type
						}
						if kind == "tool_call" || kind == "tool_call_update" {
							st.toolCalls++
						} else if len(upd.Content) > 0 {
							var asString string
							if err := json.Unmarshal(upd.Content, &asString); err == nil {
								st.appendText(asString)
							} else {
								var asObj struct {
									Text *string `json:"text"`
								}
								if err := json.Unmarshal(upd.Content, &asObj); err == nil && asObj.Text != nil {
									st.appendText(*asObj.Text)
								}
							}
						}
					}
				}
			}
		case "tool/call":
			st.toolCalls++
		default:
		}
		return nil
	}
	terminalID := st.promptID
	if terminalID == 0 {
		terminalID = 2
	}
	if len(base.ID) > 0 && len(base.Result) > 0 && string(base.ID) != "null" {
		var idNum float64
		if err := json.Unmarshal(base.ID, &idNum); err == nil {
			if int(idNum) == terminalID {
				if len(st.buf) == 0 {
					var res struct {
						Content *string `json:"content"`
					}
					if err := json.Unmarshal(base.Result, &res); err == nil && res.Content != nil {
						st.appendText(*res.Content)
					}
				}
				st.done = true
			}
		}
	}
	if base.Error != nil {
		msg := base.Error.Message
		if msg == "" {
			msg = "unknown"
		}
		st.hadError = true
		st.errMsg = "acp error: " + msg
		return errors.New(st.errMsg)
	}
	return nil
}

// resolveACPPath confines an ACP-supplied path to workdir.
// It mirrors src/server/cli_acp.c:acp_resolve_in_workdir.
func resolveACPPath(workdir, path string) (string, error) {
	if workdir == "" || path == "" {
		return "", errors.New("empty workdir or path")
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return "", errors.New("path contains .. segment")
		}
	}
	if filepath.IsAbs(path) {
		cleanWorkdir := strings.TrimRight(workdir, "/")
		if cleanWorkdir == "" {
			cleanWorkdir = "/"
		}
		if cleanWorkdir == "/" {
			return path, nil
		}
		if path != cleanWorkdir && !strings.HasPrefix(path, cleanWorkdir+"/") {
			return "", errors.New("absolute path outside workdir")
		}
		return path, nil
	}
	return filepath.Join(workdir, path), nil
}

// handleACPClientRequest handles a decoded client request line.
// It returns (handled bool, response []byte) matching the C gated helper.
// Notifications/responses (no method+id) return handled=false.
func handleACPClientRequest(line string, workdir string, writeCapable bool) (bool, []byte) {
	return acpServeClientRequestGated(line, workdir, writeCapable)
}

// acpServeClientRequestGated is the primary implementation matching the C name.
func acpServeClientRequestGated(line string, workdir string, writeCapable bool) (bool, []byte) {
	return handleDecodedACPLine([]byte(line), workdir, writeCapable)
}

func handleDecodedACPLine(line []byte, workdir string, writeCapable bool) (bool, []byte) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return false, nil
	}
	var req acpRequest
	if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
		return false, nil
	}
	if req.Method == "" || len(req.ID) == 0 || string(req.ID) == "null" {
		return false, nil
	}
	switch req.Method {
	case "fs/read_text_file":
		return true, handleACPRead(req.ID, req.Params, workdir)
	case "fs/write_text_file":
		return true, handleACPWrite(req.ID, req.Params, workdir, writeCapable)
	case "session/request_permission":
		return true, handleACPPermission(req.ID, req.Params, writeCapable)
	default:
		return true, acpErrorResponse(req.ID, -32601, "method not supported by aimee ACP client")
	}
}

func handleACPRead(id json.RawMessage, params json.RawMessage, workdir string) []byte {
	var p struct {
		Path *string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Path == nil {
		return acpErrorResponse(id, -32602, "invalid or out-of-workdir path")
	}
	full, err := resolveACPPath(workdir, *p.Path)
	if err != nil {
		return acpErrorResponse(id, -32602, "invalid or out-of-workdir path")
	}
	info, err := os.Stat(full)
	if err != nil {
		return acpErrorResponse(id, -32603, "cannot read file")
	}
	if info.Size() > 16*1024*1024 {
		return acpErrorResponse(id, -32603, "file too large")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return acpErrorResponse(id, -32603, "cannot read file")
	}
	if len(data) > 16*1024*1024 {
		return acpErrorResponse(id, -32603, "file too large")
	}
	return acpResultResponse(id, map[string]string{"content": string(data)})
}

func handleACPWrite(id json.RawMessage, params json.RawMessage, workdir string, writeCapable bool) []byte {
	if !writeCapable {
		return acpErrorResponse(id, -32603, "write denied: delegate role is read-only")
	}
	var p struct {
		Path    *string `json:"path"`
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Path == nil || p.Content == nil {
		return acpErrorResponse(id, -32602, "invalid or out-of-workdir path")
	}
	full, err := resolveACPPath(workdir, *p.Path)
	if err != nil {
		return acpErrorResponse(id, -32602, "invalid or out-of-workdir path")
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return acpErrorResponse(id, -32603, "cannot write file")
	}
	if err := os.WriteFile(full, []byte(*p.Content), 0644); err != nil {
		return acpErrorResponse(id, -32603, "cannot write file")
	}
	return acpResultResponse(id, nil)
}

func handleACPPermission(id json.RawMessage, params json.RawMessage, writeCapable bool) []byte {
	var p struct {
		Options []struct {
			OptionID *string `json:"optionId"`
			Kind     *string `json:"kind"`
		} `json:"options"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	var optID *string
	var first *string
	for i := range p.Options {
		oid := p.Options[i].OptionID
		kind := p.Options[i].Kind
		if oid == nil || *oid == "" {
			continue
		}
		if first == nil {
			first = oid
		}
		if kind == nil || *kind == "" {
			continue
		}
		k := *kind
		if writeCapable && strings.HasPrefix(k, "allow") {
			optID = oid
			break
		}
		if !writeCapable && (strings.HasPrefix(k, "reject") || strings.HasPrefix(k, "deny")) {
			optID = oid
			break
		}
	}
	if optID == nil && writeCapable && first != nil {
		optID = first
	}
	if optID != nil {
		return acpResultResponse(id, map[string]any{
			"outcome": map[string]string{
				"outcome":  "selected",
				"optionId": *optID,
			},
		})
	}
	return acpResultResponse(id, map[string]any{
		"outcome": map[string]string{
			"outcome": "cancelled",
		},
	})
}
