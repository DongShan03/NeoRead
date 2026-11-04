package neoread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func overviewTestJob(t *testing.T, s *Server) overviewJob {
	t.Helper()
	job := overviewJob{DocumentID: "doc", ID: id(), Status: "running"}
	if _, err := s.Store.DB.Exec("INSERT INTO document_overview_jobs VALUES(?,?,?,'') ON CONFLICT(document_id) DO UPDATE SET job_id=excluded.job_id,status=excluded.status", job.DocumentID, job.ID, job.Status); err != nil {
		t.Fatal(err)
	}
	return job
}
func overviewTestService(t *testing.T, calls *int, failAt *int) *GenerationService {
	t.Helper()
	adapter := adapterFunc(func(ctx context.Context, r GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
		*calls++
		if *calls == *failAt {
			return GenerateResult{}, errors.New("upstream interrupted")
		}
		raw := strings.SplitN(r.Input.Prompt, "资料：\n", 2)[1]
		var passages []documentPassage
		_ = json.Unmarshal([]byte(raw), &passages)
		block := ""
		if len(passages) > 0 {
			block = passages[0].ID
		}
		if block == "" {
			var notes []overviewContent
			_ = json.Unmarshal([]byte(raw), &notes)
			block = notes[0].Findings[0].BlockIDs[0]
		}
		text := jsonText(overviewContent{Summary: "研究 cobalt 协议及其适用条件。", Findings: []overviewPoint{{Text: "协议用于串行交易", BlockIDs: []string{block}}}, Terms: []string{"cobalt", "COBALT-19"}, Limitations: []overviewPoint{}})
		if err := emit(ProviderEvent{Text: text}); err != nil {
			return GenerateResult{}, err
		}
		return GenerateResult{Text: text, FinishReason: "stop"}, nil
	})
	return &GenerationService{primary: "codex", connections: map[string]generationConnection{"codex": {Adapter: adapter, VerifiedText: true, Incremental: true}}}
}
func TestOverviewGeneratePersistAndReuse(t *testing.T) {
	s, a := originalDocumentFixture(t)
	job := overviewTestJob(t, s)
	calls, failAt := 0, -1
	g := overviewTestService(t, &calls, &failAt)
	if err := s.compileOverview(t.Context(), job, g); err != nil {
		t.Fatal(err)
	}
	o, err := s.currentOverview(t.Context(), a.corpus)
	if err != nil || o == nil || o.Status != "complete" || o.CompiledPassages != len(a.corpus.Passages) || calls != 1 {
		t.Fatal(o, err, calls)
	}
	if err = s.compileOverview(t.Context(), job, g); err != nil || calls != 1 {
		t.Fatal("cached overview regenerated", err, calls)
	}
	store, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	saved, err := NewServer(store, "test-secret", "").currentOverview(t.Context(), a.corpus)
	if err != nil || saved == nil || saved.Content.Summary != o.Content.Summary {
		t.Fatal("overview lost after restart", err)
	}
	var indexed int
	if err = store.DB.QueryRow("SELECT count(*) FROM document_overviews_fts WHERE document_id='doc'").Scan(&indexed); err != nil || indexed != 1 {
		t.Fatal("overview index missing", err, indexed)
	}
	_, _ = store.DB.Exec("DELETE FROM documents WHERE id='doc'")
	_ = store.DB.QueryRow("SELECT count(*) FROM document_overviews_fts WHERE document_id='doc'").Scan(&indexed)
	if indexed != 0 {
		t.Fatal("orphan overview index")
	}
}
func TestOverviewResumeChaptersAndSourceBoundaries(t *testing.T) {
	s, a := originalDocumentFixture(t)
	_, _ = s.Store.DB.Exec(`INSERT INTO settings VALUES('neoread','{"documentContextTokens":8192}')`)
	c := a.corpus
	c.Passages = nil
	for i := 0; i < 6; i++ {
		c.Passages = append(c.Passages, documentPassage{ID: fmt.Sprint(i), Section: fmt.Sprint(i), Label: fmt.Sprintf("Chapter %d", i), Text: strings.Repeat("original evidence ", 350), Location: json.RawMessage(`{"type":"epub","href":"a.xhtml"}`)})
	}
	c.Revision = "large-corpus"
	_, _ = s.Store.DB.Exec("UPDATE document_corpora SET revision=?,body=? WHERE document_id='doc'", c.Revision, jsonText(c))
	job := overviewTestJob(t, s)
	calls, failAt := 0, 2
	g := overviewTestService(t, &calls, &failAt)
	if err := s.compileOverview(t.Context(), job, g); err == nil {
		t.Fatal("failed request accepted")
	}
	partial, err := s.Store.loadOverview(t.Context(), "doc")
	if err != nil || len(partial.Sections) != 1 || partial.Content != nil || partial.Status != "partial" {
		t.Fatal("checkpoint missing", partial, err)
	}
	failAt = -1
	if err = s.compileOverview(t.Context(), job, g); err != nil {
		t.Fatal(err)
	}
	complete, _ := s.Store.loadOverview(t.Context(), "doc")
	if complete.Status != "complete" || complete.CompiledPassages != 6 || calls != 8 {
		t.Fatal("retry did not reuse first chapter", complete.Status, complete.CompiledPassages, calls)
	}
	c.Revision = "changed"
	if o, e := s.currentOverview(t.Context(), c); e != nil || o != nil {
		t.Fatal("stale overview reused", e)
	}
	_, _ = s.Store.DB.Exec("UPDATE document_overview_jobs SET status='canceled' WHERE document_id='doc'")
	if err = s.Store.saveOverview(t.Context(), job, complete); errorKind(err) != ErrorCanceled {
		t.Fatal("canceled job published", err)
	}
}
func TestOverviewValidationAndOptIn(t *testing.T) {
	s, a := originalDocumentFixture(t)
	for _, raw := range []string{`{"summary":"ok","findings":[{"text":"fabricated","blockIds":["unknown"]}]}`, `{"summary":""}`, `{"summary":"ok","limitations":[{"text":"x","blockIds":[]}]}`} {
		if _, err := parseOverviewContent(raw, map[string]bool{a.corpus.Passages[0].ID: true}); err == nil {
			t.Fatal("invalid overview accepted", raw)
		}
	}
	for _, settings := range []string{"", `{}`, `{"autoGenerateOverview":false}`} {
		if err := queueImportedOverview(s.Store.DB, "doc", settings); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM document_overview_jobs").Scan(&n)
	if n != 0 {
		t.Fatal("historical/default document triggered paid job")
	}
	if err := queueImportedOverview(s.Store.DB, "doc", `{"autoGenerateOverview":true,"autoTranslatePDF":false}`); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM document_overview_jobs").Scan(&n)
	if n != 1 {
		t.Fatal("overview incorrectly depends on translation")
	}
}

func TestOverviewHTTPJobsAndSettings(t *testing.T) {
	s, _ := originalDocumentFixture(t)
	w := request(t, s, "POST", "/api/documents/doc/overview", strings.NewReader(`{"regenerate":false}`))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var first string
	_ = s.Store.DB.QueryRow("SELECT job_id FROM document_overview_jobs WHERE document_id='doc'").Scan(&first)
	w = request(t, s, "POST", "/api/documents/doc/overview", strings.NewReader(`{"regenerate":true}`))
	var second string
	_ = s.Store.DB.QueryRow("SELECT job_id FROM document_overview_jobs WHERE document_id='doc'").Scan(&second)
	if w.Code != 200 || first != second {
		t.Fatal("duplicate request replaced queued job")
	}
	w = request(t, s, "DELETE", "/api/documents/doc/overview/job", nil)
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	o, err := s.overviewState(t.Context(), "doc")
	if err != nil || o.Status != "canceled" {
		t.Fatal(o, err)
	}
	for _, body := range []string{`{"documentContextTokens":1}`, `{"documentContextTokens":8192.5}`, `{"autoGenerateOverview":"true"}`} {
		if w = request(t, s, "PUT", "/api/settings", strings.NewReader(body)); w.Code != 400 {
			t.Fatal("invalid settings accepted", w.Code)
		}
	}
}

func TestOverviewRegenerationPreservesPublishedUntilSuccess(t *testing.T) {
	for _, stop := range []string{"failure", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			s, a := originalDocumentFixture(t)
			_, _ = s.Store.DB.Exec(`INSERT INTO settings VALUES('neoread','{"documentContextTokens":8192}')`)
			c := a.corpus
			c.Passages = nil
			for i := 0; i < 6; i++ {
				c.Passages = append(c.Passages, documentPassage{ID: fmt.Sprint(i), Section: fmt.Sprint(i), Label: fmt.Sprint(i), Text: strings.Repeat("original evidence ", 350), Location: json.RawMessage(`{"type":"epub","href":"a.xhtml"}`)})
			}
			c.Revision = "regeneration-corpus"
			_, _ = s.Store.DB.Exec("UPDATE document_corpora SET revision=?,body=? WHERE document_id='doc'", c.Revision, jsonText(c))
			original := documentOverview{DocumentID: "doc", Revision: c.Revision, Version: overviewVersion, Status: "complete", Coverage: c.Coverage, Content: &overviewContent{Summary: "published summary"}, TotalPassages: 6, CompiledPassages: 6}
			old := overviewTestJob(t, s)
			if err := s.Store.saveOverview(t.Context(), old, original); err != nil {
				t.Fatal(err)
			}
			_, _ = s.Store.DB.Exec("UPDATE document_overview_jobs SET status='complete'")
			assertPublished := func() {
				t.Helper()
				o, err := s.currentOverview(t.Context(), c)
				if err != nil || o == nil || o.Content.Summary != "published summary" {
					t.Fatalf("published overview lost: %+v %v", o, err)
				}
				var indexed string
				if err = s.Store.DB.QueryRow("SELECT content FROM document_overviews_fts WHERE document_id='doc'").Scan(&indexed); err != nil || !strings.Contains(indexed, "published summary") {
					t.Fatal("published search index lost", err, indexed)
				}
			}
			r := request(t, s, "POST", "/api/documents/doc/overview", strings.NewReader(`{"regenerate":true}`))
			if r.Code != 202 {
				t.Fatal(r.Body)
			}
			assertPublished()
			var job overviewJob
			job.DocumentID = "doc"
			_ = s.Store.DB.QueryRow("SELECT job_id FROM document_overview_jobs WHERE document_id='doc'").Scan(&job.ID)
			_, _ = s.Store.DB.Exec("UPDATE document_overview_jobs SET status='running'")
			calls, failAt := 0, 2
			if err := s.compileOverview(t.Context(), job, overviewTestService(t, &calls, &failAt)); err == nil {
				t.Fatal("injected failure succeeded")
			}
			assertPublished()
			if stop == "cancel" {
				request(t, s, "DELETE", "/api/documents/doc/overview/job", nil)
			} else {
				_, _ = s.Store.DB.Exec("UPDATE document_overview_jobs SET status='failed'")
			}
			// A late completion from the old attempt cannot replace the published copy.
			if err := s.Store.saveOverview(t.Context(), job, original); errorKind(err) != ErrorCanceled {
				t.Fatal("late writer accepted", err)
			}
			assertPublished()
			r = request(t, s, "POST", "/api/documents/doc/overview", strings.NewReader(`{"regenerate":false}`))
			if r.Code != 202 {
				t.Fatal(r.Body)
			}
			_ = s.Store.DB.QueryRow("SELECT job_id FROM document_overview_jobs WHERE document_id='doc'").Scan(&job.ID)
			_, _ = s.Store.DB.Exec("UPDATE document_overview_jobs SET status='running'")
			reopened, err := OpenStore(s.Store.Root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.DB.Close()
			resumed := NewServer(reopened, "test-secret", "")
			defer resumed.Close()
			failAt = -1
			if err = resumed.compileOverview(t.Context(), job, overviewTestService(t, &calls, &failAt)); err != nil {
				t.Fatal(err)
			}
			o, err := s.currentOverview(t.Context(), c)
			if err != nil || o == nil || o.Content.Summary == "published summary" || calls != 8 {
				t.Fatalf("did not publish/resume correctly: %+v %v calls=%d", o, err, calls)
			}
			var n int
			_ = s.Store.DB.QueryRow("SELECT count(*) FROM document_overview_drafts").Scan(&n)
			if n != 0 {
				t.Fatal("published draft retained")
			}
		})
	}
}

