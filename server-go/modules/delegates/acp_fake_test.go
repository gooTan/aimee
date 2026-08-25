package delegates

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestACPHelperProcess(t *testing.T) {
	args := os.Args
	scenario := ""
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			scenario = args[i+1]
			break
		}
	}
	if scenario == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	fail := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
		os.Exit(1)
	}
	reply := func(id interface{}, result interface{}, errorMessage string) {
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": id}
		if errorMessage != "" {
			resp["error"] = map[string]interface{}{"code": -32603, "message": errorMessage}
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(os.Stderr, "encode reply failed: %v\n", err)
			os.Exit(1)
		}
	}
	readRequest := func(expectedID int, expectedMethod string) json.RawMessage {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				fail("expected request %s id %d: scan error: %v", expectedMethod, expectedID, err)
			}
			fail("expected request %s id %d: EOF", expectedMethod, expectedID)
		}
		line := scanner.Bytes()
		var req struct {
			ID      *json.RawMessage `json:"id"`
			Method  string           `json:"method"`
			Params  json.RawMessage  `json:"params"`
			JSONRPC string           `json:"jsonrpc"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			fail("invalid json for %s id %d: %v line=%s", expectedMethod, expectedID, err, string(line))
		}
		if req.ID == nil {
			fail("missing id for %s expected %d line=%s", expectedMethod, expectedID, string(line))
		}
		var gotID int
		if err := json.Unmarshal(*req.ID, &gotID); err != nil {
			fail("invalid id type for %s expected %d: %v line=%s", expectedMethod, expectedID, err, string(line))
		}
		if gotID != expectedID {
			fail("wrong id for %s: expected %d got %d line=%s", expectedMethod, expectedID, gotID, string(line))
		}
		if req.Method != expectedMethod {
			fail("wrong method: expected %s got %s id %d line=%s", expectedMethod, req.Method, expectedID, string(line))
		}
		if req.JSONRPC != "" && req.JSONRPC != "2.0" {
			fail("wrong jsonrpc for %s id %d: %s", expectedMethod, expectedID, req.JSONRPC)
		}
		return req.Params
	}
	readResponse := func(expectedID int) struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} {
		if !scanner.Scan() {
			fail("missing response for id %d: %v", expectedID, scanner.Err())
		}
		line := scanner.Bytes()
		var resp struct {
			ID     *json.RawMessage `json:"id"`
			Result json.RawMessage  `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			fail("invalid json response for id %d: %v line=%s", expectedID, err, string(line))
		}
		var gotID int
		if resp.ID != nil {
			_ = json.Unmarshal(*resp.ID, &gotID)
		}
		if gotID != expectedID {
			fail("wrong id for response: expected %d got %d line=%s", expectedID, gotID, string(line))
		}
		return struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}{Result: resp.Result, Error: resp.Error}
	}
	validateInit := func(params json.RawMessage) {
		if len(params) == 0 {
			fail("initialize missing params")
		}
		var p struct {
			ProtocolVersion    int `json:"protocolVersion"`
			ClientCapabilities struct {
				FS struct {
					ReadTextFile  bool `json:"readTextFile"`
					WriteTextFile bool `json:"writeTextFile"`
				} `json:"fs"`
			} `json:"clientCapabilities"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			fail("initialize invalid params json: %v %s", err, string(params))
		}
		if p.ProtocolVersion == 0 {
			fail("initialize missing protocolVersion: %s", string(params))
		}
		if !p.ClientCapabilities.FS.ReadTextFile {
			fail("initialize fs read true not found in params %s", string(params))
		}
		if p.ClientCapabilities.FS.WriteTextFile {
			fail("initialize fs write false expected got true %s", string(params))
		}
	}
	validateModel := func(params json.RawMessage) {
		var p struct {
			SessionID string `json:"sessionId"`
			ModelID   string `json:"modelId"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			fail("set_model invalid json: %v %s", err, string(params))
		}
		if p.ModelID != "opencode-go/muse-spark-1.2-contributor" {
			fail("set_model modelId mismatch: expected opencode-go/muse-spark-1.2-contributor got %s", string(params))
		}
		if p.SessionID == "" {
			fail("set_model missing sessionId: %s", string(params))
		}
	}
	validateEffort := func(params json.RawMessage) {
		var p struct {
			SessionID string `json:"sessionId"`
			ConfigID  string `json:"configId"`
			Value     string `json:"value"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			fail("set_config_option invalid json: %v %s", err, string(params))
		}
		if p.SessionID == "" {
			fail("set_config_option missing sessionId: %s", string(params))
		}
		if p.ConfigID != "effort" {
			fail("set_config_option missing effort configId: %s", string(params))
		}
		if p.Value != "xhigh" {
			fail("set_config_option missing xhigh value: %s", string(params))
		}
	}
	validatePrompt := func(params json.RawMessage) {
		if len(params) == 0 {
			fail("prompt missing params")
		}
		var p struct {
			SessionID string `json:"sessionId"`
			Prompt    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			fail("prompt invalid params json: %v %s", err, string(params))
		}
		if p.SessionID != "fake-session" {
			fail("prompt missing sessionId fake-session: %s", string(params))
		}
		ok := false
		for _, blk := range p.Prompt {
			if blk.Type == "text" && strings.TrimSpace(blk.Text) != "" {
				ok = true
				break
			}
		}
		if !ok {
			fail("prompt missing typed text prompt: %s", string(params))
		}
	}
	acceptInitAndNew := func() {
		p1 := readRequest(1, "initialize")
		validateInit(p1)
		reply(1, map[string]interface{}{}, "")
		p2 := readRequest(2, "session/new")
		var newSession struct {
			MCPServers []struct {
				Name    string   `json:"name"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(p2, &newSession); err != nil {
			fail("session/new invalid params json: %v %s", err, string(p2))
		}
		if len(newSession.MCPServers) != 1 || newSession.MCPServers[0].Name != "aimee" ||
			newSession.MCPServers[0].Command != "aimee" ||
			len(newSession.MCPServers[0].Args) != 1 || newSession.MCPServers[0].Args[0] != "mcp-serve" {
			fail("session/new missing Aimee MCP server: %s", string(p2))
		}
		reply(2, map[string]interface{}{"sessionId": "fake-session"}, "")
	}
	acceptModel := func() {
		p4 := readRequest(4, "session/set_model")
		validateModel(p4)
		reply(4, nil, "")
	}
	waitShort := func() {
		done := make(chan bool, 1)
		go func() { scanner.Scan(); done <- true }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}
	}
	switch scenario {
	case "happy-readonly":
		acceptInitAndNew()
		acceptModel()
		p5 := readRequest(5, "session/set_config_option")
		validateEffort(p5)
		reply(5, nil, "")
		p3 := readRequest(3, "session/prompt")
		validatePrompt(p3)
		if err := enc.Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 90, "method": "fs/write_text_file", "params": map[string]interface{}{"path": "blocked.txt", "content": "blocked"}}); err != nil {
			fail("failed to send fs/write_text_file: %v", err)
		}
		resp90 := readResponse(90)
		if resp90.Error == nil {
			fail("expected error for fs/write_text_file id90, got result %s", string(resp90.Result))
		}
		if resp90.Error.Code != -32603 {
			fail("wrong error code for fs/write_text_file id90: expected -32603 got %d", resp90.Error.Code)
		}
		if err := enc.Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 91, "method": "session/request_permission", "params": map[string]interface{}{"options": []map[string]interface{}{{"optionId": "allow", "label": "Allow once", "kind": "allow_once"}, {"optionId": "reject", "label": "Reject once", "kind": "reject_once"}}, "toolCallId": "tool-1"}}); err != nil {
			fail("failed to send session/request_permission: %v", err)
		}
		resp91 := readResponse(91)
		if resp91.Error != nil {
			fail("unexpected error for session/request_permission id91: %v", resp91.Error)
		}
		s := string(resp91.Result)
		lower := strings.ToLower(s)
		if !strings.Contains(lower, "reject") {
			fail("expected selected optionId reject for session/request_permission id91, got %s", s)
		}
		if strings.Contains(lower, "\"optionid\":\"allow") || strings.Contains(lower, "\"optionid\": \"allow") {
			fail("permission should be reject not allow: %s", s)
		}
		if err := enc.Encode(map[string]interface{}{"jsonrpc": "2.0", "method": "session/update", "params": map[string]interface{}{"sessionId": "fake-session", "update": map[string]interface{}{"sessionUpdate": "agent_message_chunk", "content": map[string]interface{}{"type": "text", "text": "MUSE-"}}}}); err != nil {
			fail("failed to send session/update MUSE-: %v", err)
		}
		if err := enc.Encode(map[string]interface{}{"jsonrpc": "2.0", "method": "session/update", "params": map[string]interface{}{"sessionId": "fake-session", "update": map[string]interface{}{"sessionUpdate": "agent_message_chunk", "content": map[string]interface{}{"type": "text", "text": "ACP-OK"}}}}); err != nil {
			fail("failed to send session/update ACP-OK: %v", err)
		}
		if err := enc.Encode(map[string]interface{}{"jsonrpc": "2.0", "method": "session/update", "params": map[string]interface{}{"sessionId": "fake-session", "update": map[string]interface{}{"sessionUpdate": "tool_call", "toolCallId": "tool-1", "title": "fake-tool", "status": "completed", "content": []map[string]interface{}{{"type": "text", "text": "tool result"}}}}}); err != nil {
			fail("failed to send session/update tool_call: %v", err)
		}
		reply(3, map[string]interface{}{"stopReason": "end_turn"}, "")
		os.Exit(0)
	case "refuse-model":
		acceptInitAndNew()
		p4 := readRequest(4, "session/set_model")
		validateModel(p4)
		reply(4, nil, "model refused")
		waitShort()
		os.Exit(0)
	case "missing-model-result":
		acceptInitAndNew()
		p4 := readRequest(4, "session/set_model")
		validateModel(p4)
		if err := enc.Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 4}); err != nil {
			fail("failed to send missing-model-result: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	case "refuse-effort":
		acceptInitAndNew()
		acceptModel()
		p5 := readRequest(5, "session/set_config_option")
		validateEffort(p5)
		reply(5, nil, "effort refused")
		waitShort()
		os.Exit(0)
	case "stall-after-new":
		acceptInitAndNew()
		time.Sleep(2 * time.Second)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario %q\n", scenario)
		os.Exit(1)
	}
}
