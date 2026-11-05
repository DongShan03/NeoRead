package neoread

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"strings"
	"testing"
	"time"
)

func fakeDocumentMCP(url, token string) *mcp.ClientSession {
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-cli", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: documentMCPHTTPClient(token), DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		os.Exit(41)
	}
	return session
}
func fakeDocumentCall(session *mcp.ClientSession, name string, args any) json.RawMessage {
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || result.IsError || len(result.Content) == 0 {
		os.Exit(42)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		os.Exit(43)
	}
	return json.RawMessage(text.Text)
}
func fakeDocumentRead(session *mcp.ClientSession, call func(string, any) json.RawMessage) string {
	raw := call("search_document", map[string]any{"query": "agent attack interpretability cobalt", "limit": 12})
	items, err := libraryToolItems(raw)
	if err != nil {
		os.Exit(44)
	}
	refs := ""
	for _, item := range items {
		raw = call("read_passage", map[string]any{"blockId": item.BlockID})
		var read struct {
			Items []struct {
				Source string `json:"sourceId"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &read) != nil {
			os.Exit(45)
		}
		for _, e := range read.Items {
			refs += "[" + e.Source + "]"
		}
	}
	return "建议比较攻击与机制研究 " + refs
}
func runFakeClaudeDocument(mode, input string, emit func(any), gate func()) bool {
	config := ""
	tools := []string{}
	for i, arg := range os.Args {
		if arg == "--mcp-config" && i+1 < len(os.Args) && strings.HasSuffix(os.Args[i+1], ".json") {
			config = os.Args[i+1]
		}
		if arg == "--allowedTools" && i+1 < len(os.Args) {
			tools = strings.Split(os.Args[i+1], ",")
		}
	}
	if config == "" {
		return false
	}
	captureFakeDocumentPrompt(input)
	stat, err := os.Stat(config)
	if err != nil || stat.Mode().Perm() != 0600 {
		os.Exit(46)
	}
	data, err := os.ReadFile(config)
	if err != nil {
		os.Exit(47)
	}
	var cfg struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		os.Exit(48)
	}
	server := cfg.Servers["neoread_document"]
	token := strings.TrimPrefix(server.Headers["Authorization"], "Bearer ")
	if strings.Contains(strings.Join(os.Args, " "), token) || strings.Contains(strings.Join(os.Args, " "), "--safe-mode") {
		os.Exit(49)
	}
	if mode == "document-extra" {
		tools = append(tools, "Bash")
	}
	if mode == "document-missing" {
		tools = tools[1:]
	}
	emit(map[string]any{"type": "system", "subtype": "init", "tools": tools, "mcp_servers": []any{map[string]string{"name": "neoread_document", "status": "connected"}}})
	part := func(text string) {
		emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": text}}})
	}
	stop := func(reason string) {
		emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": reason}}})
	}
	if mode == "document-extra" || mode == "document-missing" {
		part("UNAUTHORIZED")
		return true
	}
	answer := "你好，世界"
	if strings.HasPrefix(mode, "document-") {
		session := fakeDocumentMCP(server.URL, token)
		defer session.Close()
		n := 0
		call := func(name string, args any) json.RawMessage {
			n++
			callID := fmt.Sprint(n)
			part("INTERMEDIATE_TEXT")
			stop("tool_use")
			emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": callID, "name": "mcp__neoread_document__" + name, "input": args}}}})
			raw := fakeDocumentCall(session, name, args)
			emit(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": string(raw)}}}})
			return raw
		}
		_ = call("get_outline", map[string]any{})
		answer = fakeDocumentRead(session, call)
	}
	runes := []rune(answer)
	part(string(runes[:len(runes)/2]))
	gate()
	if mode == "fail-after" || mode == "document-partial" {
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true})
		return true
	}
	if mode == "truncated" {
		return true
	}
	part(string(runes[len(runes)/2:]))
	stop("end_turn")
	emit(map[string]any{"type": "result", "subtype": "success", "result": answer})
	return true
}

func runFakeKimiDocument(mode string, emit func(any)) {
	decoder := json.NewDecoder(os.Stdin)
	sessionID := "session"
	var endpoint, token string
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if decoder.Decode(&req) != nil {
			return
		}
		reply := func(result any) { emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
		switch req.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"mcpCapabilities": map[string]bool{"http": mode != "document-unsupported"}, "promptCapabilities": map[string]bool{"image": true}}})
		case "session/new":
			var params struct {
				Servers []struct {
					URL     string                         `json:"url"`
					Headers []struct{ Name, Value string } `json:"headers"`
				} `json:"mcpServers"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if mode == "document-rejected" && len(params.Servers) > 0 {
				emit(map[string]any{"id": req.ID, "error": map[string]any{"code": -1, "message": "unsupported"}})
				continue
			}
			if len(params.Servers) > 0 {
				endpoint = params.Servers[0].URL
				for _, h := range params.Servers[0].Headers {
					if h.Name == "Authorization" {
						token = strings.TrimPrefix(h.Value, "Bearer ")
					}
				}
			}
			reply(map[string]any{"sessionId": sessionID, "models": map[string]string{"currentModelId": "test"}, "configOptions": []any{map[string]any{"id": "model", "currentValue": "test", "options": []any{map[string]string{"value": "test", "name": "Test"}}}}})
		case "session/set_config_option":
			reply(map[string]any{})
		case "session/prompt":
			captureFakeDocumentPrompt(string(req.Params))
			update := func(v any) {
				emit(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": v}})
			}
			part := func(text string) {
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}})
			}
			part("INTERMEDIATE_TEXT")
			if mode == "document-unexpected" {
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "bad", "title": "Bash", "status": "in_progress"})
				var cancel struct{ Method string }
				if decoder.Decode(&cancel) != nil || cancel.Method != "session/cancel" {
					os.Exit(51)
				}
				_ = os.WriteFile(os.Getenv("NEOREAD_AGENT_ACK"), []byte("cancelled"), 0600)
				part("LATE_TEXT")
				reply(map[string]string{"stopReason": "cancelled"})
				continue
			}
			answer := "回退模式回答"
			if endpoint != "" {
				session := fakeDocumentMCP(endpoint, token)
				n := 0
				call := func(name string, args any) json.RawMessage {
					n++
					toolID := fmt.Sprint(n)
					update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": toolID, "title": name + ": synthetic", "status": "in_progress"})
					emit(map[string]any{"id": "permission-" + toolID, "method": "session/request_permission", "params": map[string]any{"sessionId": sessionID, "toolCall": map[string]string{"toolCallId": toolID, "title": name}, "options": []any{map[string]string{"optionId": "yes", "kind": "allow_once"}}}})
					var permission struct {
						Result struct {
							Outcome struct{ Outcome, OptionID string }
						}
					}
					if decoder.Decode(&permission) != nil || permission.Result.Outcome.Outcome != "selected" {
						os.Exit(52)
					}
					raw := fakeDocumentCall(session, name, args)
					update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": toolID, "status": "completed"})
					return raw
				}
				answer = fakeDocumentRead(session, call)
				session.Close()
			}
			part(answer)
			if mode == "document-after-output-failure" {
				emit(map[string]any{"id": req.ID, "error": map[string]any{"code": -2, "message": "failed"}})
				continue
			}
			reply(map[string]string{"stopReason": "end_turn"})
		}
	}
}

