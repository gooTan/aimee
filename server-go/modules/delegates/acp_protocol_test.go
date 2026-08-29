package delegates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestACPTurnAccumulation(t *testing.T) {
	st := &acpTurnState{promptID: 3}
	if err := acpTurnConsume(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":"hello "}}}`, st); err != nil {
		t.Fatalf("consume chunk1: %v", err)
	}
	if err := acpTurnConsume(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"world"}}}}`, st); err != nil {
		t.Fatalf("consume chunk2: %v", err)
	}
	if got := st.Text(); got != "hello world" {
		t.Fatalf("text = %q want %q", got, "hello world")
	}
	if st.ToolCalls() != 0 {
		t.Fatalf("toolCalls = %d want 0", st.ToolCalls())
	}
	if st.Done() {
		t.Fatalf("done before terminal")
	}
	if err := acpTurnConsume(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call"}}}`, st); err != nil {
		t.Fatalf("consume tool_call: %v", err)
	}
	if st.ToolCalls() != 1 {
		t.Fatalf("toolCalls = %d want 1", st.ToolCalls())
	}
	if err := acpTurnConsume(`{"jsonrpc":"2.0","id":2,"result":{"content":"ignored"}}`, st); err != nil {
		t.Fatalf("consume id2: %v", err)
	}
	if st.Done() {
		t.Fatalf("done after non-terminal id2")
	}
	if got := st.Text(); got != "hello world" {
		t.Fatalf("text after id2 = %q want %q", got, "hello world")
	}
	if err := acpTurnConsume(`{"jsonrpc":"2.0","id":3,"result":{"content":"fallback ignored"}}`, st); err != nil {
		t.Fatalf("consume id3: %v", err)
	}
	if !st.Done() {
		t.Fatalf("not done after terminal id3")
	}
	if got := st.Text(); got != "hello world" {
		t.Fatalf("streamed text should win over result.content: got %q want %q", got, "hello world")
	}
}

func TestACPTurnFallbackAndError(t *testing.T) {
	st := &acpTurnState{promptID: 3}
	if err := acpTurnConsume(`{"jsonrpc":"2.0","id":3,"result":{"content":"fallback text"}}`, st); err != nil {
		t.Fatalf("fallback consume: %v", err)
	}
	if !st.Done() {
		t.Fatalf("fallback not done")
	}
	if got := st.Text(); got != "fallback text" {
		t.Fatalf("fallback text = %q want %q", got, "fallback text")
	}
	st2 := &acpTurnState{promptID: 3}
	err := acpTurnConsume(`{"jsonrpc":"2.0","id":3,"error":{"code":-32600,"message":"peer boom"}}`, st2)
	if err == nil || !strings.Contains(err.Error(), "peer boom") {
		t.Fatalf("peer error = %v want containing peer boom", err)
	}
	if !st2.HadError() || !strings.Contains(st2.Error(), "peer boom") {
		t.Fatalf("hadError=%v errMsg=%q", st2.HadError(), st2.Error())
	}
}

func TestACPPathConfinement(t *testing.T) {
	workdir := t.TempDir()
	got, err := resolveACPPath(workdir, "a/b.txt")
	if err != nil {
		t.Fatalf("resolve inside: %v", err)
	}
	want := filepath.Join(workdir, "a/b.txt")
	if got != want {
		t.Fatalf("path = %q want %q", got, want)
	}
	for _, bad := range []string{"../outside", "a/../b", "a/b/..", "..", "a/b/../../c"} {
		if _, err := resolveACPPath(workdir, bad); err == nil {
			t.Fatalf("path %q should fail", bad)
		}
	}
	if _, err := resolveACPPath(workdir, "/etc/passwd"); err == nil {
		t.Fatalf("absolute outside should fail")
	}
	insideAbs := filepath.Join(workdir, "inside.txt")
	if p, err := resolveACPPath(workdir, insideAbs); err != nil || p != insideAbs {
		t.Fatalf("absolute inside: got %q err %v", p, err)
	}
	dotDotAbs := workdir + "/../etc/passwd"
	if _, err := resolveACPPath(workdir, dotDotAbs); err == nil {
		t.Fatalf("path with .. should fail: %q", dotDotAbs)
	}
}

