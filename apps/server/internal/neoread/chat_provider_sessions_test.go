package neoread

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runChatSessionProcess() {
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), maxProviderFrame)
	enc := json.NewEncoder(os.Stdout)
	turn := 0
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			os.Exit(1)
		}
		f, err := os.OpenFile(os.Getenv("NEOREAD_CHAT_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(f).Encode(map[string]any{"pid": os.Getpid(), "method": req.Method, "params": req.Params})
		_ = f.Close()
		reply := func(v any) { _ = enc.Encode(map[string]any{"id": req.ID, "result": v}) }
		switch req.Method {
		case "config/read":
			reply(map[string]any{"config": map[string]any{"mcp_servers": map[string]any{}, "features": map[string]bool{}, "web_search": "disabled"}})
		case "initialize":
			reply(map[string]any{})
		case "model/list":
			reply(map[string]any{"data": []any{map[string]any{"model": "test-model", "isDefault": true}}})
		case "thread/start":
			reply(map[string]any{"thread": map[string]string{"id": "thread"}, "model": "test-model"})
		case "turn/start":
			turn++
			tid := fmt.Sprintf("turn-%d", turn)
			// Old turn notifications must not leak into a new response.
			if turn > 1 {
				_ = enc.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread", "turnId": fmt.Sprintf("turn-%d", turn-1), "itemId": "old", "delta": "stale"}})
			}
			reply(map[string]any{"turn": map[string]string{"id": tid}})
			notify := func(method string, p map[string]any) {
				p["threadId"] = "thread"
				p["turnId"] = tid
				_ = enc.Encode(map[string]any{"method": method, "params": p})
			}
			if os.Getenv("NEOREAD_CHAT_SKIP_USAGE") != "1" {
				notify("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{"total": TokenCounts{InputTokens: int64(turn * 20), OutputTokens: int64(turn * 10), TotalTokens: int64(turn * 30), CachedInputTokens: tokenCount(int64(turn * 8))}}})
			}
			notify("item/started", map[string]any{"item": map[string]string{"id": "answer", "type": "agentMessage", "phase": "final_answer"}})
			notify("item/agentMessage/delta", map[string]any{"itemId": "answer", "delta": "已收到"})
			notify("item/completed", map[string]any{"item": map[string]string{"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": "已收到"}})
			notify("turn/completed", map[string]any{"turn": map[string]string{"id": tid, "status": "completed"}})
		}
	}
}
func chatFixture(t *testing.T) (*Server, string) {
	t.Helper()
	fakeAgent(t, "codex", "chat-session")
	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("NEOREAD_CHAT_CAPTURE", capture)
	s := testServer(t)
	for _, doc := range []string{"doc", "other"} {
		if _, err := s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at) VALUES(?,'pdf','test','',1,?,?)", doc, now(), now()); err != nil {
			t.Fatal(err)
		}
	}
	c := AIConfig{Primary: "codex", Models: map[string]string{"codex": "test-model"}, Capabilities: map[string]Capability{}}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	return s, capture
}
func sendChat(t *testing.T, s *Server, doc, question, source string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"provider": "codex", "prompt": question, "context": source})
	return request(t, s, "POST", "/api/documents/"+doc+"/chat", strings.NewReader(string(b))).Body.String()
}
func capturedTurns(t *testing.T, path string) ([]int, []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	var texts []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var req struct {
			PID    int    `json:"pid"`
			Method string `json:"method"`
			Params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatal(err)
		}
		if req.Method == "turn/start" {
			pids = append(pids, req.PID)
			texts = append(texts, req.Params.Input[0].Text)
		}
	}
	return pids, texts
}
func TestCodexChatReuseAndUsage(t *testing.T) {
	s, path := chatFixture(t)
	for i := 0; i < 3; i++ {
		if body := sendChat(t, s, "doc", fmt.Sprintf("问题%d", i), "预算730元"); !strings.Contains(body, "event: done") || strings.Contains(body, "stale") {
			t.Fatal(body)
		}
	}
	pids, texts := capturedTurns(t, path)
	if len(pids) != 3 || pids[0] != pids[1] || pids[1] != pids[2] {
		t.Fatalf("not reused: %v", pids)
	}
	if !strings.Contains(texts[0], "预算730元") || strings.Contains(texts[1], "预算730元") || strings.Contains(texts[1], "问题0") || !strings.Contains(texts[1], "问题1") {
		t.Fatalf("incorrect inputs: %v", texts)
	}
	rows, err := s.Store.DB.Query("SELECT body FROM processing_usage WHERE json_extract(body,'$.stage')='chat'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var b string
		var call UsageCall
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(b), &call); err != nil {
			t.Fatal(err)
		}
		n++
		tok := call.Models[0].Tokens
		if tok == nil || tok.TotalTokens != 30 || tok.CachedInputTokens == nil || *tok.CachedInputTokens != 8 {
			t.Fatalf("per-turn usage: %s", b)
		}
	}
	if n != 3 {
		t.Fatalf("usage calls %d", n)
	}
}
func TestCodexChatResetsAndRehydrates(t *testing.T) {
	s, path := chatFixture(t)
	if b := sendChat(t, s, "doc", "第一问", "原始资料"); !strings.Contains(b, "event: done") {
		t.Fatal(b)
	}
	if b := sendChat(t, s, "other", "另一文档", ""); !strings.Contains(b, "event: done") {
		t.Fatal(b)
	}
	if b := sendChat(t, s, "doc", "回来核对", ""); !strings.Contains(b, "event: done") {
		t.Fatal(b)
	}
	pids, texts := capturedTurns(t, path)
	if len(pids) != 3 || pids[0] == pids[1] || pids[1] == pids[2] {
		t.Fatal(pids)
	}
	if !strings.Contains(texts[2], "第一问") || !strings.Contains(texts[2], "原始资料") || !strings.Contains(texts[2], "已收到") {
		t.Fatal(texts[2])
	}
	c := s.aiConfig()
	c.Models["codex"] = "different-model"
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	if b := sendChat(t, s, "doc", "更换模型", ""); !strings.Contains(b, "event: done") {
		t.Fatal(b)
	}
	pids, _ = capturedTurns(t, path)
	if pids[2] == pids[3] {
		t.Fatal("model change reused session")
	}
}
func TestCodexChatDiscardsUnsavedTurn(t *testing.T) {
	s, path := chatFixture(t)
	if b := sendChat(t, s, "doc", "首问", "资料"); !strings.Contains(b, "event: done") {
		t.Fatal(b)
	}
	_, err := s.Store.DB.Exec(`CREATE TRIGGER reject_chat BEFORE INSERT ON messages WHEN json_extract(NEW.body,'$.role')='assistant' BEGIN SELECT RAISE(FAIL,'save failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if b := sendChat(t, s, "doc", "保存失败", ""); !strings.Contains(b, "event: error") || strings.Contains(b, "event: done") {
		t.Fatal(b)
	}
	if s.codexChat.entry != nil {
		t.Fatal("kept unsaved provider state")
	}
	if _, err := s.Store.DB.Exec("DROP TRIGGER reject_chat"); err != nil {
		t.Fatal(err)
	}
	if b := sendChat(t, s, "doc", "恢复", ""); !strings.Contains(b, "event: done") {
		t.Fatal(b)
	}
	pids, _ := capturedTurns(t, path)
	if pids[0] != pids[1] || pids[1] == pids[2] {
		t.Fatal(pids)
	}
}
func TestCodexChatCancellation(t *testing.T) {
	fakeAgent(t, "codex", "protocol-cancel")
	s := testServer(t)
	chat := &chatInput{DocumentID: "doc"}
	session, in, err := s.codexChat.acquire(t.Context(), s.Store.Root, "test", AIInput{Prompt: "问", Chat: chat})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = session.generate(ctx, in, func(string) { cancel() })
	if err == nil {
		t.Fatal("canceled turn succeeded")
	}
	s.codexChat.finish(chat, "", false)
	if s.codexChat.entry != nil {
		t.Fatal("canceled session retained")
	}
}

func TestCodexChatNewSourceAndHistoryMismatch(t *testing.T) {
	s, path := chatFixture(t)
	for _, pair := range [][2]string{{"第一问", "资料甲"}, {"第二问", "资料乙"}, {"第三问", "资料乙"}} {
		if b := sendChat(t, s, "doc", pair[0], pair[1]); !strings.Contains(b, "event: done") {
			t.Fatal(b)
		}
	}
	pids, texts := capturedTurns(t, path)
	if pids[0] != pids[1] || pids[1] != pids[2] || !strings.Contains(texts[1], "资料乙") || strings.Contains(texts[1], "资料甲") || strings.Contains(texts[2], "资料乙") {
		t.Fatalf("source reuse: %v %v", pids, texts)
	}
	if err := s.Store.saveMessage(Message{DocumentID: "doc", Role: "user", Content: "外部新增问题"}); err != nil {
		t.Fatal(err)
	}
	if b := sendChat(t, s, "doc", "第四问", ""); !strings.Contains(b, "event: done") {
		t.Fatal(b)
	}
	pids, texts = capturedTurns(t, path)
	if pids[2] == pids[3] || !strings.Contains(texts[3], "外部新增问题") {
		t.Fatal("did not rehydrate changed persisted history")
	}
}
func TestCodexChatLimitsEffortAndClose(t *testing.T) {
	for _, reason := range []string{"effort", "turns", "sources", "close", "delete"} {
		t.Run(reason, func(t *testing.T) {
			s, _ := chatFixture(t)
			if b := sendChat(t, s, "doc", "首问", "资料"); !strings.Contains(b, "event: done") {
				t.Fatal(b)
			}
			old := s.codexChat.entry
			work := old.work
			followUp := ""
			switch reason {
			case "effort":
				old.key = "old-effort"
			case "turns":
				old.turns = 64
			case "sources":
				// Under budget on its own, but the new source would push it over.
				old.sourceBytes = 250000
				followUp = strings.Repeat("新", 80000/3)
			case "close":
				s.Close()
			case "delete":
				s.codexChat.deleteDocument("doc")
			}
			if reason != "close" && reason != "delete" {
				if b := sendChat(t, s, "doc", "后续", followUp); !strings.Contains(b, "event: done") {
					t.Fatal(b)
				}
				if s.codexChat.entry == old {
					t.Fatal("session limit/config change did not reset")
				}
			}
			if _, err := os.Stat(work); !os.IsNotExist(err) {
				t.Fatalf("work directory retained: %v", err)
			}
		})
	}
}
func TestCodexUsageRejectsDecreasingTotals(t *testing.T) {
	prior := &TokenCounts{InputTokens: 20, OutputTokens: 10, TotalTokens: 30, CachedInputTokens: tokenCount(8)}
	if got := codexUsageDelta(&TokenCounts{InputTokens: 19, OutputTokens: 10, TotalTokens: 29}, prior); got != nil {
		t.Fatal(got)
	}
	if got := codexUsageDelta(&TokenCounts{InputTokens: 40, OutputTokens: 20, TotalTokens: 60, CachedInputTokens: tokenCount(16)}, prior); got == nil || got.TotalTokens != 30 || got.CachedInputTokens == nil || *got.CachedInputTokens != 8 {
		t.Fatal(got)
	}
}

func TestCodexChatMissingUsageRebuildsBeforeNextTurn(t *testing.T) {
	s, path := chatFixture(t)
	t.Setenv("NEOREAD_CHAT_SKIP_USAGE", "1")
	for _, q := range []string{"第一问", "第二问"} {
		if b := sendChat(t, s, "doc", q, "原始资料"); !strings.Contains(b, "event: done") {
			t.Fatal(b)
		}
	}
	pids, texts := capturedTurns(t, path)
	if len(pids) != 2 || pids[0] == pids[1] || !strings.Contains(texts[1], "第一问") {
		t.Fatal("missing usage must rebuild and rehydrate instead of attributing two turns to one call")
	}
}

func TestCodexFollowupKeepsTitleAndSummary(t *testing.T) {
	s, path := chatFixture(t)
	r := request(t, s, "PATCH", "/api/documents/doc", strings.NewReader(`{"title":"标题复用测试","metadata":{"abstract":"摘要复用测试"}}`))
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	for _, question := range []string{"普通问题", "继续追问"} {
		if body := sendChat(t, s, "doc", question, ""); !strings.Contains(body, "event: done") {
			t.Fatal(body)
		}
	}
	_, texts := capturedTurns(t, path)
	if len(texts) != 2 {
		t.Fatal(texts)
	}
	summaryID := sourceKey("[文章摘要]\n摘要复用测试")
	for _, prompt := range texts {
		if !strings.Contains(prompt, "标题复用测试") || !strings.Contains(prompt, "Document summary: "+summaryID) {
			t.Fatal(prompt)
		}
	}
	if !strings.Contains(texts[0], "摘要复用测试") {
		t.Fatal("summary never reached the session")
	}
}

func TestCodexRPCErrorsKeepSafeDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		code          int
		activeWriter  bool
	}{
		{"occupied", `{"code":-32600,"message":"thread PRIVATE-THREAD already has an active writer: PRIVATE-TOKEN","data":{"path":"PRIVATE-PATH"}}`, -32600, true},
		{"other invalid request", `{"code":-32600,"message":"no active writer; PRIVATE-TEXT"}`, -32600, false},
		{"unsupported method", `{"code":-32601,"message":"thread PRIVATE-THREAD already has an active writer"}`, -32601, false},
		{"internal error", `{"code":-32603,"message":"PRIVATE-TEXT"}`, -32603, false},
		{"malformed error", `{"code":"PRIVATE-CODE","message":"PRIVATE-TEXT"}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			session := &codexSession{scan: bufio.NewScanner(strings.NewReader(`{"id":1,"error":` + tc.payload + "}\n"))}
			life := &rpcLifecycle{encoder: json.NewEncoder(io.Discard)}
			_, err := session.callRPC(t.Context(), life, "thread/resume", map[string]string{"threadId": "PRIVATE-THREAD"}, func(codexRPCMessage) error { return nil })
			if !errors.Is(err, errCodexRPCResponse) {
				t.Fatal("RPC error compatibility lost", err)
			}
			var rpcError *codexRPCError
			if tc.code != 0 {
				if !errors.As(err, &rpcError) || rpcError.Code != tc.code || rpcError.activeWriter != tc.activeWriter {
					t.Fatal("RPC classification lost", err)
				}
				var event map[string]any
				if json.Unmarshal(logs.Bytes(), &event) != nil || event["method"] != "thread/resume" || event["rpcCode"] != float64(tc.code) || (event["reason"] == "active_writer") != tc.activeWriter {
					t.Fatal("safe diagnostic fields lost", logs.String())
				}
			} else if errors.As(err, &rpcError) {
				t.Fatal("malformed RPC error accepted")
			}
			if strings.Contains(err.Error()+logs.String(), "PRIVATE-") {
				t.Fatal("upstream message, data or request parameters leaked")
			}
		})
	}
}