func TestPartialOverviewRegenerationCallsModel(t *testing.T) {
	s, a := originalDocumentFixture(t)
	a.corpus.Coverage.Status = "partial"
	_, _ = s.Store.DB.Exec("UPDATE document_corpora SET body=? WHERE document_id='doc'", jsonText(a.corpus))
	job := overviewTestJob(t, s)
	calls, failAt := 0, -1
	g := overviewTestService(t, &calls, &failAt)
	if err := s.compileOverview(t.Context(), job, g); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Store.DB.Exec("UPDATE document_overview_jobs SET status='complete'")
	r := request(t, s, "POST", "/api/documents/doc/overview", strings.NewReader(`{"regenerate":true}`))
	if r.Code != 202 {
		t.Fatal(r.Body)
	}
	_ = s.Store.DB.QueryRow("SELECT job_id FROM document_overview_jobs WHERE document_id='doc'").Scan(&job.ID)
	_, _ = s.Store.DB.Exec("UPDATE document_overview_jobs SET status='running'")
	if err := s.compileOverview(t.Context(), job, g); err != nil || calls != 2 {
		t.Fatal("regeneration reused prior sections", err, calls)
	}
}

// Opt-in, original synthetic text only. Never alter the user's NeoRead config.
func TestLiveOverviewThenOriginal(t *testing.T) {
	provider := os.Getenv("NEOREAD_TEST_OVERVIEW_PROVIDER")
	if provider != "codex" && provider != "api" {
		t.Skip("set NEOREAD_TEST_OVERVIEW_PROVIDER=codex or api")
	}
	s, a := originalDocumentFixture(t)
	config := AIConfig{Primary: provider, Models: map[string]string{}, Capabilities: map[string]Capability{}}
	if provider == "api" {
		data, err := os.ReadFile(os.Getenv("NEOREAD_TEST_DOCUMENT_API_CONFIG"))
		if err != nil {
			t.Fatal("existing NeoRead API connection unavailable")
		}
		var saved savedConfig
		if json.Unmarshal(data, &saved) != nil {
			t.Fatal("invalid NeoRead config")
		}
		config.API = saved.Config.API
		if config.API.URL != "https://api.deepseek.com" && config.API.URL != "https://api.deepseek.com/v1" {
			t.Fatal("expected official DeepSeek connection")
		}
		model := "deepseek-v4-pro"
		config.Models[provider] = model
		config.APIModels = []string{model}
		key := apiCapabilityKey(model)
		config.Capabilities[key] = Capability{Text: true, Tools: "passed", ToolSchemaVersion: documentToolSchemaVersion, Fingerprint: configPrint(config, key)}
	} else {
		config.Capabilities[provider] = Capability{Text: true, Fingerprint: configPrint(config, provider)}
	}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	job := overviewTestJob(t, s)
	start := time.Now()
	if err := s.compileOverview(ctx, job, s.generationService(config)); err != nil {
		t.Fatal(err)
	}
	overview, err := s.currentOverview(ctx, a.corpus)
	if err != nil || overview == nil || overview.CompiledPassages != len(a.corpus.Passages) {
		t.Fatal("overview missing", err)
	}
	t.Logf("overview provider=%s elapsed=%s coverage=%d/%d", provider, time.Since(start).Round(time.Millisecond), overview.CompiledPassages, overview.TotalPassages)
	for _, question := range []string{"这篇文章大致讲什么？简洁介绍即可。", "请核对原文：cobalt 协议的精确 nonce、适用条件和实验交易数量分别是什么？请引用原文。"} {
		callCtx, stop := context.WithTimeout(ctx, 3*time.Minute)
		a.ctx = callCtx
		input := AIInput{Prompt: question, Chat: &chatInput{DocumentID: "doc", Question: question, Agent: a}}
		started := time.Now()
		var output strings.Builder
		emit := func(e ProviderEvent) error { output.WriteString(e.Text); return nil }
		var result GenerateResult
		if provider == "codex" {
			result, err = streamCodexDocumentChat(callCtx, s.Store.Root, "", input, emit)
		} else {
			req, e := s.apiAgentAdapter(config, taskChat).resolve(callCtx, false)
			if e != nil {
				stop()
				t.Fatal(e)
			}
			result, err = streamAPIDocumentChat(callCtx, config.API, req, input, emit)
		}
		if err != nil {
			stop()
			t.Fatal(err)
		}
		if result.Text != output.String() {
			stop()
			t.Fatal("stream mismatch")
		}
		if strings.Contains(question, "nonce") && (!strings.Contains(result.Text, "COBALT-19") || !strings.Contains(result.Text, "37") || len(a.Evidence) == 0) {
			stop()
			t.Fatal("specific claim did not read original", result.Text)
		}
		if _, e := a.references(result.Text); e != nil {
			stop()
			t.Fatal(e)
		}
		t.Logf("question=%s elapsed=%s tools=%d originals=%d window=%d source=%s", question, time.Since(started).Round(time.Millisecond), len(a.Traces), len(a.Evidence), a.contextTokens, a.contextSource)
		stop()
	}
}

