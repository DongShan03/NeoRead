package neoread

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshRetranslationRemovesAllOldParseAndTranslationResults(t *testing.T) {
	s, p, m := translationFixture(t)
	old := newTranslation(m.Blocks[0])
	old.Status = "complete"
	old.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "旧译文。"}}
	if err := s.saveTranslation("doc", old); err != nil {
		t.Fatal(err)
	}
	historical := old
	historical.SourceHash = "historical"
	if err := s.saveTranslation("doc", historical); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("INSERT INTO translation_previous(document_id,block_id,source_hash,body) VALUES(?,?,?,?)", "doc", old.BlockID, old.SourceHash, jsonText(old)); err != nil {
		t.Fatal(err)
	}
	note := scopedAnnotationFixture(t, s, m)
	root := filepath.Join(s.Store.Root, "cache")
	for _, dir := range []string{"doc/translation-failures", "doc/analysis/old-assets", "doc-previous-legacy", "other/analysis"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "keep.txt"), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.queueFreshRetranslation(p, snapshotTranslationOptions(s.aiConfig())); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"translations", "translation_previous"} {
		var count int
		if err := s.Store.DB.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE document_id='doc'").Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "doc")); !os.IsNotExist(err) {
		t.Fatal("old project cache remained", err)
	}
	if _, err := os.Stat(filepath.Join(root, "doc-previous-legacy")); !os.IsNotExist(err) {
		t.Fatal("old backup remained", err)
	}
	if _, err := os.Stat(filepath.Join(root, "other", "analysis", "keep.txt")); err != nil {
		t.Fatal("other project files were removed", err)
	}
	if _, err := s.Store.Document("doc"); err != nil {
		t.Fatal("original document was removed", err)
	}
	var saved Annotation
	if err := json.Unmarshal([]byte(storedAnnotationBody(t, s, note.ID)), &saved); err != nil || saved.Note != note.Note || saved.CreatedAt != note.CreatedAt {
		t.Fatal("user annotation was removed or changed", saved, err)
	}
	options, err := s.retranslationOptions(&Processing{DocumentID: "doc", Retranslating: true})
	if err != nil || !options.ClearHistory || !options.CleanupDone {
		t.Fatal(options, err)
	}
}

func TestFreshRetranslationRollbackAndBusyRejectionKeepExistingContent(t *testing.T) {
	s, p, _ := completedTranslationFixture(t)
	path := filepath.Join(s.analysisDir("doc"), "manifest.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_fresh_reset BEFORE INSERT ON translation_restarts BEGIN SELECT RAISE(FAIL,'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.queueFreshRetranslation(p, translationRunOptions{}); err == nil {
		t.Fatal("failed reset was accepted")
	}
	assertVisibleOldTranslation(t, s)
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("transaction failure removed the cache")
	}
	if _, err := s.Store.DB.Exec("DROP TRIGGER reject_fresh_reset"); err != nil {
		t.Fatal(err)
	}
	work, finish, err := s.beginDocumentTask(t.Context(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(`{"action":"retranslate","clearHistory":true}`))
	if response.Code != 409 || work.Err() != nil || s.blockedProcessing["doc"] || s.reparsingDocuments["doc"] {
		t.Fatal("busy rejection disrupted the task or left the document blocked", response.Code, work.Err())
	}
	finish()
	response = request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(`{"action":"retranslate","clearHistory":true}`))
	if response.Code != 200 || s.blockedProcessing["doc"] || s.reparsingDocuments["doc"] {
		t.Fatal("retry could not start after busy rejection", response.Code, response.Body.String())
	}
}

func completedTranslationFixture(t *testing.T) (*Server, Processing, layoutManifest) {
	t.Helper()
	s, p, m := translationFixture(t)
	for _, b := range m.Blocks {
		tx := newTranslation(b)
		tx.Status = "complete"
		tx.Sentences = []TranslationSentence{{Source: b.Text, Target: "旧译文"}}
		if err := s.saveTranslation("doc", tx); err != nil {
			t.Fatal(err)
		}
	}
	p.Translating = &ProcessingStage{Status: "complete"}
	p.TranslationsDone, p.TranslationsTotal = 2, 2
	aggregateProcessing(&p)
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	// A reachable fake provider is only needed by tests that actually generate.
	configureTranslationTest(t, s, "http://127.0.0.1:1")
	return s, p, m
}

