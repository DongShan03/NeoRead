package neoread

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestChatKeepsLongSourceWithoutRepeatingIt(t *testing.T) {
	fakeAgent(t, "codex", "fail-before")
	s := testServer(t)
	if _, err := s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at) VALUES('doc','pdf','test','',1,?,?)", now(), now()); err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("中文长资料。", 1800) + "独有原文事实：维修费用471元。"
	for _, m := range []Message{{Role: "user", Content: "费用？", Context: source}, {Role: "assistant", Content: "471元"}, {Role: "user", Content: "再次核对", Context: source}, {Role: "assistant", Content: "471元"}} {
		m.DocumentID = "doc"
		if err := s.Store.saveMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	captured := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		captured = req.Messages[1].Content
		writeAPIReply(w, "471元")
	}))
	defer api.Close()
	c := AIConfig{Primary: "codex", Models: map[string]string{}, TextAPI: APIConnection{URL: api.URL, Model: "test"}, Capabilities: map[string]Capability{}}
	c.Capabilities["text-api"] = Capability{Text: true, Fingerprint: configPrint(c, "text-api")}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	r := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(`{"provider":"codex","prompt":"根据原始依据确认费用","context":""}`))
	if !strings.Contains(r.Body.String(), "event: done") {
		t.Fatal(r.Body.String())
	}
	if n := strings.Count(captured, "独有原文事实：维修费用471元。"); n != 1 {
		t.Errorf("original source fact occurs %d times, want once", n)
	}
	if strings.Contains(captured, "�") {
		t.Error("history damaged UTF-8")
	}
	if !strings.Contains(captured, "再次核对") {
		t.Error("source budget dropped the user question")
	}
}

func TestChatHistoryKeepsQuestionAnswerBoundaries(t *testing.T) {
	source := strings.Repeat("完整中文证据。", 6000)
	messages := []Message{{Role: "assistant", Content: "不应出现的孤立回答"}, {Role: "user", Content: "需要保留的问题", Context: source}, {Role: "assistant", Content: strings.Repeat("很长的中文回答。", 6000)}}
	chat := &chatInput{DocumentID: "doc", Title: "title", Question: "本轮问题", CurrentText: source, History: messages}
	prompt := chat.legacyPrompt()
	if strings.Contains(prompt, "不应出现的孤立回答") {
		t.Fatal("included an orphan answer")
	}
	if !strings.Contains(prompt, "需要保留的问题") || strings.Contains(prompt, "历史回答已截短") {
		t.Fatal("history was truncated or lost its paired question")
	}
	if len(chat.Sources) != 1 || chat.Sources[0].Text != source {
		t.Fatal("source was truncated or duplicated")
	}
	if strings.Contains(prompt, "�") {
		t.Fatal("damaged UTF-8")
	}
}

func TestChatHistorySourcesStayWithinTotalBudget(t *testing.T) {
	old := strings.Repeat("旧", 150000/3)
	recent := strings.Repeat("近", 150000/3)
	current := strings.Repeat("今", 150000/3)
	messages := []Message{
		{Role: "user", Content: "第一问", Context: old}, {Role: "assistant", Content: "答一"},
		{Role: "user", Content: "第二问", Context: recent}, {Role: "assistant", Content: "答二"},
	}
	chat := &chatInput{DocumentID: "doc", Title: "title", Question: "第三问", CurrentText: current, History: messages}
	prompt := chat.legacyPrompt()
	total := 0
	for _, source := range chat.Sources {
		total += len(source.Text)
		if source.Text == old {
			t.Fatal("kept the oldest source past the budget")
		}
	}
	if total > chatSourceBytes {
		t.Fatalf("sources total %d bytes", total)
	}
	if len(chat.Sources) != 2 || chat.Sources[1].Text != current {
		t.Fatal("dropped the current or most recent source")
	}
	if !strings.Contains(prompt, `"text":"第一问","sourceOmitted":true`) {
		t.Fatal("omitted source is not marked")
	}
}