func TestCodexFeatureEnabledAcceptsBoolAndTable(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		enabled, ok bool
	}{
		{"", false, true},
		{"null", false, true},
		{"false", false, true},
		{"true", true, true},
		{`{"enabled":false}`, false, true},
		{`{"enabled":true,"extra":1}`, true, true},
		{`{}`, false, false},
		{`{"enabled":"yes"}`, false, false},
		{`"on"`, false, false},
	} {
		enabled, ok := codexFeatureEnabled(json.RawMessage(tc.raw))
		if enabled != tc.enabled || ok != tc.ok {
			t.Errorf("%q: got (%v, %v), want (%v, %v)", tc.raw, enabled, ok, tc.enabled, tc.ok)
		}
	}
}

func piTestRuntime(t *testing.T) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("pi integration tests require Node", err)
	}
	root, err := filepath.Abs("../../../agent")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "build.mjs")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pi worker: %v\n%s", err, out)
	}
	t.Setenv("NEOREAD_NODE", node)
	t.Setenv("NEOREAD_AGENT", filepath.Join(root, "dist/main.mjs"))
}
func createSessionTest(t *testing.T, s *Server) ChatSession {
	t.Helper()
	out := request(t, s, "POST", "/api/documents/doc/chat-sessions", strings.NewReader(`{"provider":"api"}`))
	if out.Code != 201 {
		t.Fatal(out.Code, out.Body.String())
	}
	var session ChatSession
	if err := json.Unmarshal(out.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	return session
}
func startSessionTest(t *testing.T, s *Server, session ChatSession, question string) ChatRun {
	t.Helper()
	out := request(t, s, "POST", "/api/documents/doc/chat-sessions/"+session.ID+"/chat", strings.NewReader(jsonText(map[string]any{"provider": "api", "prompt": question, "context": "", "userMessageId": id()})))
	if out.Code != 202 {
		t.Fatal(out.Code, out.Body.String())
	}
	var run ChatRun
	if err := json.Unmarshal(out.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	return run
}
func awaitRunTest(t *testing.T, s *Server, run ChatRun) chatRunRecord {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		record, err := s.Store.readChatRun(run.SessionID, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status != "running" {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not settle")
	return chatRunRecord{}
}
func TestChatSessionMigrationPreservesEvidence(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	original := Message{ID: "old-user", DocumentID: "doc", Role: "user", Content: "history", ReadingLocation: json.RawMessage(`{"type":"pdf","page":2}`), References: json.RawMessage(`[{"sourceId":"S1","contentHash":"same"}]`)}
	if err := s.Store.saveMessage(original); err != nil {
		t.Fatal(err)
	}
	if err := saveDocumentConversation(t.Context(), s.Store.DB, "doc", "codex", documentConversation{ID: "state", ConnectionKey: "key", NativeThread: "native"}); err != nil {
		t.Fatal(err)
	}
	out := request(t, s, "GET", "/api/documents/doc/chat-sessions", nil)
	if out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	var sessions []ChatSession
	_ = json.Unmarshal(out.Body.Bytes(), &sessions)
	if len(sessions) != 1 {
		t.Fatal(sessions)
	}
	messages, err := s.Store.chatHistory("doc", sessions[0].ID)
	if err != nil || len(messages) != 1 || messages[0].ID != original.ID || string(messages[0].References) != string(original.References) || string(messages[0].ReadingLocation) != string(original.ReadingLocation) {
		t.Fatal(messages, err)
	}
	state, err := s.Store.documentConversation(t.Context(), "doc", conversationProvider(sessions[0].ID, "codex"), "key")
	if err != nil || state.NativeThread != "native" {
		t.Fatal(state, err)
	}
	request(t, s, "GET", "/api/documents/doc/chat-sessions", nil)
	var count int
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM chat_sessions").Scan(&count)
	if count != 1 {
		t.Fatal("migration not idempotent", count)
	}
}
func TestPiSessionsConcurrentCancelAndDetachedSubscriber(t *testing.T) {
	piTestRuntime(t)
	s, _ := prepareConversationFixture(t)
	started := make(chan string, 4)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		text := string(raw)
		name := "ALPHA"
		if strings.Contains(text, "BETA") {
			name = "BETA"
		}
		if strings.Contains(text, "ALPHA") && strings.Contains(text, "BETA") {
			t.Error("session context mixed")
		}
		started <- name
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", jsonText(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": name + " partial"}}}}))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	configureDocumentAPI(t, s, upstream.URL)
	a, b := createSessionTest(t, s), createSessionTest(t, s)
	runA, runB := startSessionTest(t, s, a, "ALPHA"), startSessionTest(t, s, b, "BETA")
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("sessions did not run concurrently")
		}
	}
	conflict := request(t, s, "POST", "/api/documents/doc/chat-sessions/"+a.ID+"/chat", strings.NewReader(`{"provider":"api","prompt":"again","context":""}`))
	if conflict.Code != 409 {
		t.Fatal(conflict.Code, conflict.Body.String())
	}
	// Closing an SSE reader must not cancel its server-owned run.
	host := httptest.NewServer(s.Handler())
	defer host.Close()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", host.URL+"/api/documents/doc/chat-sessions/"+b.ID+"/runs/"+runB.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	response, err := host.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64)
	_, _ = response.Body.Read(chunk)
	cancel()
	response.Body.Close()
	stopped := request(t, s, "POST", "/api/documents/doc/chat-sessions/"+a.ID+"/runs/"+runA.ID+"/cancel", nil)
	if stopped.Code != 200 {
		t.Fatal(stopped.Code)
	}
	aResult := awaitRunTest(t, s, runA)
	if aResult.Status != "cancelled" {
		t.Fatal(aResult.Status, aResult.Error)
	}
	bBefore, err := s.Store.readChatRun(b.ID, runB.ID)
	if err != nil || bBefore.Status != "running" {
		t.Fatal("cancel A stopped B", bBefore, err)
	}
	close(release)
	bResult := awaitRunTest(t, s, runB)
	if bResult.Status != "completed" {
		t.Fatal(bResult.Status, bResult.Error, jsonText(bResult.Events))
	}
	ah, _ := s.Store.chatHistory("doc", a.ID)
	bh, _ := s.Store.chatHistory("doc", b.ID)
	if len(ah) != 1 || len(bh) != 2 || !strings.Contains(bh[1].Content, "BETA") || bh[1].SessionID != b.ID {
		t.Fatal(ah, bh)
	}
	replay := request(t, s, "GET", "/api/documents/doc/chat-sessions/"+b.ID+"/runs/"+runB.ID+"/events", nil)
	if !strings.Contains(replay.Body.String(), "event: done") || !strings.Contains(replay.Body.String(), "BETA partial") {
		t.Fatal(replay.Body.String())
	}
	var state documentConversation
	var raw string
	if err = s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE provider=?", conversationProvider(b.ID, "api")).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(raw), &state)
	if len(state.PiEntries) == 0 {
		t.Fatal("pi continuation was not committed")
	}
}
func TestPiSaveFailureKeepsAcceptedState(t *testing.T) {
	piTestRuntime(t)
	s, _ := prepareConversationFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		writeConversationReply(w, "remember CIVET-42", "")
	}))
	defer upstream.Close()
	configureDocumentAPI(t, s, upstream.URL)
	session := createSessionTest(t, s)
	first := awaitRunTest(t, s, startSessionTest(t, s, session, "CIVET-42"))
	if first.Status != "completed" {
		t.Fatal(first.Error)
	}
	var before, after string
	_ = s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE provider=?", conversationProvider(session.ID, "api")).Scan(&before)
	_, err := s.Store.DB.Exec(`CREATE TRIGGER fail_chat_save BEFORE INSERT ON messages WHEN json_extract(NEW.body,'$.role')='assistant' BEGIN SELECT RAISE(FAIL,'save blocked'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	failed := awaitRunTest(t, s, startSessionTest(t, s, session, "next"))
	if failed.Status != "failed" {
		t.Fatal(failed.Status)
	}
	_ = s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE provider=?", conversationProvider(session.ID, "api")).Scan(&after)
	if before != after {
		t.Fatal("unsaved turn changed accepted state")
	}
	messages, _ := s.Store.chatHistory("doc", session.ID)
	if len(messages) != 3 {
		t.Fatal("partial assistant committed", messages)
	}
	for _, e := range failed.Events {
		if e.Event == "done" {
			t.Fatal("reported persisted success on save failure")
		}
	}
}

func TestPiReadingToolsAndAcceptedContinuation(t *testing.T) {
	piTestRuntime(t)
	s, _ := prepareConversationFixture(t)
	requests := 0
	toolRead := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		var payload struct {
			Messages []toolCompletionMessage `json:"messages"`
			Tools    []json.RawMessage       `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("invalid payload")
			return
		}
		requests++
		if len(payload.Tools) != 3 {
			t.Errorf("unexpected tools: %s", jsonText(payload.Tools))
		}
		if requests == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"read-1","type":"function","function":{"name":"read_passage","arguments":"{\"full\":true}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
			return
		}
		for _, m := range payload.Messages {
			if m.Role == "tool" && strings.Contains(jsonText(m.Content), "COBALT-19") {
				toolRead = true
			}
		}
		if !toolRead {
			t.Error("host tool evidence missing")
		}
		writeConversationReply(w, "COBALT-19 [S2].", "private-continuation")
	}))
	defer upstream.Close()
	configureDocumentAPI(t, s, upstream.URL)
	session := createSessionTest(t, s)
	first := awaitRunTest(t, s, startSessionTest(t, s, session, "Read COBALT-19"))
	if first.Status != "completed" {
		t.Fatal(first.Error, jsonText(first.Events))
	}
	// A fresh worker restores accepted native entries, including the tool pair.
	second := awaitRunTest(t, s, startSessionTest(t, s, session, "Continue"))
	if second.Status != "completed" {
		t.Fatal(second.Error, jsonText(second.Events))
	}
	if requests != 3 || !toolRead {
		t.Fatal(requests, toolRead)
	}
	messages, _ := s.Store.chatHistory("doc", session.ID)
	if len(messages) != 4 || !strings.Contains(string(messages[1].References), "corpusRevision") || messages[1].Reasoning != "private-continuation" {
		t.Fatal(messages)
	}
	if len(messages[1].Tools) != 1 || messages[1].Tools[0].Target == "read_passage" {
		t.Fatalf("lost actual reading target: %s", jsonText(messages[1].Tools))
	}
}