func assertVisibleOldTranslation(t *testing.T, s *Server) {
	t.Helper()
	out := request(t, s, "GET", "/api/documents/doc/translations", nil)
	var items []TranslationBlock
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &items) != nil || len(items) < 2 {
		t.Fatal(out.Code, out.Body.String())
	}
	for _, item := range items[:2] {
		if item.Status != "complete" || len(item.Sentences) != 1 || item.Sentences[0].Target != "旧译文" {
			t.Fatalf("old translation lost: %+v", item)
		}
	}
}

func TestFullRestartQueuesParsedPaperWithoutCompletedTranslations(t *testing.T) {
	s, p, _ := completedTranslationFixture(t)
	if _, err := s.Store.DB.Exec("DELETE FROM translations WHERE document_id='doc'"); err != nil {
		t.Fatal(err)
	}
	p.Status, p.Enabled = "paused", false
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	out := request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(`{"action":"retranslate","restart":true}`))
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	if err := json.Unmarshal(out.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	options, err := s.retranslationOptions(&p)
	if err != nil || !p.Retranslating || p.Status != "queued" || options == nil || options.LayoutReady || !validLayoutRun(options.LayoutRun) {
		t.Fatal("full parse and translation were not queued", p, options, err)
	}
}

func TestRetranslationPreservesTextFormulasAndSnapshotAcrossRestart(t *testing.T) {
	s, p, m := completedTranslationFixture(t)
	formula := m.Blocks[0]
	formula.ID, formula.Label, formula.Image, formula.Text = "p1-b3", "display_formula", "assets/p1-b3.png", "x"
	m.Blocks = append(m.Blocks, formula)
	if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), []byte(jsonText(m)), 0600); err != nil {
		t.Fatal(err)
	}
	f := newTranslation(formula)
	f.Status, f.FormulaMarkdown = "complete", "$$x$$"
	if err := s.saveTranslation("doc", f); err != nil {
		t.Fatal(err)
	}
	oldPrompt := "自然中文，保留术语"
	c := s.aiConfig()
	c.TranslationPrompt = &oldPrompt
	c.TranslationModels = map[string]string{"codex": "chosen-model"}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	p = assistanceRequest(t, s, "retranslate")
	if !p.Retranslating || p.Phase != "translating" || p.Status != "queued" {
		t.Fatal(p)
	}
	assertVisibleOldTranslation(t, s)
	items, _ := s.translations("doc", m)
	if items[0].Status != "pending" || items[2].Status != "complete" || items[2].FormulaMarkdown != "$$x$$" {
		t.Fatal(items)
	}
	if out := request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(`{"action":"retranslate"}`)); out.Code != 409 {
		t.Fatal("duplicate restart accepted", out.Code)
	}
	p = assistanceRequest(t, s, "pause")
	if p.Enabled || !p.Retranslating {
		t.Fatal(p)
	}
	store, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	reopened := NewServer(store, "test-secret", "")
	assertVisibleOldTranslation(t, reopened)
	options, err := reopened.retranslationOptions(&p)
	if err != nil || options.Prompt != oldPrompt || options.Model != "chosen-model" {
		t.Fatal(options, err)
	}
	changed := "其他提示词"
	c.TranslationPrompt = &changed
	c.TranslationModels["codex"] = "other-model"
	frozen := options.apply(c)
	if *frozen.TranslationPrompt != oldPrompt || frozen.TranslationModels["codex"] != "chosen-model" || c.TranslationModels["codex"] != "other-model" {
		t.Fatal("snapshot not isolated")
	}
	p = assistanceRequest(t, reopened, "resume")
	if p.Phase != "translating" || !p.Retranslating {
		t.Fatal("resume scheduled layout again", p)
	}
}