func TestCLIDocumentParametersAndInventory(t *testing.T) {
	args := strings.Join(claudeDocumentArgs("/synthetic/config.json", false), " ")
	if strings.Contains(args, "--safe-mode") || !strings.Contains(strings.Join(claudeArgs(), " "), "--safe-mode") || !strings.Contains(args, "disableAllHooks") || !strings.Contains(args, "claudeMdExcludes") || strings.Contains(args, "--tools Web") {
		t.Fatal(args)
	}
	work := t.TempDir()
	path, err := writeDocumentMCPConfig(work, "http://127.0.0.1/mcp", "private-token")
	stat, e := os.Stat(path)
	if err != nil || e != nil || stat.Mode().Perm() != 0600 || strings.Contains(strings.Join(claudeDocumentArgs(path, false), " "), "private-token") {
		t.Fatal(err, e)
	}
	_ = os.RemoveAll(work)
	_, a, _ := libraryRetrievalFixture(t, []string{retrievalAbstract})
	for _, mode := range []string{"document-extra", "document-missing"} {
		t.Run(mode, func(t *testing.T) {
			fakeAgent(t, "claude", mode)
			visible := ""
			_, err := streamCLIDocumentChat(t.Context(), cliAdapter{a.server.Store.Root, "claude", "test"}, AIInput{Chat: a.chat}, func(e ProviderEvent) error { visible += e.Text + e.Draft; return nil })
			if errorKind(err) != ErrorCapability || visible != "" {
				t.Fatal(err, visible)
			}
		})
	}
}
func TestCLIDocumentToolsReadEvidenceAndDiscardIntermediateText(t *testing.T) {
	for _, provider := range []string{"claude", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			s, a, _ := libraryRetrievalFixture(t, []string{"Title", retrievalAbstract, "Experiments demonstrate agent attacks."}, []string{"Title", retrievalAbstract, "Mechanistic interpretability uses causal experiments."})
			fakeAgent(t, provider, "document-tools")
			visible := ""
			drafts, resets := 0, 0
			result, err := streamCLIDocumentChat(t.Context(), cliAdapter{s.Store.Root, provider, "test"}, AIInput{Chat: a.chat}, func(e ProviderEvent) error {
				visible += e.Text
				if e.Draft != "" {
					drafts++
				}
				if e.ResetDraft {
					resets++
				}
				return nil
			})
			if err != nil || len(a.Evidence) < 2 || drafts == 0 || resets == 0 || strings.Contains(visible, "INTERMEDIATE") || visible != result.Text {
				t.Fatal(result, err, len(a.Evidence), visible)
			}
			if err = s.saveDocumentAgentAnswer(t.Context(), a, result.Text, provider); err != nil {
				t.Fatal(err)
			}
			history, err := s.Store.chatHistory(libraryChatOwner, a.sessionID)
			if err != nil || len(history) != 1 || strings.Contains(history[0].Content, "INTERMEDIATE") || len(history[0].References) < 3 {
				t.Fatal(history, err)
			}
		})
	}
}
func TestKimiDocumentFallbackAndRuntimeRejection(t *testing.T) {
	for _, mode := range []string{"document-unsupported", "document-rejected", "document-unexpected", "document-after-output-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, a, _ := libraryRetrievalFixture(t, []string{retrievalAbstract}, []string{retrievalAbstract})
			ack := fakeAgent(t, "kimi", mode)
			visible := ""
			result, err := streamCLIDocumentChat(t.Context(), cliAdapter{s.Store.Root, "kimi", "test"}, AIInput{Chat: a.chat}, func(e ProviderEvent) error { visible += e.Text; return nil })
			if mode == "document-unsupported" || mode == "document-rejected" {
				if err != nil || a.Mode != "retrieval" || len(a.Evidence) != 2 || !strings.Contains(result.Text, "回退模式回答") {
					t.Fatal(result, err, a.Mode, len(a.Evidence))
				}
			} else {
				if err == nil || visible != "" || a.Mode != "agent" {
					t.Fatal(result, err, visible, a.Mode)
				}
				if mode == "document-unexpected" {
					if errorKind(err) != ErrorCapability {
						t.Fatal(err)
					}
					data, e := os.ReadFile(ack)
					if e != nil || string(data) != "cancelled" {
						t.Fatal("missing session/cancel", e)
					}
				}
			}
		})
	}
}
func TestKimiDocumentPermissionsDenyUnrelatedTools(t *testing.T) {
	for _, title := range []string{"search_document", "mcp__neoread_document__read_passage", "Bash", "ReadFile", "other/search_document", "WebSearch"} {
		raw := json.RawMessage(jsonText(map[string]any{"sessionId": "session", "toolCall": map[string]string{"title": title}, "options": []any{map[string]string{"kind": "allow_once", "optionId": "yes"}}}))
		got := jsonText(kimiDocumentPermission(raw, "session", true))
		want := title == "search_document" || title == "mcp__neoread_document__read_passage"
		if strings.Contains(got, "selected") != want {
			t.Fatal(title, got)
		}
	}
}