func TestRunConcurrencyLimitAndRestartReconciliation(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	session := createSessionTest(t, s)
	s.chatMu.Lock()
	s.chatRuns = map[string]*chatRun{}
	for i := 0; i < 4; i++ {
		s.chatRuns[fmt.Sprint(i)] = &chatRun{}
	}
	s.chatMu.Unlock()
	out := request(t, s, "POST", "/api/documents/doc/chat-sessions/"+session.ID+"/chat", strings.NewReader(`{"provider":"codex","prompt":"fifth","context":""}`))
	if out.Code != 429 {
		t.Fatal(out.Code, out.Body.String())
	}
	messages, _ := s.Store.chatHistory("doc", session.ID)
	if len(messages) != 0 {
		t.Fatal("rejected run saved a question")
	}
	s.chatMu.Lock()
	s.chatRuns = nil
	s.chatMu.Unlock()
	for i, status := range []string{"interrupted", "completed"} {
		question := Message{ID: id(), DocumentID: "doc", SessionID: session.ID, Role: "user", Content: fmt.Sprint(i)}
		if err := s.Store.saveMessage(question); err != nil {
			t.Fatal(err)
		}
		run := ChatRun{ID: id(), SessionID: session.ID, DocumentID: "doc", UserMessageID: question.ID, Status: "running"}
		_, err := s.Store.DB.Exec("INSERT INTO chat_runs VALUES(?,?,?)", run.ID, session.ID, jsonText(run))
		if err != nil {
			t.Fatal(err)
		}
		if status == "completed" {
			if err = s.Store.saveMessage(Message{DocumentID: "doc", SessionID: session.ID, Role: "assistant", Content: "committed before exit"}); err != nil {
				t.Fatal(err)
			}
		}
		store, err := OpenStore(s.Store.Root)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := store.readChatRun(session.ID, run.ID)
		store.DB.Close()
		if err != nil || restored.Status != status {
			t.Fatal(restored, err)
		}
	}
}