func TestRetranslationWaitsForProviderCompletionAndRetainsFailedOutput(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, m := completedTranslationFixture(t)
			assistanceRequest(t, s, "retranslate")
			doc, _ := s.Store.Document("doc")
			items, _ := s.translations("doc", m)
			batch := translationBatches(m, items, translationBatchCharacters)[0]
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			invoked := false
			stream := &GenerationService{primary: "codex", connections: map[string]generationConnection{
				"codex": {VerifiedText: true, Incremental: true, Adapter: retranslationTestAdapter(func(emit func(ProviderEvent) error) (GenerateResult, error) {
					invoked = true
					text := translationLine(m.Blocks[0]) + translationLine(m.Blocks[1])
					if err := emit(ProviderEvent{Text: text}); err != nil {
						return GenerateResult{}, err
					}
					assertVisibleOldTranslation(t, s)
					if outcome == "failure" {
						return GenerateResult{}, errors.New("connection lost")
					}
					if outcome == "cancel" {
						cancel()
						return GenerateResult{}, context.Canceled
					}
					return GenerateResult{Text: text, FinishReason: "stop"}, nil
				})},
			}}
			attempt, err := s.translationAttempt(ctx, doc, m, batch, stream, func() {})
			if !invoked {
				t.Fatal("provider path was not exercised")
			}
			if err != nil {
				t.Fatal(err)
			}
			if outcome != "success" {
				if attempt.err == nil || len(attempt.decoder.completed) != 0 {
					t.Fatal("failed replacement considered complete", attempt)
				}
				assertVisibleOldTranslation(t, s)
			} else {
				visible, _ := s.visibleTranslations("doc", m)
				if attempt.err != nil || visible[0].Sentences[0].Target != "译文。" {
					t.Fatal(attempt.err, visible)
				}
				previous, _ := s.translationPrevious("doc")
				if len(previous) != 0 {
					t.Fatal("successful replacement retained stale overlay")
				}
			}
		})
	}
}

type retranslationTestAdapter func(func(ProviderEvent) error) (GenerateResult, error)

func (f retranslationTestAdapter) Stream(_ context.Context, _ GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
	return f(emit)
}