func TestChatAddsDocumentSummaryWithoutImplicitExcerpt(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	r := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"metadata":{"abstract":"唯一摘要：讨论模型评估。"}}`))
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	upstream, calls := fakeAPI(t, func(int, apiCall) string { return "通用回答" })
	c := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{"api": "plain"}}
	passModels(&c, map[string]bool{"plain": false})
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	r = request(t, s, "POST", "/api/documents/"+d.ID+"/chat", strings.NewReader(`{"provider":"api","prompt":"介绍一下Go的接口","context":""}`))
	if !strings.Contains(r.Body.String(), "event: done") {
		t.Fatal(r.Body)
	}
	prompt := calls()[0].Messages[1].Content.(string)
	if !strings.Contains(prompt, "唯一摘要：讨论模型评估。") || !strings.Contains(prompt, "介绍一下Go的接口") {
		t.Fatal(prompt)
	}
	if !strings.Contains(prompt, "Current source: \n") {
		t.Fatal("summary was treated as a current excerpt", prompt)
	}
	if strings.Contains(calls()[0].Messages[0].Content.(string), "Answer only from supplied content") {
		t.Fatal("general questions remain forbidden")
	}
	messages, err := s.Store.chatHistory(d.ID)
	if err != nil || len(messages) != 2 {
		t.Fatalf("%+v %v", messages, err)
	}
	if messages[0].Context != "" || len(messages[0].References) != 0 {
		t.Fatalf("invented a quotation: %+v", messages[0])
	}
}

func TestChatSummaryIsSeparateFromExplicitSources(t *testing.T) {
	source := "手动引用的完整原文"
	chat := &chatInput{DocumentID: "doc", Title: "文章", Summary: "已有摘要", Question: "继续解释", CurrentText: source, History: []Message{{Role: "user", Content: "上一问", Context: source}}}
	prompt := chat.legacyPrompt()
	if strings.Count(prompt, source) != 1 || len(chat.Sources) != 2 || chat.CurrentSource == chat.SummarySource {
		t.Fatalf("sources not deduplicated/separated: %s", prompt)
	}
	next := chat.incremental(map[string]bool{chat.SummarySource: true})
	if strings.Contains(next, "已有摘要") || !strings.Contains(next, "Document summary: "+chat.SummarySource) {
		t.Fatal(next)
	}
	empty := &chatInput{DocumentID: "doc", Title: "文章", Question: "一般问题"}
	_ = empty.legacyPrompt()
	if len(empty.Sources) != 0 || empty.CurrentSource != "" || empty.SummarySource != "" {
		t.Fatalf("invented sources: %+v", empty)
	}
}

func TestEveryChatKindIncludesTitleAndSummary(t *testing.T) {
	for _, kind := range []string{"ordinary", "selection", "image"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := imageFixture(t)
			r := request(t, s, "PATCH", "/api/documents/doc", strings.NewReader(`{"title":"本次文章标题","metadata":{"abstract":"本次文章摘要"}}`))
			if r.Code != 200 {
				t.Fatal(r.Body)
			}
			upstream, calls := fakeAPI(t, func(int, apiCall) string { return "回答" })
			c := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{"api": "plain"}, VisionModels: map[string]string{"api": "plain"}}
			passModels(&c, map[string]bool{"plain": true})
			if err := s.writeAIConfig(c); err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"provider": "api", "prompt": "问题", "context": ""}
			if kind == "selection" {
				input["context"] = "选区及完整辅助章节"
			}
			if kind == "image" {
				input["attachments"] = []string{"p1-b1"}
			}
			r = request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(jsonText(input)))
			if !strings.Contains(r.Body.String(), "event: done") {
				t.Fatal(r.Body)
			}
			payload := jsonText(calls()[0].Messages[1].Content)
			if !strings.Contains(payload, "本次文章标题") || !strings.Contains(payload, "本次文章摘要") {
				t.Fatal(payload)
			}
		})
	}
}

func TestChatAcceptsConfirmedLongContextUpToHardCap(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	upstream, calls := fakeAPI(t, func(int, apiCall) string { return "回答" })
	c := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{"api": "plain"}}
	passModels(&c, map[string]bool{"plain": false})
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	body := func(context string) *strings.Reader {
		b, _ := json.Marshal(map[string]string{"provider": "api", "prompt": "总结", "context": context})
		return strings.NewReader(string(b))
	}
	// About 100 KB, above the old 64 KB limit.
	long := strings.Repeat("证", 100000/3)
	r := request(t, s, "POST", "/api/documents/"+d.ID+"/chat", body(long))
	if !strings.Contains(r.Body.String(), "event: done") {
		t.Fatal(r.Body)
	}
	if !strings.Contains(jsonText(calls()[0].Messages[1].Content), long) {
		t.Fatal("long context was not sent whole")
	}
	r = request(t, s, "POST", "/api/documents/"+d.ID+"/chat", body(strings.Repeat("证", chatContextBytes/3+1)))
	if r.Code != 400 {
		t.Fatal(r.Code, r.Body)
	}
}

func TestRetiredChapterPreferenceDoesNotChangeSavedConnections(t *testing.T) {
	s := testServer(t)
	file := filepath.Join(s.Store.Root, "ai-connections.json")
	raw := `{"config":{"primary":"api","models":{"api":"chosen-model"},"api":{"url":"https://example.test/v1","key":"fixture-key"},"chatIncludeChapter":true}}`
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	c := s.aiConfig()
	if c.Primary != "api" || c.Models["api"] != "chosen-model" || c.API.Key != "fixture-key" {
		t.Fatal("retired field discarded configured connection")
	}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(saved), "chatIncludeChapter") {
		t.Fatal("retired field persisted", err)
	}
	if c = s.aiConfig(); c.API.Key != "fixture-key" || c.Models["api"] != "chosen-model" {
		t.Fatal("connection changed on save")
	}
}

func chatEditFixture(t *testing.T) (*Server, ChatSession, ChatSession, []Message) {
	t.Helper()
	s, _ := prepareConversationFixture(t)
	a, b := createSessionTest(t, s), createSessionTest(t, s)
	var history []Message
	for i, key := range []string{"first", "middle", "last"} {
		user := Message{ID: key + "-user", DocumentID: "doc", SessionID: a.ID, Role: "user", Content: key, CreatedAt: "2021-02-03T04:05:06Z", Context: "original context", WebSearch: true,
			References: json.RawMessage(`[{"text":"原文 $H'$","location":{"type":"pdf","page":2},"kind":"selection"},{"kind":"chat","text":"之前回答的选中文字"}]`), ReadingLocation: json.RawMessage(`{"type":"pdf","page":2}`), Attachments: []ImageAttachment{{ID: "p2-b1", Page: 2, Label: "chart", Caption: "原图"}}}
		answer := Message{ID: key + "-answer", DocumentID: "doc", SessionID: a.ID, Role: "assistant", Content: "answer " + key, Reasoning: "thinking", CreatedAt: user.CreatedAt}
		for _, m := range []Message{user, answer} {
			if _, err := s.Store.DB.Exec("INSERT INTO messages VALUES(?,?,?,?)", m.ID, m.DocumentID, jsonText(m), m.CreatedAt); err != nil {
				t.Fatal(err)
			}
			history = append(history, m)
		}
		if i == 1 {
			for _, role := range []string{"user", "assistant"} {
				m := Message{ID: "other-" + role, DocumentID: "doc", SessionID: b.ID, Role: role, Content: "other session", CreatedAt: user.CreatedAt}
				if _, err := s.Store.DB.Exec("INSERT INTO messages VALUES(?,?,?,?)", m.ID, m.DocumentID, jsonText(m), m.CreatedAt); err != nil {
					t.Fatal(err)
				}
			}
		}
		run := chatRunRecord{ChatRun: ChatRun{ID: key + "-run", SessionID: a.ID, DocumentID: "doc", UserMessageID: user.ID, Status: "completed", Provider: "api", StartedAt: user.CreatedAt, FinishedAt: user.CreatedAt}, Events: []chatRunEvent{{Event: "done", Data: json.RawMessage(`{"ok":true}`)}}}
		if _, err := s.Store.DB.Exec("INSERT INTO chat_runs VALUES(?,?,?)", run.ID, a.ID, jsonText(run)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.DB.Exec("INSERT INTO chat_agent_runs VALUES(?,?,?)", key+"-evidence", "doc", jsonText(map[string]any{"messageId": answer.ID, "evidence": []string{"source"}})); err != nil {
			t.Fatal(err)
		}
	}
	for _, session := range []string{a.ID, b.ID, a.ID + "suffix"} {
		for _, provider := range []string{"api", "codex", "claude", "kimi"} {
			if err := saveDocumentConversation(t.Context(), s.Store.DB, "doc", conversationProvider(session, provider), documentConversation{ID: session + provider, ConnectionKey: "key", LastMessageID: "last-answer", NativeThread: "old-thread"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for key, answer := range map[string]string{"linked": "middle-answer", "other": "other-assistant"} {
		annotation := Annotation{ID: key, DocumentID: "doc", Kind: "question", Note: "keep my note", AnswerID: answer, Resolved: true, Quote: "keep my quote", Location: json.RawMessage(`{"type":"pdf","page":2}`)}
		if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES(?,?,?)", key, "doc", jsonText(annotation)); err != nil {
			t.Fatal(err)
		}
	}
	return s, a, b, history
}

func changeQuestionTest(t *testing.T, s *Server, session, message, method string, change chatMessageChange) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, s, method, "/api/documents/doc/chat-sessions/"+session+"/messages/"+message, strings.NewReader(jsonText(change)))
}

func TestEditAndWithdrawChatQuestionPreservesContextAndOtherSessions(t *testing.T) {
	s, a, b, before := chatEditFixture(t)
	otherBefore, _ := s.Store.chatHistory("doc", b.ID)
	content := "编辑后的问题 $H'$"
	out := changeQuestionTest(t, s, a.ID, "middle-user", "PATCH", chatMessageChange{ExpectedLastMessageID: "last-answer", ExpectedContent: "middle", Content: &content})
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	var result chatMessageChangeResult
	if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	expected := before[2]
	expected.Content = content
	if !reflect.DeepEqual(result.Message, expected) || !reflect.DeepEqual(result.Messages, []Message{before[0], before[1], expected}) {
		t.Fatalf("edited question lost source context or original date: %+v", result)
	}
	other, _ := s.Store.chatHistory("doc", b.ID)
	if !reflect.DeepEqual(otherBefore, other) {
		t.Fatal("another session changed", other)
	}
	for table, want := range map[string]int{"chat_runs": 1, "chat_agent_runs": 1, "document_conversations": 8} {
		var count int
		if err := s.Store.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s: %d want %d: %v", table, count, want, err)
		}
	}
	var raw string
	if err := s.Store.DB.QueryRow("SELECT body FROM annotations WHERE id='linked'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var mark Annotation
	_ = json.Unmarshal([]byte(raw), &mark)
	if mark.AnswerID != "" || mark.Resolved || mark.Note != "keep my note" || mark.Quote != "keep my quote" {
		t.Fatal("annotation content lost or stale answer retained", mark)
	}
	_ = s.Store.DB.QueryRow("SELECT body FROM annotations WHERE id='other'").Scan(&raw)
	_ = json.Unmarshal([]byte(raw), &mark)
	if mark.AnswerID != "other-assistant" || !mark.Resolved {
		t.Fatal("another session's annotation changed", mark)
	}
	state, err := s.Store.documentConversation(t.Context(), "doc", conversationProvider(a.ID, "codex"), "key")
	if err != nil || state.NativeThread != "" || state.LastMessageID != "" {
		t.Fatal("old provider continuation survived edit", state, err)
	}
	withdrawn := changeQuestionTest(t, s, a.ID, "middle-user", "DELETE", chatMessageChange{ExpectedLastMessageID: "middle-user", ExpectedContent: content})
	if withdrawn.Code != 200 {
		t.Fatal(withdrawn.Code, withdrawn.Body.String())
	}
	var returned chatMessageChangeResult
	if err := json.Unmarshal(withdrawn.Body.Bytes(), &returned); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(returned.Message, expected) || !reflect.DeepEqual(returned.Messages, before[:2]) {
		t.Fatal("withdraw lost question context or retained stale history", returned)
	}
	result.Messages = returned.Messages
	// Reloading the database retains the edited/withdrawn transcript.

	s.Store.DB.Close()
	reopened, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	got, err := reopened.chatHistory("doc", a.ID)
	if err != nil || !reflect.DeepEqual(result.Messages, got) {
		t.Fatal("edit did not persist", got, err)
	}
}

func TestChatChangeKeepsAnnotationUndoWithoutDeletedAnswerLinks(t *testing.T) {
	for _, method := range []string{"PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			s, session, _, _ := chatEditFixture(t)
			out := undoRequest(t, s, "notes", "PATCH", "/api/documents/doc/annotations/linked", map[string]any{"note": "changed note"})
			if out.Code != 200 {
				t.Fatal(out.Code, out.Body.String())
			}
			out = undoRequest(t, s, "notes", "DELETE", "/api/documents/doc/annotations/linked", nil)
			if out.Code != 204 {
				t.Fatal(out.Code, out.Body.String())
			}
			content := "edited"
			out = changeQuestionTest(t, s, session.ID, "middle-user", method, chatMessageChange{ExpectedLastMessageID: "last-answer", ExpectedContent: "middle", Content: &content})
			if out.Code != 200 {
				t.Fatal(out.Code, out.Body.String())
			}
			for _, note := range []string{"changed note", "keep my note"} {
				undoLast(t, s, "notes")
				var raw string
				if err := s.Store.DB.QueryRow("SELECT body FROM annotations WHERE id='linked'").Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var mark Annotation
				if json.Unmarshal([]byte(raw), &mark) != nil || mark.AnswerID != "" || mark.Resolved || mark.Note != note || mark.Quote != "keep my quote" {
					t.Fatal("undo restored a deleted answer or lost content", mark)
				}
			}
			var raw string
			_ = s.Store.DB.QueryRow("SELECT body FROM annotations WHERE id='other'").Scan(&raw)
			var other Annotation
			_ = json.Unmarshal([]byte(raw), &other)
			if other.AnswerID != "other-assistant" || !other.Resolved {
				t.Fatal("unrelated answer link changed", other)
			}
		})
	}
}

func TestChatQuestionChangeRollsBackTranscriptAndSourcesOnSaveFailure(t *testing.T) {
	s, a, _, before := chatEditFixture(t)
	if _, err := s.Store.DB.Exec("CREATE TRIGGER reject_chat_delete BEFORE DELETE ON messages BEGIN SELECT RAISE(ABORT,'test save failure'); END"); err != nil {
		t.Fatal(err)
	}
	content := "new question"
	out := changeQuestionTest(t, s, a.ID, "middle-user", "PATCH", chatMessageChange{ExpectedLastMessageID: "last-answer", ExpectedContent: "middle", Content: &content})
	if out.Code != 500 {
		t.Fatal(out.Code, out.Body.String())
	}
	got, _ := s.Store.chatHistory("doc", a.ID)
	if !reflect.DeepEqual(before, got) {
		t.Fatal("failed transaction changed messages")
	}
	var count int
	for table, want := range map[string]int{"chat_runs": 3, "chat_agent_runs": 3, "document_conversations": 12} {
		if err := s.Store.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatal("failed transaction lost records", table, count, err)
		}
	}
	var raw string
	_ = s.Store.DB.QueryRow("SELECT body FROM annotations WHERE id='linked'").Scan(&raw)
	if !strings.Contains(raw, `"answerId":"middle-answer"`) {
		t.Fatal("failed transaction changed annotations", raw)
	}
}

func TestChatWithdrawalWaitsForAnnotationSnapshot(t *testing.T) {
	s, session, _, _ := chatEditFixture(t)
	s.annotationMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.annotationMu.Unlock()
		}
	}()
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		finished <- changeQuestionTest(t, s, session.ID, "middle-user", "DELETE", chatMessageChange{
			ExpectedLastMessageID: "last-answer", ExpectedContent: "middle",
		})
	}()
	// A concurrent undo snapshot owns this lock. The chat transaction must
	// leave answer links untouched until that snapshot has finished.
	deadline := time.After(100 * time.Millisecond)
	for waiting := true; waiting; {
		var body string
		if err := s.Store.DB.QueryRow("SELECT body FROM annotations WHERE id='linked'").Scan(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body, `"answerId":"middle-answer"`) {
			t.Fatal("chat transaction entered an annotation snapshot")
		}
		select {
		case <-finished:
			t.Fatal("withdrawal completed while annotation snapshot was locked")
		case <-deadline:
			waiting = false
		case <-time.After(time.Millisecond):
		}
	}
	s.annotationMu.Unlock()
	locked = false
	select {
	case out := <-finished:
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("withdrawal did not resume")
	}
}

func TestChatReadingLocationValidation(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	for _, location := range []string{
		`{"type":"pdf","page":1}`, `{"type":"epub","href":"../outside"}`,
		`{"type":"epub","href":"a.xhtml","progression":2}`, `null`,
		`{"type":"epub","href":"a.xhtml","locator":"invalid"}`,
		`{"type":"epub","href":"a.xhtml","extra":"` + strings.Repeat("x", 16384) + `"}`,
	} {
		out := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(`{"provider":"api","prompt":"这里是什么","context":"","readingLocation":`+location+`}`))
		if out.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid reading location: %d", out.Code)
		}
	}
	history, _ := s.Store.chatHistory("doc")
	if len(history) != 0 {
		t.Fatal("invalid position saved a message")
	}
}

func TestDocumentReadingLocationIsNavigationUntilToolRead(t *testing.T) {
	_, a := prepareConversationFixture(t)
	position := json.RawMessage(`{"type":"epub","href":"b.xhtml#end","progression":0.7,"locator":"{}"}`)
	in := AIInput{Chat: &chatInput{Title: "test", Question: "这里的实验是什么", ReadingLocation: position}}
	a.provider = "api"
	if _, err := a.beginConversation(t.Context(), in, "test"); err != nil {
		t.Fatal(err)
	}
	prompt := a.conversationInput(in, true)
	if !strings.Contains(prompt, `"sectionId":"b.xhtml"`) || strings.Contains(prompt, "37 transactions") || strings.Contains(prompt, "COBALT-19") || len(a.Evidence) != 0 {
		t.Fatal("navigation attached original text or counted as evidence", prompt)
	}
	if _, err := a.call(t.Context(), "read_passage", readingLocationSelector(position)); err != nil {
		t.Fatal(err)
	}
	if len(a.Evidence) == 0 || !strings.Contains(jsonText(a.Evidence), "37 transactions") {
		t.Fatal("could not read the captured chapter")
	}
	for _, e := range a.Evidence {
		if !strings.Contains(string(e.Location), "b.xhtml") {
			t.Fatal("read another chapter")
		}
	}
	if string(readingLocationSelector(json.RawMessage(`{"type":"pdf","page":13}`))) != `{"fromPage":13,"toPage":13}` {
		t.Fatal("PDF snapshot did not map to the exact page")
	}
}

func TestAPIReadingPositionPersistsAndKeepsToollessFallback(t *testing.T) {
	for _, tools := range []bool{true, false} {
		t.Run(map[bool]string{true: "agent", false: "text-only"}[tools], func(t *testing.T) {
			s, _ := prepareConversationFixture(t)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
					return
				}
				var payload struct {
					Messages []toolCompletionMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				calls++
				text := jsonText(payload.Messages)
				if !strings.Contains(text, "导航信息") || !strings.Contains(text, `b.xhtml`) {
					t.Error("sending position missing")
				}
				if strings.Contains(text, "37 transactions") == tools {
					t.Error("agent auto-attached source or text-only fallback lost current chapter")
				}
				writeConversationReply(w, "已收到问题", "")
			}))
			defer upstream.Close()
			configureDocumentAPI(t, s, upstream.URL)
			if !tools {
				config := s.aiConfig()
				capability := config.Capabilities["api:test"]
				capability.Tools = "unsupported"
				config.Capabilities["api:test"] = capability
				if err := s.writeAIConfig(config); err != nil {
					t.Fatal(err)
				}
			}
			out := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(`{"provider":"api","prompt":"这里讲了什么","context":"","readingLocation":{"type":"epub","href":"b.xhtml#end","progression":0.7}}`))
			if strings.Count(out.Body.String(), "event: done") != 1 || calls != 1 {
				t.Fatal("request failed", out.Body.String(), calls)
			}
			history, err := s.Store.chatHistory("doc")
			if err != nil || len(history) != 2 || history[0].Context != "" || len(history[0].References) > 0 || !strings.Contains(string(history[0].ReadingLocation), "b.xhtml#end") {
				t.Fatal("position was not stored independently", history, err)
			}
			mirror := conversationHistory(history)
			if !strings.Contains(jsonText(mirror), "b.xhtml#end") {
				t.Fatal("provider handoff lost previous question's position")
			}
		})
	}
}

func chatMessages(t *testing.T, s *Server) []Message {
	t.Helper()
	res := request(t, s, "GET", "/api/documents/doc/messages", nil)
	var messages []Message
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &messages) != nil {
		t.Fatal("cannot read chat history", res.Body.String())
	}
	return messages
}

func TestChatRegenerationReusesQuestionWithCurrentModel(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	models := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		var payload struct {
			Model    string                  `json:"model"`
			Messages []toolCompletionMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		models = append(models, payload.Model)
		if len(models) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"unfinished answer\"}}]}\n\n")
			return
		}
		if strings.Count(jsonText(payload.Messages), "RETRY-QUESTION") != 1 || strings.Contains(jsonText(payload.Messages), "unfinished answer") {
			t.Error("retry duplicated the question or retained failed output", jsonText(payload.Messages))
		}
		writeConversationReply(w, "complete answer", "")
	}))
	defer upstream.Close()
	configureDocumentAPI(t, s, upstream.URL)
	body := `{"provider":"api","prompt":"RETRY-QUESTION","context":"original source","references":[],"readingLocation":{"type":"epub","href":"a.xhtml","progression":0},"userMessageId":"11111111111111111111111111111111"}`
	first := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(body))
	if !strings.Contains(first.Body.String(), "event: error") {
		t.Fatal("expected interrupted generation", first.Body.String())
	}
	before := chatMessages(t, s)
	if len(before) != 1 || before[0].Role != "user" {
		t.Fatal("failed answer persisted", before)
	}
	config := s.aiConfig()
	config.Models["api"] = "replacement-model"
	config.APIModels = append(config.APIModels, "replacement-model")
	passModels(&config, map[string]bool{"replacement-model": false})
	cap := config.Capabilities["api:replacement-model"]
	cap.Tools, cap.ToolSchemaVersion = "passed", documentToolSchemaVersion
	config.Capabilities["api:replacement-model"] = cap
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	second := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(body))
	if !strings.Contains(second.Body.String(), "event: done") {
		t.Fatal("retry failed", second.Body.String())
	}
	after := chatMessages(t, s)
	if len(after) != 2 || after[0].ID != before[0].ID || after[0].CreatedAt != before[0].CreatedAt || after[1].Content != "complete answer" {
		t.Fatal("retry must replace failed attempt without duplicating question", after)
	}
	if len(models) != 2 || models[1] != "replacement-model" {
		t.Fatal("retry used stale model", models)
	}
	duplicate := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(body))
	if duplicate.Code != 409 || len(models) != 2 || len(chatMessages(t, s)) != 2 {
		t.Fatal("saved turn was regenerated", duplicate.Code, duplicate.Body.String())
	}
}

func TestChatRetryRejectsChangedOrNonTailQuestion(t *testing.T) {
	for _, kind := range []string{"prompt", "context", "references", "location", "answered", "later-question", "assistant-id", "foreign-id", "busy", "invalid-id"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := prepareConversationFixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("rejected retry reached provider")
				w.WriteHeader(500)
			}))
			defer upstream.Close()
			configureDocumentAPI(t, s, upstream.URL)
			uid := "22222222222222222222222222222222"
			m := Message{ID: uid, DocumentID: "doc", Role: "user", Content: "original", Context: "source", References: json.RawMessage(`[{"text":"quote"}]`), ReadingLocation: json.RawMessage(`{"type":"epub","href":"a.xhtml","progression":0}`)}
			if kind == "assistant-id" {
				m.Role = "assistant"
			}
			if kind == "foreign-id" {
				if _, err := s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at) VALUES('other','epub','other','',0,?,?)", now(), now()); err != nil {
					t.Fatal(err)
				}
				m.DocumentID = "other"
			}
			if err := s.Store.saveMessage(m); err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"provider": "api", "prompt": m.Content, "context": m.Context, "references": m.References, "readingLocation": m.ReadingLocation, "userMessageId": uid}
			wantStatus := 409
			switch kind {
			case "prompt":
				body["prompt"] = "different"
			case "context":
				body["context"] = "different"
			case "references":
				body["references"] = []any{}
			case "location":
				body["readingLocation"] = map[string]any{"type": "epub", "href": "b.xhtml"}
			case "answered", "later-question":
				role := "assistant"
				if kind == "later-question" {
					role = "user"
				}
				if err := s.Store.saveMessage(Message{DocumentID: "doc", Role: role, Content: "later"}); err != nil {
					t.Fatal(err)
				}
			case "busy":
				s.aiMu.Lock()
				defer s.aiMu.Unlock()
			case "invalid-id":
				body["userMessageId"] = "../invalid"
				wantStatus = 400
			}
			before := jsonText(chatMessages(t, s))
			out := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(jsonText(body)))
			if out.Code != wantStatus {
				t.Fatal("unexpected retry status", out.Code, out.Body.String())
			}
			if after := jsonText(chatMessages(t, s)); after != before {
				t.Fatal("rejected retry changed saved history", after)
			}
		})
	}
}

func TestChatRetryKeepsImagesAndUsesNewProvider(t *testing.T) {
	s, pixels := imageFixture(t)
	fakeAgent(t, "codex", "fail-after")
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "vision"}}})
			return
		}
		calls++
		var payload struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(string(payload.Messages[len(payload.Messages)-1].Content), imageData(pixels)) {
			t.Error("retry lost original image")
		}
		if len(payload.Messages) != 2 {
			t.Error("failed question repeated in history", len(payload.Messages))
		}
		writeConversationReply(w, "image answer", "")
	}))
	defer upstream.Close()
	config := AIConfig{Primary: "codex", API: APIConnection{URL: upstream.URL}, Models: map[string]string{"api": "vision"}, APIModels: []string{"vision"}, Capabilities: map[string]Capability{}}
	config.Capabilities["codex"] = Capability{Text: true, Vision: true, Fingerprint: configPrint(config, "codex")}
	passModels(&config, map[string]bool{"vision": true})
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"provider": "codex", "prompt": "explain image", "context": "selected source", "attachments": []string{"p1-b1"}, "references": []any{map[string]string{"text": "selected source"}}, "readingLocation": map[string]any{"type": "pdf", "page": 1}, "userMessageId": "33333333333333333333333333333333"}
	first := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(jsonText(body)))
	if !strings.Contains(first.Body.String(), "event: error") {
		t.Fatal(first.Body.String())
	}
	before := chatMessages(t, s)
	body["provider"] = "api"
	second := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(jsonText(body)))
	if !strings.Contains(second.Body.String(), "event: done") {
		t.Fatal(second.Body.String())
	}
	after := chatMessages(t, s)
	if calls != 1 || len(after) != 2 || jsonText(before[0]) != jsonText(after[0]) || after[1].Content != "image answer" {
		t.Fatal("image retry changed question or left duplicates", after)
	}
}