func TestManagedCodexSessionsKeepSeparateNativeThreads(t *testing.T) {
	s, a := prepareConversationFixture(t)
	fakeAgent(t, "codex", "document-conversation")
	t.Setenv("NEOREAD_CONVERSATION_CAPTURE", filepath.Join(t.TempDir(), "capture.jsonl"))
	t.Setenv("NEOREAD_DOCUMENT_TEST_BLOCK", a.corpus.Passages[1].ID)
	first, second := createSessionTest(t, s), createSessionTest(t, s)
	start := func(session ChatSession, prompt string) ChatRun {
		out := request(t, s, "POST", "/api/documents/doc/chat-sessions/"+session.ID+"/chat", strings.NewReader(jsonText(map[string]any{"provider": "codex", "prompt": prompt, "context": ""})))
		if out.Code != 202 {
			t.Fatal(out.Code, out.Body.String())
		}
		var run ChatRun
		_ = json.Unmarshal(out.Body.Bytes(), &run)
		return run
	}
	ar, br := start(first, "记住 CIVET-42"), start(second, "独立问题")
	for _, run := range []ChatRun{ar, br} {
		if result := awaitRunTest(t, s, run); result.Status != "completed" {
			t.Fatal(result.Error)
		}
	}
	aHistory, _ := s.Store.chatHistory("doc", first.ID)
	bHistory, _ := s.Store.chatHistory("doc", second.ID)
	if len(aHistory) != 2 || len(bHistory) != 2 || !strings.Contains(aHistory[1].Content, "CIVET-42") || strings.Contains(bHistory[1].Content, "CIVET-42") {
		t.Fatal("native thread history mixed", aHistory, bHistory)
	}
	aState, err := s.Store.documentConversation(t.Context(), "doc", conversationProvider(first.ID, "codex"), "codex-native-v1")
	if err != nil {
		t.Fatal(err)
	}
	bState, err := s.Store.documentConversation(t.Context(), "doc", conversationProvider(second.ID, "codex"), "codex-native-v1")
	if err != nil || aState.NativeThread == bState.NativeThread {
		t.Fatal(aState, bState, err)
	}
	if result := awaitRunTest(t, s, start(first, "继续")); result.Status != "completed" {
		t.Fatal(result.Error)
	}
	aHistory, _ = s.Store.chatHistory("doc", first.ID)
	if len(aHistory) != 4 || !strings.Contains(aHistory[3].Content, "CIVET-42") {
		t.Fatal("native resume lost accepted history", aHistory)
	}
}