func TestACPClientReadAndUnknown(t *testing.T) {
	workdir := t.TempDir()
	content := "hello file content"
	rel := "sub/file.txt"
	full := filepath.Join(workdir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	line := `{"jsonrpc":"2.0","id":1,"method":"fs/read_text_file","params":{"path":"` + rel + `"}}`
	handled, resp := handleACPClientRequest(line, workdir, false)
	if !handled || resp == nil {
		t.Fatalf("read inside not handled")
	}
	var r acpResponse
	if err := json.Unmarshal(resp, &r); err != nil {
		t.Fatalf("unmarshal: %v resp=%s", err, string(resp))
	}
	if string(r.ID) != "1" {
		t.Fatalf("id = %s want 1", string(r.ID))
	}
	if r.Error != nil {
		t.Fatalf("unexpected error: %+v", r.Error)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &result); err != nil {
		t.Fatalf("result unmarshal: %v", err)
	}
	if result.Content != content {
		t.Fatalf("content = %q want %q", result.Content, content)
	}
	line2 := `{"jsonrpc":"2.0","id":2,"method":"fs/read_text_file","params":{"path":"../outside"}}`
	handled, resp = handleACPClientRequest(line2, workdir, false)
	if !handled || resp == nil {
		t.Fatalf("outside not handled")
	}
	var r2 acpResponse
	if err := json.Unmarshal(resp, &r2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r2.Error == nil || r2.Error.Code != -32602 {
		t.Fatalf("outside code = %+v want -32602", r2.Error)
	}
	line3 := `{"jsonrpc":"2.0","id":3,"method":"unknown/method","params":{}}`
	handled, resp = handleACPClientRequest(line3, workdir, false)
	if !handled || resp == nil {
		t.Fatalf("unknown not handled")
	}
	var r3 acpResponse
	if err := json.Unmarshal(resp, &r3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r3.Error == nil || r3.Error.Code != -32601 {
		t.Fatalf("unknown code = %+v want -32601", r3.Error)
	}
	line4 := `{"jsonrpc":"2.0","method":"fs/read_text_file","params":{"path":"` + rel + `"}}`
	handled, resp = handleACPClientRequest(line4, workdir, false)
	if handled || resp != nil {
		t.Fatalf("notification without id should not be handled: handled=%v resp=%s", handled, string(resp))
	}
}

func TestACPReadOnlyWriteAndPermission(t *testing.T) {
	workdir := t.TempDir()
	line := `{"jsonrpc":"2.0","id":1,"method":"fs/write_text_file","params":{"path":"out.txt","content":"hello"}}`
	handled, resp := handleACPClientRequest(line, workdir, false)
	if !handled || resp == nil {
		t.Fatalf("write not handled")
	}
	var r acpResponse
	if err := json.Unmarshal(resp, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Error == nil || r.Error.Code != -32603 {
		t.Fatalf("write read-only code = %+v want -32603", r.Error)
	}
	if _, err := os.Stat(filepath.Join(workdir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("target should be absent, stat err %v", err)
	}
	line2 := `{"jsonrpc":"2.0","id":2,"method":"session/request_permission","params":{"options":[{"optionId":"allow-1","kind":"allow"},{"optionId":"reject-1","kind":"reject"}]}}`
	handled, resp = handleACPClientRequest(line2, workdir, false)
	if !handled || resp == nil {
		t.Fatalf("permission not handled")
	}
	var r2 acpResponse
	if err := json.Unmarshal(resp, &r2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r2.Error != nil {
		t.Fatalf("permission error: %+v", r2.Error)
	}
	var perm struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(r2.Result, &perm); err != nil {
		t.Fatalf("perm unmarshal: %v", err)
	}
	if perm.Outcome.Outcome != "selected" || perm.Outcome.OptionID != "reject-1" {
		t.Fatalf("permission selected = %+v want reject-1", perm.Outcome)
	}
	line3 := `{"jsonrpc":"2.0","id":3,"method":"session/request_permission","params":{"options":[{"optionId":"allow-1","kind":"allow"}]}}`
	handled, resp = handleACPClientRequest(line3, workdir, false)
	var r3 acpResponse
	if err := json.Unmarshal(resp, &r3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := json.Unmarshal(r3.Result, &perm); err != nil {
		t.Fatalf("perm unmarshal: %v", err)
	}
	if perm.Outcome.Outcome != "cancelled" {
		t.Fatalf("expected cancelled, got %+v", perm.Outcome)
	}
}

func TestACPWriteCapable(t *testing.T) {
	workdir := t.TempDir()
	content := "nested content exact"
	rel := "a/b/c.txt"
	line := `{"jsonrpc":"2.0","id":1,"method":"fs/write_text_file","params":{"path":"` + rel + `","content":"` + content + `"}}`
	handled, resp := handleACPClientRequest(line, workdir, true)
	if !handled || resp == nil {
		t.Fatalf("write not handled")
	}
	var r acpResponse
	if err := json.Unmarshal(resp, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("write capable error: %+v", r.Error)
	}
	data, err := os.ReadFile(filepath.Join(workdir, rel))
	if err != nil {
		t.Fatalf("read written: %v", err)
	}
	if string(data) != content {
		t.Fatalf("content = %q want %q", string(data), content)
	}
	line2 := `{"jsonrpc":"2.0","id":2,"method":"session/request_permission","params":{"options":[{"optionId":"ask-1","kind":"ask"},{"optionId":"allow-1","kind":"allow"},{"optionId":"allow-2","kind":"allowOnce"}]}}`
	handled, resp = handleACPClientRequest(line2, workdir, true)
	var r2 acpResponse
	if err := json.Unmarshal(resp, &r2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var perm struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(r2.Result, &perm); err != nil {
		t.Fatalf("perm unmarshal: %v", err)
	}
	if perm.Outcome.OptionID != "allow-1" {
		t.Fatalf("expected first allow-1, got %q", perm.Outcome.OptionID)
	}
	line3 := `{"jsonrpc":"2.0","id":3,"method":"session/request_permission","params":{"options":[{"optionId":"ask-1","kind":"ask"},{"optionId":"reject-1","kind":"reject"}]}}`
	handled, resp = handleACPClientRequest(line3, workdir, true)
	_ = handled
	var r3 acpResponse
	if err := json.Unmarshal(resp, &r3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := json.Unmarshal(r3.Result, &perm); err != nil {
		t.Fatalf("perm unmarshal: %v", err)
	}
	if perm.Outcome.OptionID != "ask-1" {
		t.Fatalf("expected first option fallback ask-1, got %q", perm.Outcome.OptionID)
	}
}
