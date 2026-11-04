package neoread

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func prepareConversationFixture(t *testing.T) (*Server, *documentAgent) {
	t.Helper()
	s, a := originalDocumentFixture(t)
	d, _ := s.Store.Document("doc")
	if err := os.WriteFile(s.Store.File(d), []byte("original fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := s.documentCorpus(t.Context(), "doc", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.corpus = c
	return s, a
}
func conversationRequest(t *testing.T, s *Server, provider, prompt string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(jsonText(map[string]any{"provider": provider, "prompt": prompt, "context": ""})))
}
func writeConversationReply(w http.ResponseWriter, text, continuation string) {
	w.Header().Set("Content-Type", "text/event-stream")
	data := map[string]any{"model": "deepseek-test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text, "reasoning_content": continuation}, "finish_reason": "stop"}}}
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", jsonText(data))
}
func TestAPIConversationPersistsCompactsAndRestarts(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	_, _ = s.Store.DB.Exec(`INSERT INTO settings VALUES('neoread','{"documentContextTokens":8192}')`)
	compactCalls, answerCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]any{"id": "test", "context_window": 8192}}})
			return
		}
		var payload struct {
			Messages []toolCompletionMessage `json:"messages"`
			Tools    []any                   `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("invalid request")
			return
		}
		first, _ := payload.Messages[0].Content.(string)
		if strings.Contains(first, "整理对话检查点") {
			compactCalls++
			if !strings.Contains(jsonText(payload.Messages), "CIVET-42") {
				t.Error("compaction lost the first-turn requirement")
			}
			writeConversationReply(w, "用户要求记住 CIVET-42，使用中文解释；后续问题应延续这个要求。", "")
			return
		}
		answerCalls++
		if len(payload.Tools) == 0 {
			t.Error("unexpected separate final-answer request")
		}
		if !strings.Contains(jsonText(payload.Messages), "CIVET-42") {
			t.Errorf("turn %d forgot first-turn context", answerCalls)
		}
		for _, m := range payload.Messages {
			if m.Role == "assistant" && m.Reasoning != "private-continuation" {
				t.Error("provider continuation lost across questions")
			}
		}
		writeConversationReply(w, "CIVET-42 "+strings.Repeat("这是已经解释过的内容。", 90), "private-continuation")
	}))
	defer server.Close()
	configureDocumentAPI(t, s, server.URL)
	for i := 0; i < 18; i++ {
		question := fmt.Sprintf("继续说明第 %d 点", i)
		if i == 0 {
			question = "请记住 CIVET-42，后面都用中文解释。"
		}
		out := conversationRequest(t, s, "api", question)
		if strings.Count(out.Body.String(), "event: done") != 1 {
			t.Fatalf("turn %d: %s", i, out.Body.String())
		}
		if !strings.Contains(out.Body.String(), "event: reasoning") {
			t.Fatal("provider thinking missing from its separate UI channel")
		}
		if i == 10 {
			s.Close()
			s = NewServer(s.Store, "test-secret", "")
			t.Cleanup(s.Close)
		}
	}
	if answerCalls != 18 || compactCalls == 0 {
		t.Fatal("missing single-loop replies or compaction", answerCalls, compactCalls)
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 36 || !strings.Contains(history[0].Content, "CIVET-42") {
		t.Fatal("fixed history clipping remains", len(history), err)
	}
	var raw string
	if err = s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc' AND provider='api'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state documentConversation
	_ = json.Unmarshal([]byte(raw), &state)
	if state.Compactions == 0 || state.LastMessageID != history[len(history)-1].ID || !strings.Contains(jsonText(state.Messages), "private-continuation") {
		t.Fatal("active API window not durably saved")
	}
	var archived int
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM chat_agent_runs WHERE json_array_length(json_extract(body,'$.conversationEvents'))>=2").Scan(&archived)
	if archived != 18 {
		t.Fatal("raw successful turns not archived", archived)
	}
}
func TestConversationCompactionKeepsProtocolGroups(t *testing.T) {
	call := completionToolCall{ID: "call", Type: "function"}
	call.Function.Name = "read_passage"
	paired := []toolCompletionMessage{{Role: "user", Content: "question"}, {Role: "assistant", Calls: []completionToolCall{call}, Reasoning: "private"}, {Role: "tool", CallID: "call", Content: "original"}, {Role: "assistant", Content: "answer"}}
	groups, err := conversationGroups(paired)
	if err != nil || len(groups) != 3 || len(groups[1]) != 2 {
		t.Fatal("tool pair was split", groups, err)
	}
	if _, err = conversationGroups(paired[:2]); err == nil {
		t.Fatal("compacted unfinished tool call")
	}
	if _, err = conversationGroups(paired[2:]); err == nil {
		t.Fatal("accepted orphan tool result")
	}
	text := strings.Repeat("中文⚡source", 3000)
	left := text
	var all strings.Builder
	for left != "" {
		part, rest := takeCompactionText(left, 500)
		if part == "" || estimateDocumentTokens(part) > 500 {
			t.Fatal("invalid compaction fragment")
		}
		all.WriteString(part)
		left = rest
	}
	if all.String() != text {
		t.Fatal("compaction input lost original data")
	}
}
func TestFailedAPITurnDoesNotReplaceConversation(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		calls++
		if calls == 2 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
			return
		}
		writeConversationReply(w, "remembered", "private")
	}))
	defer server.Close()
	configureDocumentAPI(t, s, server.URL)
	if out := conversationRequest(t, s, "api", "remember this"); !strings.Contains(out.Body.String(), "event: done") {
		t.Fatal(out.Body.String())
	}
	var before, after string
	_ = s.Store.DB.QueryRow("SELECT body FROM document_conversations").Scan(&before)
	out := conversationRequest(t, s, "api", "fail this time")
	if strings.Contains(out.Body.String(), "event: done") || !strings.Contains(out.Body.String(), "event: draft") {
		t.Fatal("partial turn incorrectly completed", out.Body.String())
	}
	_ = s.Store.DB.QueryRow("SELECT body FROM document_conversations").Scan(&after)
	if before != after {
		t.Fatal("failed turn replaced the committed context")
	}
}

func TestConversationContextDeltaAndRevision(t *testing.T) {
	s, a := originalDocumentFixture(t)
	a.provider = "api"
	state := documentConversation{ID: id(), ConnectionKey: "key", Revision: a.corpus.Revision, PassageCount: len(a.corpus.Passages), SeenSources: map[string]bool{"existing": true}}
	if err := saveDocumentConversation(t.Context(), s.Store.DB, "doc", "api", state); err != nil {
		t.Fatal(err)
	}
	input := AIInput{Prompt: "question", Chat: &chatInput{Question: "question", CurrentText: "unchanged source", Agent: a}}
	if err := a.prepareReading(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := a.beginConversation(t.Context(), input, "key"); err != nil {
		t.Fatal(err)
	}
	first := a.conversationInput(input, false)
	next := a.conversationInput(input, false)
	if !strings.Contains(first, "unchanged source") || strings.Contains(next, "unchanged source") {
		t.Fatal("same source resent every question", next)
	}
	if a.committedConversation.SeenSources[sourceKey("unchanged source")] {
		t.Fatal("uncommitted source changed durable checkpoint")
	}
	a.corpus.Revision = "new-source-version"
	if _, err := a.beginConversation(t.Context(), input, "key"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.conversationInput(input, false), "new-source-version") || a.conversation.SourceOffset != state.PassageCount {
		t.Fatal("source update silently reused old context/IDs")
	}
}

func TestLiveContinuousDocumentConversation(t *testing.T) {
	provider := os.Getenv("NEOREAD_TEST_CONVERSATION_PROVIDER")
	if provider != "codex" && provider != "api" {
		t.Skip("set NEOREAD_TEST_CONVERSATION_PROVIDER=codex or api; original synthetic text only")
	}
	s, a := prepareConversationFixture(t)
	config := AIConfig{Primary: provider, Models: map[string]string{}, Capabilities: map[string]Capability{}}
	if provider == "api" {
		raw, err := os.ReadFile(os.Getenv("NEOREAD_TEST_DOCUMENT_API_CONFIG"))
		if err != nil {
			t.Fatal("NeoRead API connection unavailable")
		}
		var saved savedConfig
		if json.Unmarshal(raw, &saved) != nil {
			t.Fatal("invalid NeoRead API config")
		}
		config.API = saved.Config.API
		if config.API.URL != "https://api.deepseek.com" && config.API.URL != "https://api.deepseek.com/v1" {
			t.Fatal("expected official DeepSeek endpoint")
		}
		model := "deepseek-v4-pro"
		config.Models[provider] = model
		config.APIModels = []string{model}
		config.Capabilities[apiCapabilityKey(model)] = Capability{Text: true, Tools: "passed", ToolSchemaVersion: documentToolSchemaVersion, Fingerprint: configPrint(config, apiCapabilityKey(model))}
	} else {
		config.Capabilities[provider] = Capability{Text: true, Fingerprint: configPrint(config, provider)}
	}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	questions := []string{"请记住口令 LANTERN-72841，后面都用中文回答。请读原文确认 cobalt 协议的 nonce，并给出处。", "只复述我上轮要求记住的口令。", "继续上次的讨论：口令是什么？cobalt 协议只适用于什么交易？请引用原文。"}
	firstThread := ""
	for i, q := range questions {
		start := time.Now()
		out := conversationRequest(t, s, provider, q)
		if !strings.Contains(out.Body.String(), "event: done") {
			t.Fatal(out.Body.String())
		}
		history, err := s.Store.chatHistory("doc")
		if err != nil {
			t.Fatal(err)
		}
		answer := history[len(history)-1]
		if i > 0 && !strings.Contains(answer.Content, "LANTERN-72841") {
			t.Fatal("prior-turn requirement lost", answer.Content)
		}
		var raw string
		if err = s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc' AND provider=?", provider).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var state documentConversation
		_ = json.Unmarshal([]byte(raw), &state)
		if provider == "codex" {
			if i == 0 {
				firstThread = state.NativeThread
			}
			if state.NativeThread != firstThread {
				t.Fatal("thread changed between questions")
			}
		}
		usage, _ := s.Store.scopedUsageReport("doc", true)
		cached := "unknown"
		if usage.Total.CachedInputTokens != nil {
			cached = fmt.Sprint(*usage.Total.CachedInputTokens)
		}
		t.Logf("provider=%s turn=%d elapsed=%s history=%d compactions=%d model-calls=%d input=%d cached=%v output=%d", provider, i+1, time.Since(start).Round(time.Millisecond), len(history), state.Compactions, len(usage.Calls), usage.Total.InputTokens, cached, usage.Total.OutputTokens)
		if i == 1 {
			if provider == "api" {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
				req, e := s.apiAgentAdapter(config, taskChat).resolve(ctx, false)
				if e != nil {
					cancel()
					t.Fatal(e)
				}
				a.ctx = ctx
				a.provider = provider
				a.conversation = &state
				a.contextTokens = 8192
				a.needsCompaction = true
				a.usageSink = s.processingUsageSink("doc", "chat", "live-compact")
				next, e := compactAPIConversation(ctx, config.API, req, a, state.Messages)
				if e != nil {
					cancel()
					t.Fatal(e)
				}
				state.Messages = next
				if e = saveDocumentConversation(ctx, s.Store.DB, "doc", provider, state); e != nil {
					cancel()
					t.Fatal(e)
				}
				cancel()
				t.Logf("live API checkpoint persisted: compactions=%d", state.Compactions)
			}
			s.Close()
			s = NewServer(s.Store, "test-secret", "")
			t.Cleanup(s.Close)
		}
	}
}

func runPersistentDocumentProcess() {
	url := ""
	for _, arg := range os.Args {
		if v, ok := strings.CutPrefix(arg, "mcp_servers.neoread_document.url="); ok {
			url, _ = strconv.Unquote(v)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), maxProviderFrame)
	work, _ := os.Getwd()
	file := filepath.Join(work, "native-state.json")
	thread := "thread-" + filepath.Base(work)
	type nativeState struct {
		Turn     int
		Head     int
		Remember string
		Source   string
		Archived bool
	}
	load := func() nativeState {
		var state nativeState
		b, _ := os.ReadFile(file)
		_ = json.Unmarshal(b, &state)
		return state
	}
	save := func(state nativeState) { _ = os.WriteFile(file, []byte(jsonText(state)), 0600) }
	for scan.Scan() {
		var r codexRPCMessage
		if json.Unmarshal(scan.Bytes(), &r) != nil {
			os.Exit(2)
		}
		f, e := os.OpenFile(os.Getenv("NEOREAD_CONVERSATION_CAPTURE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if e != nil {
			os.Exit(3)
		}
		_ = json.NewEncoder(f).Encode(map[string]any{"pid": os.Getpid(), "method": r.Method, "params": r.Params})
		f.Close()
		reply := func(v any) { _ = enc.Encode(map[string]any{"id": r.ID, "result": v}) }
		switch r.Method {
		case "initialize":
			reply(map[string]any{})
		case "model/list":
			reply(map[string]any{"data": []any{map[string]any{"model": "test", "isDefault": true}}})
		case "config/read":
			servers := map[string]any{}
			if url != "" {
				servers["neoread_document"] = map[string]any{"enabled": true, "url": url}
			}
			reply(map[string]any{"config": map[string]any{"mcp_servers": servers, "features": map[string]bool{}, "model_context_window": 64000, "web_search": "disabled"}})
		case "thread/start":
			save(nativeState{})
			reply(map[string]any{"thread": map[string]any{"id": thread, "ephemeral": false}, "model": "test"})
		case "thread/archive", "thread/unarchive":
			var p struct {
				Thread string `json:"threadId"`
			}
			_ = json.Unmarshal(r.Params, &p)
			target := filepath.Join(filepath.Dir(work), strings.TrimPrefix(p.Thread, "thread-"), "native-state.json")
			previous := file
			file = target
			state := load()
			state.Archived = r.Method == "thread/archive"
			save(state)
			file = previous
			reply(map[string]any{"thread": map[string]any{"id": p.Thread}})
		case "thread/resume":
			var p struct {
				Thread string `json:"threadId"`
			}
			_ = json.Unmarshal(r.Params, &p)
			if p.Thread != thread {
				os.Exit(4)
			}
			if load().Archived {
				_ = enc.Encode(map[string]any{"id": r.ID, "error": map[string]any{"code": -32600, "message": "thread is archived"}})
				continue
			}
			if failure := os.Getenv("NEOREAD_CONVERSATION_RESUME_ERROR"); failure != "" {
				_ = enc.Encode(map[string]any{"id": r.ID, "error": json.RawMessage(failure)})
				continue
			}
			reply(map[string]any{"thread": map[string]any{"id": thread}, "model": "test"})
		case "thread/turns/list":
			state := load()
			data := []any{}
			if state.Head > 0 {
				data = append(data, map[string]any{"id": fmt.Sprintf("turn-%d", state.Head)})
			}
			reply(map[string]any{"data": data})
		case "thread/revert":
			state := load()
			var p struct {
				Before string `json:"beforeTurnId"`
			}
			_ = json.Unmarshal(r.Params, &p)
			head, _ := strconv.Atoi(strings.TrimPrefix(p.Before, "turn-"))
			state.Head = head - 1
			save(state)
			reply(map[string]any{"thread": map[string]any{"id": thread}})
		case "mcpServerStatus/list":
			tools := map[string]any{}
			for _, d := range documentToolDefinitions {
				tools[d.Name] = map[string]any{}
			}
			reply(map[string]any{"data": []any{map[string]any{"name": "neoread_document", "tools": tools}}})
		case "turn/start":
			state := load()
			state.Turn++
			state.Head = state.Turn
			turn := fmt.Sprintf("turn-%d", state.Turn)
			var p struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(r.Params, &p)
			input := ""
			for _, x := range p.Input {
				input += x.Text
			}
			if strings.Contains(input, "CIVET-42") {
				state.Remember = "CIVET-42"
			}
			reply(map[string]any{"turn": map[string]any{"id": turn}})
			notify := func(method string, params map[string]any) {
				params["threadId"] = thread
				params["turnId"] = turn
				_ = enc.Encode(map[string]any{"method": method, "params": params})
			}
			notify("item/started", map[string]any{"item": map[string]string{"id": "commentary", "type": "agentMessage", "phase": "commentary"}})
			notify("item/agentMessage/delta", map[string]any{"itemId": "commentary", "delta": "private planning prose"})
			if state.Source == "" || strings.HasSuffix(input, "verify-again") {
				body := jsonText(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "read_passage", "arguments": map[string]string{"blockId": os.Getenv("NEOREAD_DOCUMENT_TEST_BLOCK")}}})
				request, _ := http.NewRequest("POST", url, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer "+os.Getenv("NEOREAD_DOCUMENT_MCP_TOKEN"))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Accept", "application/json, text/event-stream")
				request.Header.Set("MCP-Protocol-Version", "2025-11-25")
				response, e := http.DefaultClient.Do(request)
				if e != nil {
					os.Exit(5)
				}
				raw, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if !strings.Contains(string(raw), "COBALT-19") {
					os.Exit(6)
				}
				state.Source = "S2"
			}
			if state.Turn == 10 {
				notify("item/started", map[string]any{"item": map[string]string{"id": "compact", "type": "contextCompaction"}})
				notify("item/completed", map[string]any{"item": map[string]string{"id": "compact", "type": "contextCompaction"}})
			}
			text := state.Remember + " COBALT-19 [" + state.Source + "]."
			status := "completed"
			if strings.HasSuffix(input, "cancel-test") {
				status = "failed"
			}
			notify("item/started", map[string]any{"item": map[string]string{"id": "answer", "type": "agentMessage", "phase": "final_answer"}})
			notify("item/agentMessage/delta", map[string]any{"itemId": "answer", "delta": state.Remember + " "})
			notify("item/agentMessage/delta", map[string]any{"itemId": "answer", "delta": "COBALT-19 [" + state.Source + "]."})
			notify("item/completed", map[string]any{"item": map[string]string{"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": text}})
			notify("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{"modelContextWindow": 64000, "total": TokenCounts{InputTokens: int64(state.Turn * 20), OutputTokens: int64(state.Turn * 10), TotalTokens: int64(state.Turn * 30)}}})
			save(state)
			notify("turn/completed", map[string]any{"turn": map[string]string{"id": turn, "status": status}})
		}
	}
}

func TestCodexConversationReportsActiveWriterAndPreservesHistory(t *testing.T) {
	s, a := prepareConversationFixture(t)
	fakeAgent(t, "codex", "document-conversation")
	capture := filepath.Join(t.TempDir(), "capture.jsonl")
	t.Setenv("NEOREAD_CONVERSATION_CAPTURE", capture)
	t.Setenv("NEOREAD_DOCUMENT_TEST_BLOCK", a.corpus.Passages[1].ID)
	if out := conversationRequest(t, s, "codex", "记住 CIVET-42"); !strings.Contains(out.Body.String(), "event: done") {
		t.Fatal(out.Body.String())
	}
	var before string
	if err := s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc' AND provider='codex'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = NewServer(s.Store, "test-secret", "")
	t.Cleanup(s.Close)
	t.Setenv("NEOREAD_CONVERSATION_RESUME_ERROR", `{"code":-32600,"message":"thread private-thread already has an active writer: PRIVATE-TOKEN","data":{"secret":"PRIVATE-DATA"}}`)
	out := conversationRequest(t, s, "codex", "继续解释")
	text := out.Body.String()
	if !strings.Contains(text, "event: error") || !strings.Contains(text, "会话仍由 Codex App 占用") || strings.Contains(text, "event: done") || strings.Contains(text, "PRIVATE-") || strings.Contains(text, "private-thread") {
		t.Fatal("occupied thread did not produce a safe, actionable error", text)
	}
	var after string
	if err := s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc' AND provider='codex'").Scan(&after); err != nil || after != before {
		t.Fatal("resume failure changed the committed conversation", err)
	}
	history := chatMessages(t, s)
	if len(history) != 3 || history[0].Role != "user" || history[1].Role != "assistant" || history[2].Role != "user" {
		t.Fatal("resume failure lost history or saved a failed answer", history)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `"method":"thread/start"`) != 1 || strings.Count(string(data), `"method":"turn/start"`) != 1 || strings.Count(string(data), `"method":"thread/resume"`) != 1 || strings.Contains(string(data), `"method":"thread/revert"`) {
		t.Fatal("occupied thread was retried, replaced or reverted", string(data))
	}
	t.Setenv("NEOREAD_CONVERSATION_RESUME_ERROR", "")
	if out := conversationRequest(t, s, "codex", "占用已释放，继续解释"); !strings.Contains(out.Body.String(), "event: done") || !strings.Contains(out.Body.String(), "CIVET-42") {
		t.Fatal("same conversation did not recover after the writer was released", out.Body.String())
	}
}
func TestCodexConversationRetainsThreadAndSources(t *testing.T) {
	s, a := prepareConversationFixture(t)
	fakeAgent(t, "codex", "document-conversation")
	capture := filepath.Join(t.TempDir(), "capture.jsonl")
	t.Setenv("NEOREAD_CONVERSATION_CAPTURE", capture)
	t.Setenv("NEOREAD_DOCUMENT_TEST_BLOCK", a.corpus.Passages[1].ID)
	for i := 0; i < 13; i++ {
		prompt := fmt.Sprintf("继续第 %d 个问题", i)
		if i == 0 {
			prompt = "记住 CIVET-42"
		}
		if i == 11 {
			prompt = "verify-again"
		}
		out := conversationRequest(t, s, "codex", prompt)
		if strings.Count(out.Body.String(), "event: done") != 1 || !strings.Contains(out.Body.String(), "CIVET-42") || strings.Contains(out.Body.String(), "private planning prose") {
			t.Fatalf("turn %d: %s", i, out.Body.String())
		}
		if i == 9 && !strings.Contains(out.Body.String(), "正在整理对话上下文") {
			t.Fatal("native compact event was lost")
		}
		if i == 10 {
			s.Close()
			s = NewServer(s.Store, "test-secret", "")
			t.Cleanup(s.Close)
		}
	}
	// A failed native turn stays uncommitted locally. The next turn resumes the
	// same persistent thread and reverts only the uncommitted native turn.
	out := conversationRequest(t, s, "codex", "cancel-test")
	if strings.Contains(out.Body.String(), "event: done") {
		t.Fatal("failed turn saved")
	}
	out = conversationRequest(t, s, "codex", "继续解释")
	if !strings.Contains(out.Body.String(), "event: done") || !strings.Contains(out.Body.String(), "CIVET-42") {
		t.Fatal(out.Body.String())
	}
	_, _ = s.Store.DB.Exec(`CREATE TRIGGER fail_conversation_save BEFORE INSERT ON messages WHEN json_extract(NEW.body,'$.role')='assistant' BEGIN SELECT RAISE(ABORT,'save failed'); END`)
	out = conversationRequest(t, s, "codex", "save-failure")
	if strings.Contains(out.Body.String(), "event: done") {
		t.Fatal("unsaved answer acknowledged")
	}
	_, _ = s.Store.DB.Exec("DROP TRIGGER fail_conversation_save")
	out = conversationRequest(t, s, "codex", "继续上次的解释")
	if !strings.Contains(out.Body.String(), "event: done") {
		t.Fatal("failed save poisoned native continuation", out.Body.String())
	}
	data, _ := os.ReadFile(capture)
	starts, resumes, reverts := 0, 0, 0
	turns := []string{}
	scan := bufio.NewScanner(strings.NewReader(string(data)))
	scan.Buffer(make([]byte, 4096), maxProviderFrame)
	for scan.Scan() {
		var e struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(scan.Bytes(), &e)
		switch e.Method {
		case "thread/start":
			starts++
			if strings.Contains(string(e.Params), `"ephemeral":true`) {
				t.Fatal("persistent thread marked ephemeral")
			}
		case "thread/resume":
			resumes++
		case "thread/revert":
			reverts++
		case "turn/start":
			turns = append(turns, string(e.Params))
		}
	}
	if starts != 1 || resumes < 2 || reverts != 2 || len(turns) != 17 {
		t.Fatal("thread recreated per question", starts, resumes, reverts, len(turns))
	}
	if strings.Contains(turns[1], "用户问题：记住 CIVET-42") || !strings.Contains(turns[1], "继续第 1 个问题") {
		t.Fatal("native history resent instead of appended", turns[1])
	}
	var messages int
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE document_id='doc'").Scan(&messages)
	if messages != 32 {
		t.Fatal("history or failed-turn boundary lost", messages)
	}
}

func TestLiveDocumentDeepSeek(t *testing.T) {
	path := os.Getenv("NEOREAD_TEST_DOCUMENT_API_CONFIG")
	if path == "" {
		t.Skip("set NEOREAD_TEST_DOCUMENT_API_CONFIG to NeoRead's existing connection config; no configuration is changed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("NeoRead connection file unavailable")
	}
	var saved savedConfig
	if json.Unmarshal(data, &saved) != nil {
		t.Fatal("NeoRead connection config invalid")
	}
	connection := saved.Config.API
	if !strings.HasPrefix(connection.URL, "https://api.deepseek.com") || connection.Key == "" {
		t.Fatal("expected an existing official DeepSeek connection")
	}
	model := os.Getenv("NEOREAD_TEST_DOCUMENT_MODEL")
	if model == "" {
		for _, id := range []string{"deepseek-v4-pro", "deepseek-flash"} {
			if saved.Config.Capabilities[apiCapabilityKey(id)].Text {
				model = id
				break
			}
		}
	}
	if model == "" {
		t.Fatal("choose an already configured DeepSeek model")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	status, err := probeAPIDocumentTools(ctx, connection, model)
	if err != nil || status != "passed" {
		t.Fatalf("DeepSeek tool round-trip: %s %v", status, err)
	}
	s, a := originalDocumentFixture(t)
	a.ctx = ctx
	input := AIInput{Prompt: "Read both sections of the original document. Report the cobalt protocol's exact nonce, its applicability condition, and the number of experiment transactions. Search and read original passages; cite [Snumber].", Chat: &chatInput{Question: "Compare definition and experiment", Agent: a}}
	var delta strings.Builder
	start := time.Now()
	first := time.Duration(0)
	result, err := streamAPIDocumentChat(ctx, connection, completionRequest{Model: model, Effort: "high", Tools: true}, input, func(event ProviderEvent) error {
		if event.Text != "" {
			if first == 0 {
				first = time.Since(start)
			}
			delta.WriteString(event.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != delta.String() || !strings.Contains(result.Text, "37") || !strings.Contains(result.Text, "COBALT-19") || len(a.Evidence) < 2 {
		t.Fatalf("missing original evidence: calls=%d sources=%d answer=%s", len(a.Traces), len(a.Evidence), result.Text)
	}
	if _, err = a.references(result.Text); err != nil {
		t.Fatal(err)
	}
	for _, trace := range a.Traces {
		t.Logf("tool=%s args=%s", trace.Tool, trace.Arguments)
	}
	t.Logf("model=%s thinking=enabled effort=high elapsed=%s first-final-delta=%s sources=%d answer=%s", model, time.Since(start).Round(time.Millisecond), first.Round(time.Millisecond), len(a.Evidence), result.Text)
	_ = s
}

func TestLiveDocumentCodex(t *testing.T) {
	if os.Getenv("NEOREAD_TEST_DOCUMENT_CODEX") != "1" {
		t.Skip("set NEOREAD_TEST_DOCUMENT_CODEX=1; uses CLI login and only original synthetic passages")
	}
	s, a := originalDocumentFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	a.ctx = ctx
	input := AIInput{Prompt: "Read the original document. What nonce does the cobalt protocol use, what condition limits it, and how many transactions were used in the experiment? Search and read both sections. Cite the source IDs.", Chat: &chatInput{DocumentID: "doc", Question: "Compare definition and experiment", Agent: a}}
	var delta strings.Builder
	start := time.Now()
	first := time.Duration(0)
	result, e := streamCodexDocumentChat(ctx, s.Store.Root, os.Getenv("NEOREAD_TEST_DOCUMENT_MODEL"), input, func(event ProviderEvent) error {
		if event.Text != "" {
			if first == 0 {
				first = time.Since(start)
			}
			delta.WriteString(event.Text)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if result.Text != delta.String() || !strings.Contains(result.Text, "37") || !strings.Contains(result.Text, "COBALT-19") {
		t.Fatalf("answer mismatch: %s", result.Text)
	}
	if len(a.Traces) < 1 || len(a.Evidence) < 2 {
		t.Fatalf("no original evidence: calls=%d sources=%d", len(a.Traces), len(a.Evidence))
	}
	if _, e = a.references(result.Text); e != nil {
		t.Fatal(e)
	}
	for _, trace := range a.Traces {
		t.Logf("tool=%s args=%s", trace.Tool, trace.Arguments)
	}
	t.Logf("elapsed=%s first-final-delta=%s sources=%d answer=%s", time.Since(start).Round(time.Millisecond), first.Round(time.Millisecond), len(a.Evidence), result.Text)
}
