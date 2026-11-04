package neoread

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const handoffTestThread = "01900000-0000-7000-8000-000000000001"
const handoffTestFork = "01900000-0000-7000-8000-000000000002"

func handoffFixture(t *testing.T) *Server {
	t.Helper()
	s, _ := originalDocumentFixture(t)
	d, _ := s.Store.Document("doc")
	if err := os.WriteFile(s.Store.File(d), []byte("original EPUB bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Message{{ID: "question", DocumentID: "doc", Role: "user", Content: "继续解释 COBALT-19"}, {ID: "answer", DocumentID: "doc", Role: "assistant", Content: "之前的回答"}} {
		if err := s.Store.saveMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	c := documentConversation{ID: "neoread-conversation", ConnectionKey: "codex-native-v1", NativeThread: handoffTestThread, NativeCommitted: "committed-turn", LastMessageID: "answer"}
	if err := saveDocumentConversation(t.Context(), s.Store.DB, "doc", "codex", c); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCodexHandoffCopiesCommittedContextWithoutMutatingReader(t *testing.T) {
	fakeAgent(t, "codex", "codex-handoff")
	capture := filepath.Join(t.TempDir(), "rpc.jsonl")
	t.Setenv("NEOREAD_HANDOFF_CAPTURE", capture)
	s := handoffFixture(t)
	before, _ := s.Store.chatHistory("doc")
	w := request(t, s, "POST", "/api/documents/doc/codex-handoff", nil)
	if w.Code != 200 {
		t.Fatalf("handoff: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		ThreadID  string `json:"threadId"`
		URL       string `json:"url"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ThreadID != handoffTestFork || result.URL != "codex://threads/"+handoffTestFork {
		t.Fatalf("bad result: %+v", result)
	}
	for file, want := range map[string]string{"source.epub": "original EPUB bytes", "document.json": "COBALT-19", "conversation.json": "之前的回答", "READING_CONTEXT.md": "单向"} {
		data, err := os.ReadFile(filepath.Join(result.Directory, file))
		if err != nil || !strings.Contains(string(data), want) {
			t.Fatalf("%s: %s %v", file, data, err)
		}
	}
	data, _ := os.ReadFile(capture)
	if strings.Contains(string(data), `"method":"turn/start"`) || strings.Contains(string(data), `"method":"thread/resume"`) {
		t.Fatal("handoff must not generate or resume the original")
	}
	var fork map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rpc struct {
			Method string
			Params map[string]any
		}
		_ = json.Unmarshal([]byte(line), &rpc)
		if rpc.Method == "thread/fork" {
			fork = rpc.Params
		}
	}
	if fork["threadId"] != handoffTestThread || fork["lastTurnId"] != "committed-turn" || fork["cwd"] != result.Directory || fork["ephemeral"] != false {
		t.Fatalf("fork: %+v", fork)
	}
	if !strings.Contains(fork["baseInstructions"].(string), "READING_CONTEXT.md") {
		t.Fatal("NeoRead tool instructions survived")
	}
	w = request(t, s, "POST", "/api/documents/doc/codex-handoff", nil)
	if w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	data, _ = os.ReadFile(capture)
	if strings.Count(string(data), `"method":"thread/fork"`) != 1 {
		t.Fatal("retry forked a duplicate")
	}
	if strings.Count(string(data), `"method":"thread/name/set"`) != 1 {
		t.Fatal("reopening overwrote the Codex title")
	}
	after, _ := s.Store.chatHistory("doc")
	c, _ := s.Store.documentConversation(t.Context(), "doc", "codex", "codex-native-v1")
	if jsonText(before) != jsonText(after) || c.NativeThread != handoffTestThread || c.NativeCommitted != "committed-turn" {
		t.Fatal("modified NeoRead conversation")
	}
}

func TestCodexHandoffRetryAfterNamingFailureKeepsForkAndSnapshot(t *testing.T) {
	fakeAgent(t, "codex", "codex-handoff")
	capture := filepath.Join(t.TempDir(), "rpc.jsonl")
	t.Setenv("NEOREAD_HANDOFF_CAPTURE", capture)
	t.Setenv("NEOREAD_HANDOFF_FAIL_NAME", "1")
	s := handoffFixture(t)
	w := request(t, s, "POST", "/api/documents/doc/codex-handoff", nil)
	if w.Code != 502 {
		t.Fatalf("failure: %d %s", w.Code, w.Body.String())
	}
	t.Setenv("NEOREAD_HANDOFF_FAIL_NAME", "")
	w = request(t, s, "POST", "/api/documents/doc/codex-handoff", nil)
	if w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	data, _ := os.ReadFile(capture)
	if strings.Count(string(data), `"method":"thread/fork"`) != 1 {
		t.Fatal("naming failure lost the fork")
	}
	var result codexHandoff
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	// An exported source is an independent file, not a link into the library.
	d, _ := s.Store.Document("doc")
	_ = os.Remove(s.Store.File(d))
	data, err := os.ReadFile(filepath.Join(result.Directory, "source.epub"))
	if err != nil || string(data) != "original EPUB bytes" {
		t.Fatalf("snapshot lost with original: %v", err)
	}
}

func TestCodexHandoffCanceledCopyDoesNotPublishPartialSource(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := copyHandoffSource(ctx, source, target); err == nil {
		t.Fatal("canceled copy succeeded")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("published incomplete snapshot")
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("left temporary source")
	}
}

func TestCodexHandoffRejectsMissingConversationAndConcurrentTurn(t *testing.T) {
	s := handoffFixture(t)
	s.aiMu.Lock()
	w := request(t, s, "POST", "/api/documents/doc/codex-handoff", nil)
	s.aiMu.Unlock()
	if w.Code != 409 {
		t.Fatalf("busy: %d", w.Code)
	}
	_, _ = s.Store.DB.Exec("DELETE FROM document_conversations")
	w = request(t, s, "POST", "/api/documents/doc/codex-handoff", nil)
	if w.Code != 409 {
		t.Fatalf("missing: %d %s", w.Code, w.Body.String())
	}
	w = request(t, s, "POST", "/api/documents/missing/codex-handoff", nil)
	if w.Code != 404 {
		t.Fatalf("deleted: %d", w.Code)
	}
}

func runCodexHandoffProcess() {
	enc := json.NewEncoder(os.Stdout)
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var r codexRPCMessage
		if json.Unmarshal(scan.Bytes(), &r) != nil {
			os.Exit(2)
		}
		f, err := os.OpenFile(os.Getenv("NEOREAD_HANDOFF_CAPTURE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(3)
		}
		_, _ = f.Write(append(append([]byte{}, scan.Bytes()...), '\n'))
		_ = f.Close()
		switch r.Method {
		case "thread/name/set":
			if os.Getenv("NEOREAD_HANDOFF_FAIL_NAME") != "" {
				_ = enc.Encode(map[string]any{"id": r.ID, "error": map[string]any{"code": -32000, "message": "failed"}})
			} else {
				_ = enc.Encode(map[string]any{"id": r.ID, "result": map[string]any{}})
			}
		case "initialize":
			_ = enc.Encode(map[string]any{"id": r.ID, "result": map[string]any{}})
		case "initialized":
		case "thread/fork":
			_ = enc.Encode(map[string]any{"id": r.ID, "result": map[string]any{"thread": map[string]any{"id": handoffTestFork}}})
		default:
			os.Exit(4)
		}
	}
}

// Opt-in installed-CLI contract check. All state and responses are synthetic.
// A loopback provider creates the seed turn; no real model or credentials are used.
func TestCodexHandoffInstalledCLI(t *testing.T) {
	if os.Getenv("NEOREAD_CODEX_HANDOFF_LIVE") != "1" {
		t.Skip("set NEOREAD_CODEX_HANDOFF_LIVE=1 for the offline installed-CLI check")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SQLITE_HOME", home)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"fixture-response"}}`,
			`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"fixture-message","content":[{"type":"output_text","text":"Synthetic completed answer"}]}}`,
			`{"type":"response.completed","response":{"id":"fixture-response","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer model.Close()
	config := fmt.Sprintf(`model = "fixture"
model_provider = "offline"
[model_providers.offline]
name = "Offline metadata test"
base_url = "%s/v1"
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
`, model.URL)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	s := handoffFixture(t)
	seed, err := newCodexSession(t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer seed.close()
	seed.persistent = true
	seed.baseInstructions = "Synthetic reading fixture"
	// generate normally verifies NeoRead MCP isolation; the fixture has no MCP.
	if _, err = seed.generate(ctx, AIInput{Prompt: "Synthetic reader question"}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	c := documentConversation{ID: "fixture", ConnectionKey: "codex-native-v1", NativeThread: seed.threadID, NativeCommitted: seed.lastTurnID, LastMessageID: "answer"}
	if calls.Load() != 1 || c.NativeThread == "" || c.NativeCommitted == "" {
		t.Fatal("seed turn was not completed")
	}
	if err = saveDocumentConversation(ctx, s.Store.DB, "doc", "codex", c); err != nil {
		t.Fatal(err)
	}
	seed.close()
	w := request(t, s, "POST", "/api/documents/doc/codex-handoff", nil)
	if w.Code != 200 {
		t.Fatalf("handoff: %d %s", w.Code, w.Body.String())
	}
	var handoff codexHandoff
	if err = json.Unmarshal(w.Body.Bytes(), &handoff); err != nil {
		t.Fatal(err)
	}
	if handoff.ThreadID == c.NativeThread || calls.Load() != 1 {
		t.Fatal("handoff reused the original or called the provider")
	}
	reopened, err := newCodexSession(t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if err = reopened.initializeControl(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := reopened.control(ctx, "thread/resume", map[string]any{"threadId": handoff.ThreadID, "excludeTurns": true})
	if err != nil {
		t.Fatal(err)
	}
	var resumed struct {
		CWD    string `json:"cwd"`
		Thread struct {
			Path string `json:"path"`
		} `json:"thread"`
	}
	want, _ := filepath.EvalSymlinks(handoff.Directory)
	if err = json.Unmarshal(raw, &resumed); err != nil {
		t.Fatal(err)
	}
	got, _ := filepath.EvalSymlinks(resumed.CWD)
	if got != want {
		t.Fatalf("cwd not retained: %s %v", raw, err)
	}
	rollout, err := os.ReadFile(resumed.Thread.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rollout), "READING_CONTEXT.md") {
		t.Fatal("fork lost instructions")
	}
	raw, err = reopened.control(ctx, "thread/items/list", map[string]any{"threadId": handoff.ThreadID, "limit": 50})
	if err != nil || !strings.Contains(string(raw), "Synthetic completed answer") {
		t.Fatalf("fork lost paginated history: %s %v", raw, err)
	}
	t.Log("installed Codex retained the committed fork, snapshot cwd and reading instructions after process restart; handoff made no provider request")
}

func TestCodexHiddenHistoryContinuesTheSameThreadAcrossVisibilityChanges(t *testing.T) {
	s, a := prepareConversationFixture(t)
	fakeAgent(t, "codex", "document-conversation")
	capture := filepath.Join(t.TempDir(), "capture.jsonl")
	t.Setenv("NEOREAD_CONVERSATION_CAPTURE", capture)
	t.Setenv("NEOREAD_DOCUMENT_TEST_BLOCK", a.corpus.Passages[1].ID)
	question := func(prompt string) {
		t.Helper()
		out := conversationRequest(t, s, "codex", prompt)
		if !strings.Contains(out.Body.String(), "event: done") || !strings.Contains(out.Body.String(), "CIVET-42") {
			t.Fatal(out.Body.String())
		}
	}
	question("记住 CIVET-42")
	var raw string
	if err := s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc' AND provider='codex'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var before documentConversation
	if err := json.Unmarshal([]byte(raw), &before); err != nil {
		t.Fatal(err)
	}
	for index, hide := range []bool{true, false, true, false} {
		out := request(t, s, "PUT", "/api/settings", strings.NewReader(jsonText(map[string]bool{"hideCodexHistory": hide})))
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body.String())
		}
		if index == 0 {
			s.Close()
			s = NewServer(s.Store, "test-secret", "")
			t.Cleanup(s.Close)
		}
		question("继续说明刚才记住的内容")
		if err := s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc' AND provider='codex'").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var current documentConversation
		if err := json.Unmarshal([]byte(raw), &current); err != nil || current.NativeThread != before.NativeThread {
			t.Fatal("visibility replaced the native conversation", err, current.NativeThread)
		}
		var hidden bool
		if err := s.Store.DB.QueryRow("SELECT hidden FROM codex_history_threads WHERE thread_id=?", before.NativeThread).Scan(&hidden); err != nil || hidden != hide {
			t.Fatal("visibility not applied", hidden, hide, err)
		}
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 10 {
		t.Fatal("visibility lost messages", len(history), err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(data)
	if strings.Count(trace, `"method":"thread/start"`) != 1 || !strings.Contains(trace, `"method":"thread/unarchive"`) || strings.Contains(trace, `"method":"thread/delete"`) {
		t.Fatal("native history was recreated or deleted", trace)
	}
}

func TestCodexHistoryOwnershipStaysOnTheOriginalMachine(t *testing.T) {
	root := t.TempDir()
	source := archiveFixture(t, filepath.Join(root, "source"))
	archiveDocument(t, source, "paper", "分类")
	if _, err := source.DB.Exec("INSERT INTO codex_history_threads VALUES('owned-thread','paper',1)"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "library.zip")
	if err := ExportLibraryArchive(source.Root, archive, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := source.DB.QueryRow("SELECT count(*) FROM codex_history_threads").Scan(&count); err != nil || count != 1 {
		t.Fatal("export changed original ownership", count, err)
	}
	target := filepath.Join(root, "restored")
	if err := ImportLibraryArchive(archive, target, ""); err != nil {
		t.Fatal(err)
	}
	restored := archiveFixture(t, target)
	if err := restored.DB.QueryRow("SELECT count(*) FROM codex_history_threads").Scan(&count); err != nil || count != 0 {
		t.Fatal("import could control another library's Codex threads", count, err)
	}
}

func TestCodexHistoryVisibilityRejectsNonBooleanValues(t *testing.T) {
	s := testServer(t)
	if out := request(t, s, "PUT", "/api/settings", strings.NewReader(`{"hideCodexHistory":"yes"}`)); out.Code != 400 {
		t.Fatal(out.Code, out.Body.String())
	}
}

// textFixture is a parsed two-page paper with a page number, a figure and one
// translated paragraph.
func textFixture(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	if _, err := s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,library) VALUES('doc','pdf','Concurrency Control','',1,?,?,'papers')", now(), now()); err != nil {
		t.Fatal(err)
	}
	block := func(id string, page int, label, text string) PDFBlock {
		b := PDFBlock{ID: id, Page: page, Label: label, Text: text}
		b.Bounds.X, b.Bounds.Y, b.Bounds.Width, b.Bounds.Height = .1, .3, .6, .1
		return b
	}
	number := block("p1-b3", 1, "number", "1")
	figure := block("p2-b2", 2, "chart", "axis labels")
	figure.Image, figure.Caption = "assets/p2-b2.png", "Figure 1: Throughput under contention."
	m := layoutManifest{Pages: 2, Blocks: []PDFBlock{
		block("p1-b1", 1, "paragraph_title", "Introduction"),
		block("p1-b2", 1, "text", "Long-running transactions block short ones."),
		number,
		block("p2-b1", 2, "text", "We evaluate Oze on TPC-C."),
		figure,
	}}
	dir := s.analysisDir("doc")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	source := "Long-running transactions block short ones."
	if err := s.saveTranslation("doc", TranslationBlock{BlockID: "p1-b2", SourceHash: translationHash(source), Status: "complete", Sentences: []TranslationSentence{{Source: source, Target: "长事务会阻塞短事务。"}}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func decodeBody[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func TestDocumentTextReadsParsedPagesInOrder(t *testing.T) {
	s := textFixture(t)
	w := request(t, s, "GET", "/api/documents/doc/text", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	text := decodeBody[documentText](t, w.Body.Bytes())
	if !text.Parsed || text.Units != 2 || len(text.Blocks) != 4 {
		t.Fatalf("unexpected text: %+v", text)
	}
	if text.Blocks[0].Text != "Introduction" || text.Blocks[3].Text != "Figure 1: Throughput under contention." {
		t.Fatalf("figures read their caption, page numbers are dropped: %+v", text.Blocks)
	}
	if text.Blocks[1].Translation != "" {
		t.Fatal("source side included a translation")
	}

	w = request(t, s, "GET", "/api/documents/doc/text?from=1&to=1&side=both", nil)
	text = decodeBody[documentText](t, w.Body.Bytes())
	if len(text.Blocks) != 2 || text.Blocks[1].Translation != "长事务会阻塞短事务。" || text.Blocks[1].Text == "" {
		t.Fatalf("page range or translation missing: %+v", text.Blocks)
	}
	w = request(t, s, "GET", "/api/documents/doc/text?side=translation&from=2", nil)
	text = decodeBody[documentText](t, w.Body.Bytes())
	for _, b := range text.Blocks {
		if b.Unit != 2 || b.Text != "" {
			t.Fatalf("translation side kept source text or other pages: %+v", b)
		}
	}
	for _, bad := range []string{"?side=all", "?from=0", "?from=3&to=2", "?to=x"} {
		if w := request(t, s, "GET", "/api/documents/doc/text"+bad, nil); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
}

func TestDocumentTextReportsUnparsedPDF(t *testing.T) {
	s := testServer(t)
	if _, err := s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,library) VALUES('raw','pdf','Raw','',1,?,?,'papers')", now(), now()); err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "GET", "/api/documents/raw/text", nil)
	text := decodeBody[documentText](t, w.Body.Bytes())
	if w.Code != 200 || text.Parsed || len(text.Blocks) != 0 {
		t.Fatalf("%d %+v", w.Code, text)
	}
	if w := request(t, s, "GET", "/api/documents/missing/text", nil); w.Code != 404 {
		t.Fatalf("missing document: %d", w.Code)
	}
}

func TestDocumentTextReadsEPUBChapters(t *testing.T) {
	s := testServer(t)
	w := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	if w.Code != 201 {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	doc := decodeBody[Document](t, w.Body.Bytes())
	w = request(t, s, "GET", "/api/documents/"+doc.ID+"/text", nil)
	text := decodeBody[documentText](t, w.Body.Bytes())
	if !text.Parsed || text.Units < 2 || len(text.Blocks) == 0 {
		t.Fatalf("%+v", text)
	}
	w = request(t, s, "GET", "/api/documents/"+doc.ID+"/text?from=2&to=2", nil)
	chapter := decodeBody[documentText](t, w.Body.Bytes())
	if len(chapter.Blocks) == 0 {
		t.Fatal("second chapter is empty")
	}
	for _, b := range chapter.Blocks {
		if b.Unit != 2 || b.Href != chapter.Blocks[0].Href {
			t.Fatalf("chapter range crossed chapters: %+v", b)
		}
	}
}

func TestLibrarySearchMatchesSourceAndTranslation(t *testing.T) {
	s := textFixture(t)
	search := func(query string) []searchHit {
		t.Helper()
		w := request(t, s, "GET", "/api/search?"+query, nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
		}
		return decodeBody[[]searchHit](t, w.Body.Bytes())
	}
	hits := search("q=" + url.QueryEscape("oze TPC-C"))
	if len(hits) != 1 || hits[0].Unit != 2 || hits[0].BlockID != "p2-b1" || hits[0].Side != "source" || hits[0].Title != "Concurrency Control" {
		t.Fatalf("all terms, any case: %+v", hits)
	}
	hits = search("q=" + url.QueryEscape("阻塞"))
	if len(hits) != 1 || hits[0].Side != "translation" || hits[0].Excerpt != "长事务会阻塞短事务。" {
		t.Fatalf("translation hit: %+v", hits)
	}
	if hits = search("q=throughput"); len(hits) != 1 || hits[0].BlockID != "p2-b2" {
		t.Fatalf("figure caption hit: %+v", hits)
	}
	if hits = search("q=oze+missing"); len(hits) != 0 {
		t.Fatalf("a missing term still matched: %+v", hits)
	}
	if hits = search("q=transactions&library=books"); len(hits) != 0 {
		t.Fatalf("library filter ignored: %+v", hits)
	}
	if hits = search("q=e&document=doc&limit=2"); len(hits) != 2 {
		t.Fatalf("limit ignored: %+v", hits)
	}
	for _, bad := range []string{"q=", "q=a&limit=0", "q=a&limit=201", "q=a&library=films"} {
		if w := request(t, s, "GET", "/api/search?"+bad, nil); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	if w := request(t, s, "GET", "/api/search?q=a&document=missing", nil); w.Code != 404 {
		t.Fatalf("missing document: %d", w.Code)
	}
}

func TestSearchExcerptKeepsContextAroundTheTerm(t *testing.T) {
	long := ""
	for range 40 {
		long += "padding "
	}
	excerpt := searchExcerpt(long+"Needle in\n  the haystack "+long, "needle")
	if []rune(excerpt)[0] != '…' || !strings.Contains(excerpt, "Needle in the haystack") {
		t.Fatalf("%q", excerpt)
	}
	if got := searchExcerpt("短文本", "文"); got != "短文本" {
		t.Fatalf("%q", got)
	}
}

func TestImportBumpsLibraryRevision(t *testing.T) {
	s := testServer(t)
	before := s.connectorRevision.Load()
	if w := upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf")); w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if s.connectorRevision.Load() == before {
		t.Fatal("open windows would not see the imported document")
	}
}

func TestDiscoveryFileIsPrivateAndRemovedOnlyByItsWriter(t *testing.T) {
	root := t.TempDir()
	first := Discovery{URL: "http://127.0.0.1:1", Token: "secret", PID: 1, Version: "dev"}
	remove, err := WriteDiscovery(root, first)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, DiscoveryFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("discovery file readable by others: %v", info.Mode())
	}
	data, _ := os.ReadFile(path)
	if decodeBody[Discovery](t, data) != first {
		t.Fatalf("%s", data)
	}
	// A newer server took over; the old one must not delete its file.
	if _, err = WriteDiscovery(root, Discovery{URL: "http://127.0.0.1:2", Token: "other", PID: 2}); err != nil {
		t.Fatal(err)
	}
	remove()
	if _, err = os.Stat(path); err != nil {
		t.Fatal("removed another server's discovery file")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}
