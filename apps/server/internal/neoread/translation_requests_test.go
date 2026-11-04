package neoread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func paperDoc(t *testing.T, s *Server, data []byte) Document {
	t.Helper()
	w := uploadTo(t, s, "papers", "paper.pdf", data)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}
func assistanceRequest(t *testing.T, s *Server, action string) Processing {
	t.Helper()
	s.Token = "test-secret"
	body, _ := json.Marshal(map[string]any{"action": action})
	w := request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(string(body)))
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", action, w.Code, w.Body.String())
	}
	var p Processing
	if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestPrioritizeTranslationReordersQueueAndCancelsOnlyOtherTranslation(t *testing.T) {
	s, p := processingFixture(t)
	s.Token = "test-secret"
	p.Enabled, p.Phase, p.Status = true, "translating", "queued"
	p.Translating = &ProcessingStage{Status: "queued", Detail: "等待翻译"}
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,library) VALUES('other','pdf','other','',1,?,?,'papers')", now(), now()); err != nil {
		t.Fatal(err)
	}
	other := p
	other.DocumentID = "other"
	if err := s.Store.saveProcessing(other); err != nil {
		t.Fatal(err)
	}
	first, err := s.Store.nextProcessing("translating")
	if err != nil || first.DocumentID != "doc" {
		t.Fatal(first, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.activeProcessing = map[string]*documentTask{"doc": {cancel: cancel}}
	response := request(t, s, "POST", "/api/documents/other/assistance", strings.NewReader(`{"action":"prioritize"}`))
	if response.Code != 200 || ctx.Err() == nil {
		t.Fatal("priority did not interrupt the old translation", response.Code)
	}
	first, err = s.Store.nextProcessing("translating")
	if err != nil || first.DocumentID != "other" {
		t.Fatal("queue order ignored saved priority", first, err)
	}
	other.Enabled = false
	if err := s.Store.saveProcessing(other); err != nil {
		t.Fatal(err)
	}
	response = request(t, s, "POST", "/api/documents/other/assistance", strings.NewReader(`{"action":"prioritize"}`))
	if response.Code != 409 {
		t.Fatal("paused translation was prioritized", response.Code)
	}
}
func TestAssistancePauseSurvivesRestart(t *testing.T) {
	s, p := processingFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got Processing
	p = assistanceRequest(t, s, "start")
	if !p.Enabled || p.Status != "queued" {
		t.Fatalf("wrong scope: %+v", p)
	}
	p.Phase = "translating"
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	p = assistanceRequest(t, s, "pause")
	s.StartProcessing(ctx)()
	got, _ = s.Store.processing("doc")
	if got.Enabled || got.Status != "paused" {
		t.Fatalf("pause lost on restart: %+v", got)
	}
	if r := request(t, s, "POST", "/api/documents/doc/processing", nil); r.Code != 409 {
		t.Fatal("retry enabled a paused task")
	}
}
func TestAssistanceJoinsProcessingWithoutCancellingChat(t *testing.T) {
	s, p := processingFixture(t)
	s.Token = "test-secret"
	p.Phase = "translating"
	_ = s.Store.saveProcessing(p)
	ctx, cancel := context.WithCancel(context.Background())
	task := &documentTask{cancel: cancel, done: make(chan struct{})}
	s.activeProcessing = map[string]*documentTask{"doc": task}
	chat, finish, e := s.beginDocumentTask(context.Background(), "doc")
	if e != nil {
		t.Fatal(e)
	}
	defer finish()
	go func() {
		<-ctx.Done()
		p.Status = "failed"
		p.Enabled = true
		_ = s.Store.saveProcessing(p)
		s.finishProcessingTask("doc", task)
	}()
	got := assistanceRequest(t, s, "pause")
	if got.Enabled || got.Status != "paused" {
		t.Fatalf("stale worker overwrote pause: %+v", got)
	}
	if chat.Err() != nil {
		t.Fatal("paused the independent chat")
	}
}
func TestWholeDocumentTranslationReusesCachedResults(t *testing.T) {
	s, _, m := translationFixture(t)
	m.Pages = 20
	outside := m.Blocks[0]
	outside.ID = "p9-b1"
	outside.Page = 9
	m.Blocks = append(m.Blocks, outside)
	raw, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), raw, 0600)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		input := readTranslationInput(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, paragraph := range input.Batch.Paragraphs {
			sendTranslationDelta(w, translationParagraphLine(paragraph))
		}
		finishTranslationStream(w)
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	p := assistanceRequest(t, s, "start")
	if e := s.learnPDF(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	if e := s.processTranslation(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	before := calls.Load()
	if before != 1 {
		t.Fatalf("calls=%d", before)
	}
	p = assistanceRequest(t, s, "start")
	if e := s.learnPDF(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	if e := s.processTranslation(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != before {
		t.Fatal("revisited page generated again")
	}
	items, _ := s.translations("doc", m)
	if items[len(items)-1].Status != "complete" {
		t.Fatal("whole document did not finish")
	}
	if w := request(t, s, "POST", "/api/documents/doc/translations", strings.NewReader(`{"blockId":"p9-b1"}`)); w.Code != 202 {
		t.Fatal("explicit cached paragraph request failed")
	}
}
func TestImportWithAutoTranslationDisabledDoesNotStartBackgroundAI(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	if w := request(t, s, "PUT", "/api/settings", strings.NewReader(`{"autoTranslatePDF":false}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	d := paperDoc(t, s, sample(t, "reading-notes.pdf"))
	p, e := s.Store.processing(d.ID)
	if e != nil || !p.Enabled || p.Status != "queued" || p.AutoTranslateMaxPages == nil || *p.AutoTranslateMaxPages != 0 {
		t.Fatalf("import must queue local parsing only: %+v %v", p, e)
	}
}

func TestFullProcessingIgnoresEarlierOCRWarnings(t *testing.T) {
	s, p := processingFixture(t)
	m, _ := s.readLayout("doc")
	m.Pages = 10
	m.IncompletePages = []int{1}
	m.Warnings = []string{"第 1 页无可提取文字；尚未接入 OCR，正文不完整。"}
	data, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), data, 0600)
	if e := s.learnPDF(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	if p.Incomplete || p.Warning != "" {
		t.Fatalf("obsolete no-text warning retained: %+v", p)
	}
}

func TestImportAutoTranslationPreferenceOnlyAffectsNewDocuments(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	if w := request(t, s, "PUT", "/api/settings", strings.NewReader(`{"autoTranslatePDF":false}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	first := paperDoc(t, s, sample(t, "reading-notes.pdf"))
	for _, enabled := range []bool{true, false} {
		body, _ := json.Marshal(map[string]any{"autoTranslatePDF": enabled, "appearance": "dark"})
		w := request(t, s, "PUT", "/api/settings", strings.NewReader(string(body)))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		settings := request(t, s, "GET", "/api/settings", nil)
		var saved map[string]any
		if err := json.Unmarshal(settings.Body.Bytes(), &saved); err != nil || saved["autoTranslatePDF"] != enabled || saved["appearance"] != "dark" {
			t.Fatalf("settings not persisted: %s", settings.Body.String())
		}
		content := append(sample(t, "reading-notes.pdf"), []byte("\n% preference "+string(body))...)
		next := paperDoc(t, s, content)
		p, err := s.Store.processing(next.ID)
		if err != nil || !p.Enabled || p.Status != "queued" || p.AutoTranslateMaxPages == nil || (*p.AutoTranslateMaxPages > 0) != enabled {
			t.Fatalf("preference ignored: %+v %v", p, err)
		}
		original, _ := s.Store.processing(first.ID)
		if original.AutoTranslateMaxPages == nil || *original.AutoTranslateMaxPages != 0 {
			t.Fatal("changing preference enabled an existing document")
		}
	}
	if w := request(t, s, "PUT", "/api/settings", strings.NewReader(`{"autoTranslatePDF":"true"}`)); w.Code != 400 {
		t.Fatal("non-boolean preference accepted")
	}
}

func TestPausedParallelProgressCannotReenableProcessing(t *testing.T) {
	s, p := processingFixture(t)
	p.Translating = &ProcessingStage{Status: "running"}
	p.Phase = "translating"
	p.Enabled, p.Status = false, "paused"
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	stale := p
	stale.Enabled, stale.lane, stale.Status = true, "translating", "queued"
	if err := s.Store.saveProcessing(stale); err != nil {
		t.Fatal(err)
	}
	s.wakeProcessing()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.StartProcessing(ctx)()
	got, _ := s.Store.processing("doc")
	if got.Enabled || got.Status != "paused" {
		t.Fatalf("parallel progress or recovery lost pause: %+v", got)
	}
}

func TestImportDefaultsToAutomaticTranslation(t *testing.T) {
	for _, settings := range []string{"", `{}`, `{"appearance":"dark"}`} {
		t.Run(settings, func(t *testing.T) {
			s := testServer(t)
			if settings != "" {
				if _, err := s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('neoread',?)", settings); err != nil {
					t.Fatal(err)
				}
			}
			d := paperDoc(t, s, sample(t, "reading-notes.pdf"))
			p, err := s.Store.processing(d.ID)
			if err != nil || !p.Enabled || p.Status != "queued" {
				t.Fatalf("default did not queue translation: %+v %v", p, err)
			}
		})
	}
}

func TestStartupKeepsLegacyProcessingRunnable(t *testing.T) {
	s, _ := processingFixture(t)
	legacy := `{"documentId":"doc","phase":"translating","status":"queued","detail":"等待翻译正文","translating":{"status":"queued"}}`
	if _, err := s.Store.DB.Exec("UPDATE document_processing SET status='queued',body=? WHERE document_id='doc'", legacy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.StartProcessing(ctx)()
	got, _ := s.Store.processing("doc")
	if !got.Enabled || got.Status != "queued" {
		t.Fatalf("legacy job was stranded: %+v", got)
	}
	s.Token = "test-secret"
	if r := request(t, s, "POST", "/api/documents/doc/processing", nil); r.Code != 200 {
		t.Fatalf("legacy retry: %d %s", r.Code, r.Body.String())
	}
}

// Ten pages with one 9000-character paragraph each and a formula image on page 9.
func bookFixture(t *testing.T) (*Server, Processing, layoutManifest) {
	t.Helper()
	s, p := processingFixture(t)
	s.Token = "test-secret"
	if _, err := s.Store.DB.Exec("UPDATE documents SET library='books' WHERE id='doc'"); err != nil {
		t.Fatal(err)
	}
	m := layoutManifest{Pages: 10}
	for page := 1; page <= 10; page++ {
		b := PDFBlock{ID: fmt.Sprintf("p%d-b1", page), Page: page, Label: "text", Text: strings.Repeat("Word. ", 1500)}
		b.Bounds.X, b.Bounds.Y, b.Bounds.Width, b.Bounds.Height = .1, .1, .5, .3
		m.Blocks = append(m.Blocks, b)
	}
	formula := PDFBlock{ID: "p9-b2", Page: 9, Label: "display_formula", Text: "E=mc^2", Image: "assets/p9-b2.png"}
	formula.Bounds.X, formula.Bounds.Y, formula.Bounds.Width, formula.Bounds.Height = .1, .5, .5, .1
	m.Blocks = append(m.Blocks, formula)
	dir := s.analysisDir("doc")
	if err := os.WriteFile(filepath.Join(dir, formula.Image), []byte("png"), 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return s, p, m
}

func translationStatuses(t *testing.T, s *Server, id string, m layoutManifest) map[string]string {
	t.Helper()
	items, err := s.translations(id, m)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, item := range items {
		out[item.BlockID] = item.Status
	}
	return out
}

func requestRange(t *testing.T, s *Server, from, to int) (int, translationRange) {
	t.Helper()
	w := request(t, s, "POST", "/api/documents/doc/translations/range", strings.NewReader(fmt.Sprintf(`{"fromPage":%d,"toPage":%d}`, from, to)))
	var result translationRange
	if w.Code == 202 {
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, result
}

func TestBookImportWaitsForAChapterRequest(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	w := uploadTo(t, s, "books", "book.pdf", sample(t, "reading-notes.pdf"))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var d Document
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	p, err := s.Store.processing(d.ID)
	if err != nil || !p.Enabled || p.Status != "complete" || p.Phase != "ready" {
		t.Fatalf("book import started processing: %+v %v", p, err)
	}
	if d.ClassificationStatus != "idle" {
		t.Fatal("book PDF queued classification before any text was parsed")
	}
}

// fakeLayout stands in for the processor: it records the requested pages and
// writes one 1020-character paragraph per page of an 80-page book.
func fakeLayout(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "layout.sh")
	body := `while [ $# -gt 0 ]; do case "$1" in --output) out="$2"; shift;; --from-page) from="$2"; shift;; --to-page) to="$2"; shift;; esac; shift; done
echo "$from-$to" >> "$FAKE_LAYOUT_LOG"
[ "$from" = "$FAKE_LAYOUT_FAIL_FROM" ] && exit 1
mkdir -p "$out/assets"
text=""; i=0; while [ $i -lt 60 ]; do text="${text}Sentence number. "; i=$((i+1)); done
blocks=""; pages=""; p=$from
while [ $p -le $to ]; do
  [ -n "$blocks" ] && blocks="$blocks," && pages="$pages,"
  blocks="$blocks{\"id\":\"p$p-b1\",\"page\":$p,\"label\":\"text\",\"bounds\":{\"x\":0.1,\"y\":0.1,\"width\":0.5,\"height\":0.3},\"text\":\"$text\"}"
  pages="$pages$p"
  echo "{\"event\":\"page\",\"page\":$p,\"total\":80}"
  p=$((p+1))
done
printf '{"pages":80,"model":"fake","parsedPages":[%s],"blocks":[%s],"warnings":[],"incompletePages":[]}' "$pages" "$blocks" > "$out/manifest.json"
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_NODE", "/bin/sh")
	t.Setenv("NEOREAD_PROCESSOR", script)
	t.Setenv("FAKE_LAYOUT_LOG", log)
	return log
}

func TestBookChapterParsesOnlyItsPagesThenTranslates(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	log := fakeLayout(t)
	w := uploadTo(t, s, "books", "book.pdf", testPDF(80))
	var d Document
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &d) != nil {
		t.Fatal(w.Body.String())
	}
	from, to := 1, 65
	chapter := func() translationRange {
		t.Helper()
		w := request(t, s, "POST", "/api/documents/"+d.ID+"/translations/range", strings.NewReader(fmt.Sprintf(`{"fromPage":%d,"toPage":%d}`, from, to)))
		var result translationRange
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("chapter request: %d %s", w.Code, w.Body.String())
		}
		return result
	}
	learn := func() {
		t.Helper()
		p, _ := s.Store.processing(d.ID)
		if p.Phase != "learning" || p.Status != "queued" || p.Chapter == nil {
			t.Fatalf("chapter not queued for parsing: %+v", p)
		}
		if err := s.learnPDF(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
	}
	if result := chapter(); !result.Parsing {
		t.Fatalf("unparsed chapter queued translation directly: %+v", result)
	}
	learn()
	m, err := s.readLayout(d.ID)
	if err != nil || len(m.ParsedPages) != 65 || len(m.Blocks) != 65 {
		t.Fatalf("first window: %+v %v", m.ParsedPages, err)
	}
	p, _ := s.Store.processing(d.ID)
	if p.Chapter != nil || p.Phase != "translating" || p.Status != "queued" || p.TranslationsTotal != 65 {
		t.Fatalf("parsed chapter was not queued for translation: %+v", p)
	}
	items, _ := s.translations(d.ID, m)
	for _, item := range items {
		item.Status, item.Sentences = "complete", []TranslationSentence{{Source: "x", Target: "译"}}
		if err := s.saveTranslation(d.ID, item); err != nil {
			t.Fatal(err)
		}
	}
	from, to = 61, 80
	if result := chapter(); !result.Parsing {
		t.Fatalf("continuing the chapter did not parse the next pages: %+v", result)
	}
	learn()
	m, _ = s.readLayout(d.ID)
	if len(m.ParsedPages) != 80 || len(m.Blocks) != 80 || m.Blocks[65].ID != "p66-b1" {
		t.Fatalf("second window not merged: %v", m.ParsedPages)
	}
	raw, _ := os.ReadFile(filepath.Join(s.analysisDir(d.ID), "manifest.json"))
	if !strings.Contains(string(raw), `"model": "fake"`) {
		t.Fatal("merging dropped manifest fields")
	}
	calls, _ := os.ReadFile(log)
	if string(calls) != "1-30\n31-60\n61-65\n66-80\n" {
		t.Fatalf("layout ran for %q", calls)
	}
	statuses := translationStatuses(t, s, d.ID, m)
	if statuses["p65-b1"] != "complete" || statuses["p66-b1"] != "pending" || statuses["p80-b1"] != "pending" {
		t.Fatalf("second window statuses: %v", statuses)
	}
}

func TestChapterWindowStartsAtUnfinishedPages(t *testing.T) {
	m := layoutManifest{Pages: 100, ParsedPages: []int{1, 2, 3}}
	for page := 1; page <= 3; page++ {
		m.Blocks = append(m.Blocks, PDFBlock{ID: fmt.Sprintf("p%d-b1", page), Page: page})
	}
	items := []TranslationBlock{{BlockID: "p1-b1", Status: "complete"}, {BlockID: "p2-b1", Status: "idle"}, {BlockID: "p3-b1", Status: "complete"}}
	if start, end, ok := chapterWindow(&m, items, 1, 100); !ok || start != 2 || end != 100 {
		t.Fatalf("window %d-%d %v", start, end, ok)
	}
	items[1].Status = "complete"
	if start, end, ok := chapterWindow(&m, items, 1, 100); !ok || start != 4 || end != 100 {
		t.Fatalf("window %d-%d %v", start, end, ok)
	}
	if _, _, ok := chapterWindow(&m, items, 1, 3); ok {
		t.Fatal("finished chapter still has work")
	}
	if start, end, ok := chapterWindow(nil, nil, 5, 9); !ok || start != 5 || end != 9 {
		t.Fatalf("unparsed book window %d-%d %v", start, end, ok)
	}
}

func TestBookChapterRetryReusesParsedBatches(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	log := fakeLayout(t)
	t.Setenv("FAKE_LAYOUT_FAIL_FROM", "31")
	w := uploadTo(t, s, "books", "book.pdf", testPDF(80))
	var d Document
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &d) != nil {
		t.Fatal(w.Body.String())
	}
	p, _ := s.Store.processing(d.ID)
	p.Chapter = &PageRange{1, 65}
	if err := s.learnPDF(t.Context(), &p); err == nil {
		t.Fatal("failed parser batch reported success")
	}
	m, err := s.readLayout(d.ID)
	if err != nil || len(m.ParsedPages) != 30 || p.Chapter == nil {
		t.Fatalf("lost completed parsing or chapter request: %+v %v", m, err)
	}
	t.Setenv("FAKE_LAYOUT_FAIL_FROM", "")
	if err := s.learnPDF(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if string(calls) != "1-30\n31-60\n31-60\n61-65\n" {
		t.Fatalf("retry reparsed completed pages: %s", calls)
	}
	m, _ = s.readLayout(d.ID)
	if len(m.ParsedPages) != 65 || p.TranslationsTotal != 65 || p.Chapter != nil {
		t.Fatalf("retry did not queue whole chapter: %+v", p)
	}
}

func TestBookTranslatesWholeRequestedChapterWithPaperWorkers(t *testing.T) {
	s, p, m := bookFixture(t)
	var mu sync.Mutex
	translated := []string{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := readTranslationInput(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, paragraph := range input.Batch.Paragraphs {
			mu.Lock()
			translated = append(translated, paragraph.BlockID)
			mu.Unlock()
			sendTranslationDelta(w, translationParagraphLine(paragraph))
		}
		finishTranslationStream(w)
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)

	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != "complete" || p.TranslationsTotal != 0 {
		t.Fatalf("book queued translation after layout: %+v", p)
	}
	for id, status := range translationStatuses(t, s, "doc", m) {
		if status != "idle" {
			t.Fatalf("%s is %s before any request", id, status)
		}
	}
	if w := request(t, s, "POST", "/api/documents/doc/translations", strings.NewReader(`{}`)); w.Code != 400 {
		t.Fatal("book accepted a whole-document translation request")
	}
	if code, _ := requestRange(t, s, 0, 3); code != 400 {
		t.Fatal("accepted page 0")
	}
	if code, _ := requestRange(t, s, 4, 11); code != 400 {
		t.Fatal("accepted a page past the end")
	}

	code, result := requestRange(t, s, 2, 6)
	if code != 202 || result.Queued != 5 || result.Characters != 5*8999 || result.NextPage != 0 {
		t.Fatalf("whole chapter not queued: %d %+v", code, result)
	}
	p, _ = s.Store.processing("doc")
	if p.Status != "queued" || p.Phase != "translating" {
		t.Fatalf("range request did not queue translation: %+v", p)
	}
	if err := s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	sort.Strings(translated)
	if strings.Join(translated, ",") != "p2-b1,p3-b1,p4-b1,p5-b1,p6-b1" {
		t.Fatalf("translated %v", translated)
	}
	statuses := translationStatuses(t, s, "doc", m)
	for id, want := range map[string]string{"p1-b1": "idle", "p2-b1": "complete", "p5-b1": "complete", "p6-b1": "complete", "p7-b1": "idle", "p9-b2": "idle"} {
		if statuses[id] != want {
			t.Fatalf("%s is %s, want %s", id, statuses[id], want)
		}
	}
	p, _ = s.Store.processing("doc")
	if p.Status != "complete" || p.TranslationsDone != 5 || p.TranslationsTotal != 5 {
		t.Fatalf("progress counted unrequested paragraphs: %+v", p)
	}

	code, result = requestRange(t, s, 2, 6)
	if code != 202 || result.Queued != 0 || result.NextPage != 0 {
		t.Fatalf("continuing the chapter: %d %+v", code, result)
	}
}

func TestBookRangeRequestResumesPausedProcessing(t *testing.T) {
	s, p, _ := bookFixture(t)
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if got := assistanceRequest(t, s, "pause"); got.Enabled {
		t.Fatal("pause did not disable processing")
	}
	if code, result := requestRange(t, s, 1, 1); code != 202 || result.Queued != 1 {
		t.Fatalf("range request: %d %+v", code, result)
	}
	p, _ = s.Store.processing("doc")
	if !p.Enabled || p.Status != "queued" {
		t.Fatalf("explicit chapter request stayed paused: %+v", p)
	}
	if got := assistanceRequest(t, s, "pause"); got.Enabled {
		t.Fatal("pause did not disable queued work")
	}
	if code, result := requestRange(t, s, 1, 1); code != 202 || result.Queued != 0 {
		t.Fatalf("pending chapter was duplicated: %d %+v", code, result)
	}
	p, _ = s.Store.processing("doc")
	if !p.Enabled || p.Status != "queued" {
		t.Fatalf("pending chapter did not resume: %+v", p)
	}
}

func TestPaperTranslationThreshold(t *testing.T) {
	for _, tc := range []struct {
		settings string
		pages    int
		manual   bool
	}{
		{"", 50, false}, {"", 51, true},
		{`{"autoTranslateMaxPages":0}`, 1, true},
		{`{"autoTranslateMaxPages":10}`, 10, false},
		{`{"autoTranslateMaxPages":10}`, 11, true},
		{`{"autoTranslateMaxPages":100}`, 100, false},
		{`{"autoTranslateMaxPages":100}`, 101, true},
		{`{"autoTranslateMaxPages":150}`, 150, false},
		{`{"autoTranslateMaxPages":150}`, 151, true},
		{`{"autoTranslatePDF":false}`, 1, true},
		{`{"autoTranslatePDF":false,"autoTranslateMaxPages":50}`, 2, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.settings, tc.pages), func(t *testing.T) {
			s, _ := processingFixture(t)
			p := importedProcessing("doc", "papers", tc.settings)
			if !p.Enabled || p.Status != "queued" {
				t.Fatalf("local parsing not queued: %+v", p)
			}
			p.StartedAt = now()
			block := PDFBlock{ID: "p1-b1", Page: 1, Label: "text", Text: "Original text."}
			block.Bounds.Width, block.Bounds.Height = .5, .5
			data, _ := json.Marshal(layoutManifest{Pages: tc.pages, Blocks: []PDFBlock{block}})
			if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.saveProcessing(p); err != nil {
				t.Fatal(err)
			}
			if err := s.learnPDF(context.Background(), &p); err != nil {
				t.Fatal(err)
			}
			if p.ManualTranslation != tc.manual || p.PagesDone != tc.pages {
				t.Fatalf("parsed state: %+v", p)
			}
			if !tc.manual {
				if !p.Enabled || p.Translating.Status != "queued" {
					t.Fatalf("translation not queued: %+v", p)
				}
				return
			}
			if p.Enabled || p.Status != "paused" {
				t.Fatalf("manual paper enabled: %+v", p)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.StartProcessing(ctx)()
			saved, err := s.Store.processing("doc")
			if err != nil || !saved.ManualTranslation || saved.Enabled {
				t.Fatalf("restart lost manual gate: %+v %v", saved, err)
			}
			if err := s.processTranslation(context.Background(), &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Translating.Status != "paused" {
				t.Fatal("manual translation was claimed")
			}
			p = assistanceRequest(t, s, "start")
			if p.ManualTranslation || p.AutoTranslateMaxPages != nil {
				t.Fatalf("explicit action did not remove gate: %+v", p)
			}
			if err := s.learnPDF(context.Background(), &p); err != nil {
				t.Fatal(err)
			}
			if !p.Enabled || p.Translating.Status != "queued" {
				t.Fatalf("manual start did not queue translation: %+v", p)
			}
		})
	}
}

func TestTranslationThresholdSettingsValidation(t *testing.T) {
	s := testServer(t)
	for _, value := range []string{"-10", "1", "15", "160", "10.5", `"50"`, "null", "true"} {
		w := request(t, s, "PUT", "/api/settings", strings.NewReader(`{"autoTranslateMaxPages":`+value+`}`))
		if w.Code != 400 {
			t.Fatalf("accepted invalid threshold %s: %s", value, w.Body.String())
		}
	}
	for value := 0; value <= 150; value += 10 {
		w := request(t, s, "PUT", "/api/settings", strings.NewReader(fmt.Sprintf(`{"autoTranslateMaxPages":%d}`, value)))
		if w.Code != 200 {
			t.Fatalf("rejected threshold %d: %s", value, w.Body.String())
		}
	}
}

// The padded files have no readable page count, so size alone decides.
func TestPaperSizeConfirmationBoundary(t *testing.T) {
	for _, tc := range []struct {
		size      int64
		confirmed bool
		library   string
		status    int
	}{
		{largePaperBytes, false, "papers", 201},
		{largePaperBytes + 1, false, "papers", 409},
		{largePaperBytes + 1, true, "papers", 201},
		{largePaperBytes + 1, false, "books", 201},
	} {
		t.Run(fmt.Sprintf("%d/%t/%s", tc.size, tc.confirmed, tc.library), func(t *testing.T) {
			s := testServer(t)
			f, err := os.CreateTemp(s.Store.Root, "size-*.pdf")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(f.Name())
			if _, err = f.Write(testPDF(1)); err != nil {
				t.Fatal(err)
			}
			if err = f.Truncate(tc.size); err != nil {
				t.Fatal(err)
			}
			f.Close()
			_, status, err := s.importFile(context.Background(), f.Name(), "large.pdf", tc.library, tc.confirmed)
			if status != tc.status {
				t.Fatalf("status=%d err=%v", status, err)
			}
			if status == 409 {
				docs, _ := s.Store.Documents()
				if len(docs) != 0 {
					t.Fatal("unconfirmed file persisted")
				}
			}
		})
	}
}

func TestZoteroLargePaperConfirmation(t *testing.T) {
	lowerLargePaperBytes(t)
	for _, library := range []string{"papers", "books"} {
		t.Run(library, func(t *testing.T) {
			dir, _ := zoteroFixture(t)
			if err := os.WriteFile(filepath.Join(dir, "storage", "PDF00001", "paper.pdf"), testPDF(160), 0600); err != nil {
				t.Fatal(err)
			}
			entry := mustScanZotero(t, dir).Entries[0]
			s := testServer(t)
			if _, err := s.importZoteroEntryTo(context.Background(), entry, "papers", false); err == nil {
				t.Fatal("large Zotero PDF imported without confirmation")
			}
			if docs, _ := s.Store.Documents(); len(docs) != 0 {
				t.Fatal("unconfirmed migration persisted")
			}
			result, err := s.importZoteroEntryTo(context.Background(), entry, library, true)
			if err != nil {
				t.Fatal(err)
			}
			d, err := s.Store.Document(result.DocumentID)
			if err != nil || d.Library != library {
				t.Fatalf("destination: %+v %v", d, err)
			}
		})
	}
}

func TestSharedImportFileSizeLimit(t *testing.T) {
	if maxImportBytes != 512<<20 {
		t.Fatal("both libraries must use a 512 MB limit")
	}
	for _, library := range []string{"papers", "books"} {
		for _, extra := range []int64{0, 1} {
			t.Run(fmt.Sprintf("%s/extra=%d", library, extra), func(t *testing.T) {
				s := testServer(t)
				file, err := os.CreateTemp(s.Store.Root, "limit-*.pdf")
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(file.Name())
				if _, err = file.Write(testPDF(1)); err != nil {
					t.Fatal(err)
				}
				if err = file.Truncate(maxImportBytes + extra); err != nil {
					t.Fatal(err)
				}
				file.Close()
				_, status, err := s.importFile(context.Background(), file.Name(), "large.pdf", library, true)
				if extra == 0 && status != 201 {
					t.Fatalf("exact limit rejected: %d %v", status, err)
				}
				if extra > 0 {
					if status != 400 || err == nil || !strings.Contains(err.Error(), "512 MB") {
						t.Fatalf("oversized file not rejected: %d %v", status, err)
					}
					if docs, _ := s.Store.Documents(); len(docs) != 0 {
						t.Fatal("oversized import persisted")
					}
				}
			})
		}
	}
}

func TestTranslationPromptSettingsPreserveAndReset(t *testing.T) {
	s, _, _ := translationFixture(t)
	value := "自然流畅，保留英文术语"
	c := AIConfig{Primary: "codex", TranslationPrompt: &value}
	for _, step := range []struct {
		prompt *string
		want   string
	}{{&value, value}, {nil, value}, {new(string), ""}} {
		c.TranslationPrompt = step.prompt
		out := request(t, s, "PUT", "/api/ai/config", strings.NewReader(jsonText(c)))
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body.String())
		}
		stored := s.aiConfig().TranslationPrompt
		if stored == nil || *stored != step.want {
			t.Fatalf("preference not preserved/reset: %v", stored)
		}
	}
	long := strings.Repeat("字", 6001)
	c.TranslationPrompt = &long
	if out := request(t, s, "PUT", "/api/ai/config", strings.NewReader(jsonText(c))); out.Code != 400 {
		t.Fatal("unbounded preference", out.Code)
	}
}

func TestTranslationPromptAppliesToTextButNotChatOrSource(t *testing.T) {
	s, p, m := translationFixture(t)
	var prompts []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		prompt := req.Messages[len(req.Messages)-1].Content
		prompts = append(prompts, prompt)
		if strings.Contains(prompt, "输入资料：") {
			writeAPIReply(w, translationLine(m.Blocks[0])+translationLine(m.Blocks[1]))
		} else {
			writeAPIReply(w, "划词译文")
		}
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	c := s.aiConfig()
	value := "自然中文，transaction 译为事务"
	c.TranslationPrompt = &value
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	if err := s.processTranslation(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	g := s.taskGenerationService(c, taskTranslation)
	if _, err := generateSelectionTranslation(t.Context(), g, selectionTranslationInput{Text: "transaction"}); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatal(prompts)
	}
	for _, prompt := range prompts {
		if !strings.Contains(prompt, value) {
			t.Fatal("preference missing", prompt)
		}
	}
	if !strings.Contains(prompts[0], "只输出 JSONL") || !strings.Contains(prompts[0], "不可信") || !strings.Contains(prompts[0], `"source":"First sentence. Second sentence."`) {
		t.Fatal("preference replaced the format/source contract")
	}
	if s.taskGenerationService(c, taskChat).translationInstructions != "" {
		t.Fatal("translation preference leaked into chat")
	}
}

func TestSelectionTranslationBoundsAndCompletion(t *testing.T) {
	g := &GenerationService{primary: "codex", connections: map[string]generationConnection{}}
	g.connections["codex"] = generationConnection{VerifiedText: true, Adapter: adapterFunc(func(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
		if !req.Input.SourceOnly || req.Input.Chat != nil {
			t.Fatal("selection translation must use an isolated source-only request")
		}
		var input selectionTranslationInput
		if err := json.Unmarshal([]byte(strings.SplitN(req.Input.Prompt, "资料：\n", 2)[1]), &input); err != nil {
			t.Fatal(err)
		}
		if input.Text != "selected" || len(strings.Fields(input.Before)) != 30 || len(strings.Fields(input.After)) != 30 || !strings.HasSuffix(input.Before, "nearest") || !strings.HasPrefix(input.After, "nearest") {
			t.Fatalf("unexpected context: %+v", input)
		}
		if err := emit(ProviderEvent{Text: "译文"}); err != nil {
			return GenerateResult{}, err
		}
		return GenerateResult{Text: "译文", FinishReason: "stop"}, nil
	})}
	text, err := generateSelectionTranslation(t.Context(), g, selectionTranslationInput{Text: "selected", Before: strings.Repeat("far ", 100) + "nearest", After: "nearest " + strings.Repeat("far ", 100)})
	if err != nil || text != "译文" {
		t.Fatalf("%q %v", text, err)
	}
}

func TestSelectionTranslationRejectsFailedPartialAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		g := &GenerationService{primary: "codex", connections: map[string]generationConnection{}}
		g.connections["codex"] = generationConnection{VerifiedText: true, Adapter: adapterFunc(func(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
			if canceled {
				<-ctx.Done()
				return GenerateResult{}, ctx.Err()
			}
			return GenerateResult{Text: "partial"}, errors.New("interrupted")
		})}
		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		}
		text, err := generateSelectionTranslation(ctx, g, selectionTranslationInput{Text: "selected"})
		cancel()
		if err == nil || text != "" {
			t.Fatalf("accepted failed result: %q %v", text, err)
		}
	}
}
func TestSelectionTranslationValidation(t *testing.T) {
	s := testServer(t)
	for _, body := range []string{`{"text":" "}`, `{"text":"` + strings.Repeat("a", 12001) + `"}`, `{"text":"hello","unexpected":true}`} {
		if w := request(t, s, "POST", "/api/translate-selection", strings.NewReader(body)); w.Code != 400 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
	if got := nearbySelectionWords(strings.Repeat("中", 80), true); got != strings.Repeat("中", 30) {
		t.Fatal(got)
	}
}