func TestRetranslationQueueRollsBackAndJoinsOldWorker(t *testing.T) {
	s, p, m := completedTranslationFixture(t)
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_restart BEFORE INSERT ON translation_restarts BEGIN SELECT RAISE(FAIL,'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.queueRetranslation(p, m, snapshotTranslationOptions(s.aiConfig())); err == nil {
		t.Fatal("transaction failure not returned")
	}
	items, _ := s.translations("doc", m)
	previous, _ := s.translationPrevious("doc")
	if items[0].Status != "complete" || len(previous) != 0 {
		t.Fatal("partially committed restart", items, previous)
	}
	if _, err := s.Store.DB.Exec("DROP TRIGGER reject_restart"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	task := &documentTask{cancel: cancel, done: make(chan struct{})}
	s.activeProcessing = map[string]*documentTask{"doc": task}
	go func() { <-ctx.Done(); s.finishProcessingTask("doc", task) }()
	assistanceRequest(t, s, "retranslate")
	select {
	case <-task.done:
	default:
		t.Fatal("old worker still active")
	}
	assertVisibleOldTranslation(t, s)
}

func TestRetranslationRunsThroughExistingLaneAndClearsRestartState(t *testing.T) {
	s, _, m := completedTranslationFixture(t)
	prepareReparseDocument(t, s)
	reparseFixture(t, m)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := readTranslationInput(t, r).Batch
		if len(batch.Paragraphs) != 2 {
			t.Error("missing retranslation scope")
		}
		writeAPIReply(w, translationLine(m.Blocks[0])+translationLine(m.Blocks[1]))
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	p := assistanceRequest(t, s, "retranslate")
	if err := s.processTranslation(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != "complete" || p.Retranslating || p.TranslationsDone != 2 {
		t.Fatal(p)
	}
	visible, _ := s.visibleTranslations("doc", m)
	if visible[0].Sentences[0].Target != "译文。" {
		t.Fatal(visible)
	}
}

// Exercise the subprocess boundary on every platform without a model download.
func init() {
	mode := os.Getenv("NEOREAD_REPARSE_TEST_CHILD")
	if mode == "" {
		return
	}
	out := ""
	for i, arg := range os.Args {
		if arg == "--from-page" || arg == "--to-page" {
			os.Exit(9)
		}
		if arg == "--output" && i+1 < len(os.Args) {
			out = os.Args[i+1]
		}
	}
	if out == "" {
		os.Exit(8)
	}
	f, _ := os.OpenFile(os.Getenv("NEOREAD_REPARSE_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("parse\n")
	_ = f.Close()
	_ = os.MkdirAll(filepath.Join(out, "assets"), 0700)
	if mode == "fail" {
		_ = os.WriteFile(filepath.Join(out, "manifest.json"), []byte("partial"), 0600)
		os.Exit(3)
	}
	data, _ := os.ReadFile(os.Getenv("NEOREAD_REPARSE_FIXTURE"))
	_ = os.WriteFile(filepath.Join(out, "manifest.json"), data, 0600)
	var m layoutManifest
	_ = json.Unmarshal(data, &m)
	image, _, _ := visionProbe()
	for _, b := range m.Blocks {
		if b.Image != "" {
			_ = os.WriteFile(filepath.Join(out, b.Image), image, 0600)
		}
	}
	_ = os.WriteFile(filepath.Join(out, "paper.md"), []byte("new paper\n[algorithm](assets/p1-b3.png)"), 0600)
	fmt.Printf("{\"event\":\"page\",\"page\":%d,\"total\":%d}\n", m.Pages, m.Pages)
	os.Exit(0)
}

func reparseFixture(t *testing.T, m layoutManifest) string {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(fixture, []byte(jsonText(m)), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv("NEOREAD_NODE", executable)
	t.Setenv("NEOREAD_PROCESSOR", "test-layout-worker")
	t.Setenv("NEOREAD_REPARSE_TEST_CHILD", "success")
	t.Setenv("NEOREAD_REPARSE_FIXTURE", fixture)
	t.Setenv("NEOREAD_REPARSE_CALLS", calls)
	return calls
}

func prepareReparseDocument(t *testing.T, s *Server) {
	t.Helper()
	d, _ := s.Store.Document("doc")
	if err := os.WriteFile(s.Store.File(d), visualPagesPDF(), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFreshRetranslationUsesOnlyNewPDFResultsForPapersAndBooks(t *testing.T) {
	for _, library := range []string{"papers", "books"} {
		t.Run(library, func(t *testing.T) {
			s, _, old := completedTranslationFixture(t)
			prepareReparseDocument(t, s)
			if _, err := s.Store.DB.Exec("UPDATE documents SET library=? WHERE id='doc'", library); err != nil {
				t.Fatal(err)
			}
			fresh := old
			fresh.Blocks = append([]PDFBlock(nil), old.Blocks...)
			fresh.Blocks[0].Text = "Newly parsed sentence."
			calls := reparseFixture(t, fresh)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				batch := readTranslationInput(t, r).Batch
				var reply strings.Builder
				for _, paragraph := range batch.Paragraphs {
					if paragraph.Source == old.Blocks[0].Text {
						t.Error("old parse was sent to translator")
					}
					reply.WriteString(translationParagraphLine(paragraph))
				}
				writeAPIReply(w, reply.String())
			}))
			defer provider.Close()
			configureTranslationTest(t, s, provider.URL)
			response := request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(`{"action":"retranslate","clearHistory":true}`))
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			var p Processing
			if err := json.Unmarshal(response.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if _, err := s.readTranslationSource("doc"); !os.IsNotExist(err) {
				t.Fatal("old layout remained", err)
			}
			if err := s.processTranslation(t.Context(), &p); err != nil || p.Status != "complete" {
				t.Fatal(p, err)
			}
			layout, err := s.readLayout("doc")
			if err != nil || layout.Blocks[0].Text != fresh.Blocks[0].Text {
				t.Fatal(layout, err)
			}
			data, _ := os.ReadFile(calls)
			if string(data) != "parse\n" {
				t.Fatal("did not parse the full PDF once", string(data))
			}
			if library == "books" {
				var status string
				if err := s.Store.DB.QueryRow("SELECT status FROM book_layout_jobs WHERE document_id='doc'").Scan(&status); err != nil || status != "complete" {
					t.Fatal("background book parser will repeat full parse", status, err)
				}
			}
		})
	}
}

func TestRetranslationReparsesBeforeSendingNewTextAndAlgorithmImage(t *testing.T) {
	s, _, old := completedTranslationFixture(t)
	prepareReparseDocument(t, s)
	fresh := old
	fresh.Blocks = append([]PDFBlock(nil), old.Blocks...)
	fresh.Blocks[0].Text = "Newly parsed text."
	algorithm := old.Blocks[0]
	algorithm.ID, algorithm.Label, algorithm.Text, algorithm.Image = "p1-b3", "algorithm", "Function read(txn)", "assets/p1-b3.png"
	fresh.Blocks = append(fresh.Blocks, algorithm)
	calls := reparseFixture(t, fresh)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := readTranslationInput(t, r).Batch
		if len(batch.Paragraphs) != 2 || batch.Paragraphs[0].Source != fresh.Blocks[0].Text {
			t.Error("translation did not use newly parsed text", batch)
		}
		if _, _, err := s.readBlockImage("doc", algorithm.ID); err != nil {
			t.Error("algorithm image not published before translation", err)
		}
		writeAPIReply(w, translationLine(fresh.Blocks[0])+translationLine(fresh.Blocks[1]))
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	p := assistanceRequest(t, s, "retranslate")
	assertVisibleOldTranslation(t, s)
	if err := s.processTranslation(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != "complete" || p.Retranslating {
		t.Fatal(p)
	}
	m, err := s.readLayout("doc")
	if err != nil || m.Blocks[2].Image == algorithm.Image || !validBlockImage(m.Blocks[2]) {
		t.Fatal("new algorithm crop not published", m, err)
	}
	data, _ := os.ReadFile(calls)
	if string(data) != "parse\n" {
		t.Fatal("did not perform one full parse", string(data))
	}
}

func TestRetranslationParseFailureOrCancellationPreservesOldContent(t *testing.T) {
	for _, mode := range []string{"fail", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, _, m := completedTranslationFixture(t)
			prepareReparseDocument(t, s)
			reparseFixture(t, m)
			t.Setenv("NEOREAD_REPARSE_TEST_CHILD", "fail")
			path := filepath.Join(s.analysisDir("doc"), "manifest.json")
			before, _ := os.ReadFile(path)
			p := assistanceRequest(t, s, "retranslate")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			if err := s.processTranslation(ctx, &p); err == nil {
				t.Fatal("unsuccessful parse reported success")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("old layout changed before successful parse")
			}
			assertVisibleOldTranslation(t, s)
			if mode == "fail" && p.Status != "failed" {
				t.Fatal("parse failure was automatically requeued", p)
			}
		})
	}
}

func TestRetranslationResumesPublishedLayoutWithoutParsingAgain(t *testing.T) {
	s, _, m := completedTranslationFixture(t)
	prepareReparseDocument(t, s)
	calls := reparseFixture(t, m)
	p := assistanceRequest(t, s, "retranslate")
	// Fail after publication, before the ready checkpoint commits.
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_layout_ready BEFORE UPDATE ON translation_restarts BEGIN SELECT RAISE(FAIL,'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshRetranslationLayout(t.Context(), &p); err == nil {
		t.Fatal("checkpoint failure was lost")
	}
	_, _ = s.Store.DB.Exec("DROP TRIGGER reject_layout_ready")
	if err := s.refreshRetranslationLayout(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshRetranslationLayout(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(calls)
	if strings.Count(string(data), "parse\n") != 1 {
		t.Fatal("resume repeated layout inference", string(data))
	}
}

func TestRetranslationReadyRetriesBodyEndPublication(t *testing.T) {
	s, _, m := completedTranslationFixture(t)
	prepareReparseDocument(t, s)
	calls := reparseFixture(t, m)
	if _, err := s.Store.DB.Exec(`UPDATE documents SET body_end='{"page":1,"y":0.1}' WHERE id='doc'`); err != nil {
		t.Fatal(err)
	}
	p := assistanceRequest(t, s, "retranslate")
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_body_end BEFORE UPDATE OF body_end ON documents BEGIN SELECT RAISE(FAIL,'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshRetranslationLayout(t.Context(), &p); err == nil {
		t.Fatal("body end failure was lost")
	}
	options, err := s.retranslationOptions(&p)
	if err != nil || !options.LayoutReady {
		t.Fatal("missing ready checkpoint", options, err)
	}
	_, _ = s.Store.DB.Exec("DROP TRIGGER reject_body_end")
	if err := s.refreshRetranslationLayout(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Store.Document("doc")
	if want := layoutBodyEnd(m); err != nil || doc.BodyEnd == nil || *doc.BodyEnd != *want {
		t.Fatal("stale body end after ready recovery", doc.BodyEnd, want, err)
	}
	data, _ := os.ReadFile(calls)
	if strings.Count(string(data), "parse\n") != 1 {
		t.Fatal("ready recovery repeated inference", string(data))
	}
}

func saveExportTranslation(t *testing.T, s *Server, b PDFBlock, target string) {
	t.Helper()
	item := newTranslation(b)
	item.Status = "complete"
	item.Sentences = []TranslationSentence{{Source: b.Text, Target: target}}
	if err := s.saveTranslation("doc", item); err != nil {
		t.Fatal(err)
	}
}

func TestTranslationExportUsesSavedResultsAndMarksGaps(t *testing.T) {
	s, _, m := translationFixture(t)
	saveExportTranslation(t, s, m.Blocks[0], "**第一句**包含 $x^2$。\n\n| 项目 | 值 |\n| --- | --- |\n| 甲 | 1 |")
	partial := newTranslation(m.Blocks[1])
	partial.Status = "running"
	partial.Sentences = []TranslationSentence{{Target: "未完成的临时输出"}}
	if err := s.saveTranslation("doc", partial); err != nil {
		t.Fatal(err)
	}
	m.ParsedPages = []int{1}
	m.IncompletePages = []int{1}
	decoration := m.Blocks[0]
	decoration.ID, decoration.Label, decoration.Text = "p1-b3", "header", "不要导出页眉"
	m.Blocks = append(m.Blocks, decoration)
	if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), []byte(jsonText(m)), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Store.processing("doc")
	w := request(t, s, "GET", "/api/documents/doc/translations/export", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") {
		t.Fatal(w.Code, w.Body.String())
	}
	text := w.Body.String()
	for _, want := range []string{"**第一句**包含 $x^2$。", "| 甲 | 1 |", "已完成 1 / 2", "此段尚未翻译", "## 第 2 页\n\n> 此页尚未解析"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for _, unwanted := range []string{"未完成的临时输出", "不要导出页眉", "test-secret", "First sentence", "此页解析不完整"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("leaked %q", unwanted)
		}
	}
	after, _ := s.Store.processing("doc")
	if jsonText(before) != jsonText(after) {
		t.Fatal("export changed processing state")
	}
}

func TestTranslationExportRejectsEmptyStaleAndUnauthorized(t *testing.T) {
	s, _, m := translationFixture(t)
	if w := request(t, s, "GET", "/api/documents/doc/translations/export", nil); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	saveExportTranslation(t, s, m.Blocks[0], "旧来源的译文")
	m.Blocks[0].Text = "Changed source text."
	if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), []byte(jsonText(m)), 0600); err != nil {
		t.Fatal(err)
	}
	if w := request(t, s, "GET", "/api/documents/doc/translations/export", nil); w.Code != 409 || strings.Contains(w.Body.String(), "旧来源的译文") {
		t.Fatal(w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/api/documents/doc/translations/export", nil))
	if w.Code != 401 {
		t.Fatal("unauthorized export", w.Code)
	}
	for path, status := range map[string]int{
		"/api/documents/doc/translations/export?format=pdf": 400,
		"/api/documents/missing/translations/export":        404,
	} {
		if w := request(t, s, "GET", path, nil); w.Code != status {
			t.Fatal(path, w.Code)
		}
	}
}

func TestTranslationExportRetainsPreviousResultsDuringRetranslation(t *testing.T) {
	s, _, _ := completedTranslationFixture(t)
	assistanceRequest(t, s, "retranslate")
	w := request(t, s, "GET", "/api/documents/doc/translations/export", nil)
	if w.Code != 200 || strings.Count(w.Body.String(), "旧译文") != 2 || strings.Contains(w.Body.String(), "此段尚未翻译") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestTranslationExportCombinesSelectedNotesWithoutSavingThem(t *testing.T) {
	s, _, m := translationFixture(t)
	saveExportTranslation(t, s, m.Blocks[0], "正文译文")
	const notes = "## 论文笔记\n\n所选笔记\n\n## 批注\n\n> 所选摘录\n\n批注内容"
	body := jsonText(map[string]string{"notesMarkdown": notes})
	for _, format := range []string{"markdown", "zip"} {
		w := request(t, s, "POST", "/api/documents/doc/translations/export?format="+format, strings.NewReader(body))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		text := w.Body.String()
		if format == "zip" {
			text = unzipExport(t, w.Body.Bytes())["translation.md"]
		}
		if !strings.Contains(text, "正文译文") || !strings.HasSuffix(text, "---\n\n"+notes+"\n") || strings.Count(text, "# test\n") != 1 {
			t.Fatal("selected content missing or repeated", text)
		}
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT COUNT(*) FROM document_notes WHERE document_id='doc'").Scan(&count); err != nil || count != 0 {
		t.Fatal("download changed notes", count, err)
	}
	w := request(t, s, "POST", "/api/documents/doc/translations/export", strings.NewReader(`{"unknown":true}`))
	if w.Code != 400 {
		t.Fatal("accepted unknown export fields", w.Code)
	}
}

func unzipExport(t *testing.T, data []byte) map[string]string {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range z.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name] = string(b)
	}
	return files
}

func TestTranslationExportBundlesImagesAndPreservesFormulaAndCode(t *testing.T) {
	s, _, m := translationFixture(t)
	saveExportTranslation(t, s, m.Blocks[0], "正文译文")
	image := m.Blocks[0]
	image.ID, image.Label, image.Image, image.Text, image.Caption = "p1-b3", "chart", "assets/p1-b3.png", "", "Figure [one]"
	saveExportTranslation(t, s, image, "图一：结果")
	formula := m.Blocks[0]
	formula.ID, formula.Label, formula.Image = "p1-b4", "display_formula", "assets/p1-b4.png"
	f := newTranslation(formula)
	f.Status, f.FormulaMarkdown = "complete", "$$\nx^2\n$$"
	if err := s.saveTranslation("doc", f); err != nil {
		t.Fatal(err)
	}
	algorithm := m.Blocks[0]
	algorithm.ID, algorithm.Label, algorithm.Text = "p1-b5", "algorithm", "if x: print(```)"
	reference := m.Blocks[0]
	reference.ID, reference.Label, reference.Text = "p1-b6", "reference_content", "[1] Original reference."
	m.Blocks = append(m.Blocks, image, formula, algorithm, reference)
	if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), []byte(jsonText(m)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), image.Image), []byte("image data"), 0600); err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "GET", "/api/documents/doc/translations/export?format=zip", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatal(w.Code, w.Body.String())
	}
	files := unzipExport(t, w.Body.Bytes())
	if len(files) != 2 || files["assets/image-0001.png"] != "image data" {
		t.Fatal(files)
	}
	text := files["translation.md"]
	for _, want := range []string{"![Figure \\[one\\]](assets/image-0001.png)", "图一：结果", "$$\nx^2\n$$", "````text\nif x: print(```)\n````", "[1] Original reference."} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Index(text, "正文译文") > strings.Index(text, "图一：结果") {
		t.Fatal("reading order changed")
	}
	if err := os.Remove(filepath.Join(s.analysisDir("doc"), image.Image)); err != nil {
		t.Fatal(err)
	}
	if w := request(t, s, "GET", "/api/documents/doc/translations/export?format=zip", nil); w.Code != 500 || w.Header().Get("Content-Disposition") != "" {
		t.Fatal("missing image produced download", w.Code)
	}
	if w := request(t, s, "GET", "/api/documents/doc/translations/export", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "图片未包含") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestTranslationExportEPUBReadingOrderAndLocalImages(t *testing.T) {
	s, _ := epubRangeFixture(t, nil)
	cache := filepath.Join(s.Store.Root, "cache", "doc")
	if err := os.WriteFile(filepath.Join(cache, "a.xhtml"), []byte(`<html><body><h1>Title</h1><p>Paragraph.</p><img src="figure.png" alt="Figure"/><img src="figure.png" alt="Again"/></body></html>`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "figure.png"), []byte("EPUB image"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := s.readEPUBBlocks("doc")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range m.translationItems() {
		item.Status = "complete"
		item.Sentences = []TranslationSentence{{Target: "译文 " + item.BlockID}}
		if err := s.saveTranslation("doc", item); err != nil {
			t.Fatal(err)
		}
	}
	w := request(t, s, "GET", "/api/documents/doc/translations/export?format=zip", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	files := unzipExport(t, w.Body.Bytes())
	text := files["translation.md"]
	if len(files) != 2 || files["assets/image-0001.png"] != "EPUB image" || strings.Count(text, "(assets/image-0001.png)") != 2 {
		t.Fatal(files)
	}
	if strings.Contains(text, "## 第 1 页") || !strings.Contains(text, "### 译文 "+m[0].ID) || strings.Index(text, m[1].ID) > strings.Index(text, m[len(m)-1].ID) {
		t.Fatal("lost EPUB reading order", text)
	}
}

func TestTranslationArchiveRejectsEscapingImages(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private.png")
	if err := os.WriteFile(outside, []byte("private contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.png")); err != nil {
		t.Skip(err)
	}
	for _, source := range []string{outside, "../private.png", "escape.png"} {
		var out bytes.Buffer
		if err := writeTranslationArchive(&out, root, "text", []translationExportAsset{{source, "assets/image.png"}}); err == nil || bytes.Contains(out.Bytes(), []byte("private contents")) {
			t.Fatal("escaped document root", source, err)
		}
	}
}

// Inspect instructions at the provider boundary, for text and image requests.
func TestCLISystemPrompt(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		for _, vision := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/image=%t", provider, vision), func(t *testing.T) {
				fakeAgent(t, provider, "success")
				s := testServer(t)
				capture := filepath.Join(t.TempDir(), "system.txt")
				t.Setenv("NEOREAD_SYSTEM_CAPTURE", capture)
				in := AIInput{Prompt: "Explain the supplied formula."}
				if vision {
					in.Image = []byte{1}
				}
				if _, err := invokeCLI(t.Context(), s.Store.Root, provider, "", in, nil); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(capture)
				if err != nil || !strings.Contains(string(data), readerSystemPrompt) {
					t.Fatalf("shared system prompt missing: %v", err)
				}
			})
		}
	}
}

func TestAPISystemPrompt(t *testing.T) {
	for _, vision := range []bool{false, true} {
		t.Run(fmt.Sprintf("image=%t", vision), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
					t.Error("expected system and user messages", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var system string
				if err := json.Unmarshal(body.Messages[0].Content, &system); err != nil || system != readerSystemPrompt || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
					t.Error("shared system prompt missing or incorrectly scoped", err)
				}
				if !strings.Contains(string(body.Messages[1].Content), "Explain") || (vision && !strings.Contains(string(body.Messages[1].Content), "data:image/png;base64,")) {
					t.Error("user content lost")
				}
				writeAPIReply(w, "ok")
			}))
			defer upstream.Close()
			in := AIInput{Prompt: "Explain"}
			if vision {
				in.Image = []byte{1}
			}
			if _, err := invokeAPI(t.Context(), APIConnection{URL: upstream.URL, Model: "test"}, in, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