func TestOverviewReadingDoesNotRequireOriginal(t *testing.T) {
	s, a := originalDocumentFixture(t)
	job := overviewTestJob(t, s)
	calls, failAt := 0, -1
	if err := s.compileOverview(t.Context(), job, overviewTestService(t, &calls, &failAt)); err != nil {
		t.Fatal(err)
	}
	in := AIInput{Prompt: "这篇文章大致讲什么？", Chat: &chatInput{Agent: a}}
	if err := a.prepareReading(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := a.beginConversation(t.Context(), in, "test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.conversationInput(in, true), "generated_overview") {
		t.Fatal("overview not seeded")
	}
	a.stopReading("预留最终回答时间")
	if len(a.Evidence) != 0 {
		t.Fatal("overview forged original evidence")
	}

	result, err := a.finishDocumentAnswer(t.Context(), GenerateResult{Text: "介绍研究问题。"}, nil)
	if err != nil || !strings.Contains(result.Text, "未逐段核对原文") {
		t.Fatal("missing provenance", result, err)
	}
	a.providedSources = true
	withExcerpt, err := a.finishDocumentAnswer(t.Context(), GenerateResult{Text: "翻译用户摘录。"}, nil)
	if err != nil || !strings.Contains(withExcerpt.Text, "原文摘录") {
		t.Fatal("provided original described as overview-only", err)
	}
	if _, err = a.references("摘要中的位置 [S1]"); err == nil {
		t.Fatal("summary anchor accepted as original citation")
	}
}
func TestDocumentRangeReadAndTokenBudget(t *testing.T) {
	_, a := originalDocumentFixture(t)
	raw, err := a.call(t.Context(), "read_passage", json.RawMessage(`{"full":true}`))
	if err != nil || len(a.Evidence) != len(a.corpus.Passages) || strings.Contains(string(raw), `"truncated":true`) {
		t.Fatal("short document not read in one call", err, string(raw))
	}
	_, b := originalDocumentFixture(t)
	b.prepared = true
	b.sourceTokens = 2500
	// Keep fixture source revision; tool selection reads an immutable test snapshot.
	b.corpus.Passages = nil
	for i := 0; i < 40; i++ {
		b.corpus.Passages = append(b.corpus.Passages, documentPassage{ID: fmt.Sprint(i), Section: "chapter", Text: strings.Repeat("fact ", 100), Location: json.RawMessage(`{"type":"pdf","page":3}`)})
	}
	raw, err = b.call(t.Context(), "read_passage", json.RawMessage(`{"fromPage":3,"toPage":3}`))
	if err != nil || !strings.Contains(string(raw), `"truncated":true`) || !strings.Contains(string(raw), `"nextCursor"`) || b.evidenceTokens > b.sourceTokens {
		t.Fatal("range ignored context budget", err, string(raw))
	}
	var response struct {
		Next string `json:"nextCursor"`
	}
	_ = json.Unmarshal(raw, &response)
	before := len(b.Evidence)
	b.sourceTokens = 20000
	raw, err = b.call(t.Context(), "read_passage", json.RawMessage(jsonText(map[string]any{"fromPage": 3, "toPage": 3, "cursor": response.Next})))
	if err != nil || len(b.Evidence) != 40 || before == 0 {
		t.Fatal("continuation lost paragraphs", err, len(b.Evidence), string(raw))
	}
	if _, valid := documentReadIndices(b.corpus, documentReadRequest{Full: true, FromPage: 3, ToPage: 3}); valid {
		t.Fatal("ambiguous selectors accepted")
	}
	if _, valid := documentReadIndices(b.corpus, documentReadRequest{Section: "missing", Cursor: "0"}); valid {
		t.Fatal("cursor escaped requested range")
	}
}
func TestDocumentRepeatingToolStopsWithoutDiscardingOverview(t *testing.T) {
	_, a := originalDocumentFixture(t)
	for i := 0; i < 3; i++ {
		if _, err := a.call(t.Context(), "get_outline", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if a.readingStopped() || a.repeatedCalls != 3 {
		t.Fatal("identical requests should warn without stopping")
	}
}

func TestDocumentOverviewReplacesMetadataBackground(t *testing.T) {
	for _, compiled := range []bool{false, true} {
		t.Run(fmt.Sprint(compiled), func(t *testing.T) {
			s, a := originalDocumentFixture(t)
			if compiled {
				calls, failAt := 0, -1
				if err := s.compileOverview(t.Context(), overviewTestJob(t, s), overviewTestService(t, &calls, &failAt)); err != nil {
					t.Fatal(err)
				}
			}
			in := AIInput{Prompt: "介绍文章", Chat: &chatInput{Title: "article", Summary: "METADATA-ABSTRACT-MARKER", Question: "介绍文章", Agent: a}}
			if err := a.prepareReading(t.Context(), in); err != nil {
				t.Fatal(err)
			}
			if _, err := a.beginConversation(t.Context(), in, "test"); err != nil {
				t.Fatal(err)
			}
			prompt := a.conversationInput(in, true)
			if strings.Contains(prompt, "METADATA-ABSTRACT-MARKER") == compiled {
				t.Fatal("redundant or missing document background", prompt)
			}
			if compiled && !strings.Contains(prompt, "generated_overview") {
				t.Fatal("compiled overview omitted")
			}
			if a.providedSources {
				t.Fatal("abstract mistaken for explicit original text")
			}
			next := a.conversationInput(in, false)
			if strings.Contains(next, "METADATA-ABSTRACT-MARKER") || strings.Contains(next, "generated_overview") {
				t.Fatal("unchanged background resent")
			}
			if compiled {
				result, err := a.finishDocumentAnswer(t.Context(), GenerateResult{Text: "介绍主题。"}, nil)
				if err != nil || !strings.Contains(result.Text, "未逐段核对原文") || strings.Contains(result.Text, "原文摘录") {
					t.Fatal("overview-only answer mislabeled", err, result.Text)
				}
			}
		})
	}
}

func waitPreparedCorpus(t *testing.T, s *Server, documentID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if s.Store.DB.QueryRow("SELECT count(*) FROM document_corpora WHERE document_id=?", documentID).Scan(&count) == nil && count == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("imported original text was not indexed before chat")
}

func TestDocumentCorpusPreparedAfterImportWithoutChat(t *testing.T) {
	s := testServer(t)
	w := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.Store.DB.QueryRow("SELECT status FROM document_corpus_jobs WHERE document_id=?", d.ID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("import did not queue indexing: %s %v", status, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stop := s.StartProcessing(ctx)
	defer func() { cancel(); stop() }()
	waitPreparedCorpus(t, s, d.ID)
	corpus, err := s.documentCorpus(t.Context(), d.ID, func() error {
		t.Error("cached import index displayed a preparation activity")
		return nil
	})
	if err != nil || len(corpus.Passages) == 0 {
		t.Fatalf("imported original text unavailable: %v", err)
	}
}

func TestDocumentCorpusPreparedDuringPDFParsing(t *testing.T) {
	s, p := processingFixture(t)
	t.Setenv("NEOREAD_NODE", "/nonexistent-node")
	t.Setenv("NEOREAD_CORPUS_PROCESSOR", "/nonexistent-worker")
	if err := s.learnPDF(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM document_corpora WHERE document_id='doc'").Scan(&count); err != nil || count != 1 {
		t.Fatal("PDF parsing did not publish its original-text index", err)
	}
	// An unrelated import preparing its index must not block a cached document.
	s.corpusMu.Lock()
	defer s.corpusMu.Unlock()
	if _, err := s.documentCorpus(t.Context(), "doc", func() error {
		t.Error("cached PDF displayed preparation or attempted to rebuild")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentCorpusRecoveryAndJobDeletion(t *testing.T) {
	s, _ := epubRangeFixture(t, nil)
	if err := s.Store.queueDocumentCorpus("doc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("UPDATE document_corpus_jobs SET status='running' WHERE document_id='doc'"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.StartProcessing(ctx)()
	var status string
	if err := s.Store.DB.QueryRow("SELECT status FROM document_corpus_jobs WHERE document_id='doc'").Scan(&status); err != nil || status != "queued" {
		t.Fatalf("interrupted indexing not recovered: %s %v", status, err)
	}
	if code, message := s.purgeDocument(t.Context(), "doc"); code != 204 {
		t.Fatal(code, message)
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM document_corpus_jobs").Scan(&count)
	if count != 0 {
		t.Fatal("document deletion left an indexing job")
	}
}