func createLibrarySessionTest(t *testing.T, s *Server, ids ...string) libraryChatSession {
	t.Helper()
	body := jsonText(map[string]any{"provider": "codex", "scope": libraryChatScope{Category: "研究", CategoryKey: "folder:研究", DocumentIDs: ids}})
	out := request(t, s, "POST", "/api/library/chat-sessions", strings.NewReader(body))
	if out.Code != 201 {
		t.Fatal(out.Code, out.Body.String())
	}
	var item libraryChatSession
	if err := json.Unmarshal(out.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestLibraryChatScopesSearchAndCitationsToSelectedPapers(t *testing.T) {
	s := testServer(t)
	for _, id := range []string{"alpha", "beta", "excluded"} {
		archiveDocument(t, s.Store, id, "研究")
		doc, _ := s.Store.Document(id)
		fingerprint, err := s.corpusFingerprint(doc)
		if err != nil {
			t.Fatal(err)
		}
		text := id + " studies the cobalt protocol with a different threat model."
		corpus := documentCorpus{Revision: "rev-" + id, Coverage: corpusCoverage{Status: "complete", TotalUnits: 1, SearchableUnits: 1}, Passages: []documentPassage{{ID: "p1-b1", Section: "page-1", Label: id, Text: text, Location: json.RawMessage(`{"type":"pdf","page":1}`), Precision: "block"}}}
		if _, err = s.Store.DB.Exec("UPDATE document_corpora SET fingerprint=?,revision=?,body=? WHERE document_id=?", fingerprint, corpus.Revision, jsonText(corpus), id); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Store.DB.Exec("DELETE FROM document_passages_fts WHERE document_id=?", id); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Store.DB.Exec("INSERT INTO document_passages_fts VALUES(?,?,?,?)", id, corpus.Revision, "p1-b1", text); err != nil {
			t.Fatal(err)
		}
	}
	item := createLibrarySessionTest(t, s, "alpha", "beta")
	corpus, finish, err := s.libraryCorpus(t.Context(), item.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if len(corpus.Library.Sources) != 2 || len(corpus.Passages) != 2 {
		t.Fatal("paper scope was not preserved", corpus)
	}
	a := &documentAgent{server: s, corpus: corpus, ctx: t.Context(), sessionID: item.ID, ID: id(), chat: &chatInput{SessionID: item.ID}}
	if err = a.prepareReading(t.Context(), AIInput{Prompt: "compare"}); err != nil {
		t.Fatal(err)
	}
	if blocked, e := a.call(t.Context(), "read_passage", json.RawMessage(`{"documentId":"excluded","full":true}`)); e != nil || !strings.Contains(string(blocked), "invalid_arguments") || len(a.Evidence) != 0 {
		t.Fatal("reading escaped the selected scope", string(blocked), e)
	}
	found, err := a.call(t.Context(), "search_document", json.RawMessage(`{"query":"cobalt","limit":12}`))
	if err != nil || strings.Contains(string(found), "excluded") || !strings.Contains(string(found), "alpha:p1-b1") || !strings.Contains(string(found), "beta:p1-b1") {
		t.Fatal("search escaped or missed the selected scope", string(found), err)
	}
	if _, err = a.call(t.Context(), "read_passage", json.RawMessage(`{"documentId":"beta","full":true}`)); err != nil {
		t.Fatal(err)
	}
	if len(a.Evidence) != 1 || a.Evidence[0].DocumentID != "beta" {
		t.Fatal("read mixed documents", a.Evidence)
	}
	if err = s.saveDocumentAgentAnswer(t.Context(), a, "Beta supplies this evidence [S2].", "codex"); err != nil {
		t.Fatal(err)
	}
	history, err := s.Store.chatHistory(libraryChatOwner, item.ID)
	if err != nil || len(history) != 1 {
		t.Fatal(history, err)
	}
	out := request(t, s, "GET", "/api/library/chat-sessions/"+item.ID+"/messages/"+history[0].ID+"/sources/S2", nil)
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"documentId":"beta"`) {
		t.Fatal(out.Code, out.Body.String())
	}
	if _, err = s.Store.DB.Exec("UPDATE document_corpora SET revision='new' WHERE document_id='beta'"); err != nil {
		t.Fatal(err)
	}
	out = request(t, s, "GET", "/api/library/chat-sessions/"+item.ID+"/messages/"+history[0].ID+"/sources/S2", nil)
	if out.Code != 409 {
		t.Fatal("stale source remained navigable", out.Code, out.Body.String())
	}
}

func TestLibraryChatManagedCodexStreamsAndContinuesWithoutCreatingPaperRows(t *testing.T) {
	s, a := prepareConversationFixture(t)
	if _, err := s.Store.DB.Exec("UPDATE documents SET library='papers' WHERE id='doc'"); err != nil {
		t.Fatal(err)
	}
	fakeAgent(t, "codex", "document-conversation")
	t.Setenv("NEOREAD_CONVERSATION_CAPTURE", filepath.Join(t.TempDir(), "capture.jsonl"))
	t.Setenv("NEOREAD_DOCUMENT_TEST_BLOCK", "doc:"+a.corpus.Passages[1].ID)
	item := createLibrarySessionTest(t, s, "doc")
	for _, prompt := range []string{"记住 CIVET-42，分析分类中的论文", "请继续解释"} {
		out := request(t, s, "POST", "/api/library/chat-sessions/"+item.ID+"/chat", strings.NewReader(jsonText(map[string]any{"provider": "codex", "prompt": prompt, "context": "", "userMessageId": id()})))
		if out.Code != 202 {
			t.Fatal(out.Code, out.Body.String())
		}
		var run ChatRun
		if err := json.Unmarshal(out.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		result := awaitRunTest(t, s, run)
		if result.Status != "completed" {
			t.Fatal(result.Error)
		}
		if len(result.Events) < 2 || result.Events[len(result.Events)-1].Event != "done" {
			t.Fatal("stream never reached a saved terminal event", result)
		}
	}
	history, err := s.Store.chatHistory(libraryChatOwner, item.ID)
	if err != nil || len(history) != 4 || !strings.Contains(history[3].Content, "CIVET-42") {
		t.Fatal("library history was not continued", history, err)
	}
	var count int
	if err = s.Store.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count); err != nil || count != 1 {
		t.Fatal("chat created a synthetic library paper", count, err)
	}
	listed := request(t, s, "GET", "/api/library/chat-sessions", nil)
	var sessions []libraryChatSession
	if listed.Code != 200 || json.Unmarshal(listed.Body.Bytes(), &sessions) != nil || len(sessions) != 1 || sessions[0].Scope.CategoryKey != "folder:研究" || sessions[0].LatestRun == nil || sessions[0].LatestRun.Status != "completed" || strings.Contains(listed.Body.String(), `"events"`) {
		t.Fatal("session list lost its category or lightweight run summary", listed.Code, listed.Body.String())
	}
	out := request(t, s, "DELETE", "/api/library/chat-sessions/"+item.ID+"/messages/"+history[2].ID, strings.NewReader(jsonText(chatMessageChange{ExpectedLastMessageID: history[3].ID, ExpectedContent: history[2].Content})))
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	history, err = s.Store.chatHistory(libraryChatOwner, item.ID)
	if err != nil || len(history) != 2 {
		t.Fatal("withdrawal changed the wrong session", history, err)
	}
}