func TestLiveClaudeDocumentTools(t *testing.T) {
	if os.Getenv("NEOREAD_TEST_CLAUDE_DOCUMENT") != "1" {
		t.Skip("opt-in real Claude using synthetic papers")
	}
	s, a, _ := libraryRetrievalFixture(t, []string{retrievalAbstract, "The synthetic COBALT protocol tests agent attacks using a control group."}, []string{retrievalAbstract, "The synthetic COBALT protocol measures mechanistic interpretability using interventions."})
	a.chat.Question = "这是两篇合成论文。请依次调用 get_outline、search_document（关键词 COBALT），再 read_passage 读取两篇论文各一段。用中文比较两者，引用两篇的 [S编号]；只给一小段最终回答。"
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	drafts, resets := 0, 0
	result, err := streamCLIDocumentChat(ctx, cliAdapter{s.Store.Root, "claude", ""}, AIInput{Chat: a.chat}, func(e ProviderEvent) error {
		if e.Draft != "" {
			drafts++
		}
		if e.ResetDraft {
			resets++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.saveDocumentAgentAnswer(ctx, a, result.Text, "claude"); err != nil {
		t.Fatal(err)
	}
	docs := map[string]bool{}
	names := []string{}
	for _, e := range a.Evidence {
		docs[e.DocumentID] = true
	}
	for _, trace := range a.Traces {
		names = append(names, trace.Tool)
	}
	if len(docs) < 2 {
		t.Fatal("real CLI did not read both synthetic papers")
	}
	t.Logf("duration=%s tools=%v evidence=%d papers=%d drafts=%d resets=%d saved=true", time.Since(started), names, len(a.Evidence), len(docs), drafts, resets)
}

func captureFakeDocumentPrompt(prompt string) {
	if path := os.Getenv("NEOREAD_DOCUMENT_PROMPTS"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(53)
		}
		_ = json.NewEncoder(file).Encode(prompt)
		_ = file.Close()
	}
}
func TestLiveClaudeDocumentWebTools(t *testing.T) {
	if os.Getenv("NEOREAD_TEST_CLAUDE_WEB") != "1" {
		t.Skip("opt-in native Claude web tools using public queries")
	}
	s, a, _ := libraryRetrievalFixture(t, []string{retrievalAbstract})
	a.chat.WebSearch = true
	a.chat.Question = "请必须先调用 WebSearch 搜索 IANA example domains，然后调用 WebFetch 读取 https://example.com/ 。最终仅用中文一两句话，分别如实说明这两个工具是否成功，给普通 Markdown 网页链接；不讨论论文，不用本地文件工具。"
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	result, err := streamCLIDocumentChat(ctx, cliAdapter{s.Store.Root, "claude", ""}, AIInput{Chat: a.chat}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.saveDocumentAgentAnswer(ctx, a, result.Text, "claude"); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range a.chat.toolSnapshot() {
		names = append(names, tool.Tool+":"+tool.Status)
	}
	t.Logf("tools=%v sources=%d saved=true", names, len(a.chat.webSourcesSnapshot()))
	if len(names) < 2 {
		t.Fatal("native web tools were not both exercised")
	}
}
