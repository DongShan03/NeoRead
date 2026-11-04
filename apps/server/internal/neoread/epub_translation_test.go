package neoread

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshEPUBTranslationRebuildsCacheAndQueuesEveryParagraph(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "book.epub", sample(t, "the-art-of-reading.epub"))
	var doc Document
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &doc) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	old, err := s.readEPUBBlocks(doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := newTranslation(PDFBlock{ID: old[0].ID, Text: old[0].Text})
	stale.Status = "complete"
	stale.Sentences = []TranslationSentence{{Source: old[0].Text, Target: "旧译文。"}}
	if err := s.saveTranslation(doc.ID, stale); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reply strings.Builder
		for _, paragraph := range readTranslationInput(t, r).Batch.Paragraphs {
			reply.WriteString(translationParagraphLine(paragraph))
		}
		writeAPIReply(w, reply.String())
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	response = request(t, s, "POST", "/api/documents/"+doc.ID+"/assistance", strings.NewReader(`{"action":"retranslate","restart":true,"clearHistory":true}`))
	var p Processing
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &p) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.Store.Root, "cache", doc.ID)); !os.IsNotExist(err) {
		t.Fatal("old EPUB cache remained", err)
	}
	if err := s.processTranslation(t.Context(), &p); err != nil || p.Status != "complete" {
		t.Fatal(p, err)
	}
	blocks, err := s.readEPUBBlocks(doc.ID)
	if err != nil || len(blocks) != len(old) {
		t.Fatal("EPUB cache not rebuilt", blocks, err)
	}
	items, err := s.translations(doc.ID, blocks)
	if err != nil || len(items) != len(blocks.translationParagraphs()) {
		t.Fatal("paragraphs were omitted", items, err)
	}
	for _, item := range items {
		if item.Status != "complete" || len(item.Sentences) == 0 || item.Sentences[0].Target == "旧译文。" {
			t.Fatal("old or untranslated paragraph remained", item)
		}
	}
}

func TestEPUBTextBlocksPreserveNestedTextAndUTF16(t *testing.T) {
	source := `<html><body><div>Before <strong>😀</strong><p> Same <em>words</em>. </p>After<p>Same <em>words</em>.</p></div></body></html>`
	blocks, err := epubTextBlocks("EPUB/a.xhtml", source)
	if err != nil || len(blocks) != 4 {
		t.Fatalf("%+v %v", blocks, err)
	}
	expected := []string{"Before 😀", "Same words.", "After", "Same words."}
	ids := map[string]bool{}
	for i, b := range blocks {
		if b.Text != expected[i] || ids[b.ID] || !validEPUBLocator(b.Location.Locator, b.Location.Href) {
			t.Fatalf("invalid block: %+v", b)
		}
		ids[b.ID] = true
	}
	var loc annotationLocator
	_ = json.Unmarshal([]byte(blocks[0].Location.Locator), &loc)
	if loc.Locations.DOMRange.End.CharOffset != 2 || loc.Locations.TextRange.End != 9 {
		t.Fatalf("wrong UTF16 offsets: %+v", loc.Locations)
	}
	again, _ := epubTextBlocks("EPUB/a.xhtml", source)
	if again[1].ID != blocks[1].ID {
		t.Fatal("unstable ID")
	}
}
func TestEPUBTranslationUsesSharedQueueAndPersistsAcrossRetries(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	var doc Document
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &doc) != nil {
		t.Fatal(response.Body.String())
	}
	p, err := s.Store.processing(doc.ID)
	if err != nil || !p.Enabled || p.Status != "complete" || p.Translating == nil || p.Translating.Status != "complete" {
		t.Fatalf("EPUB did not wait for a chapter request: %+v %v", p, err)
	}
	blocks, err := s.readEPUBBlocks(doc.ID)
	if err != nil || len(blocks) == 0 {
		t.Fatalf("no blocks: %v", err)
	}
	response = request(t, s, "GET", "/api/documents/"+doc.ID+"/epub-blocks", nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), `"page":`) {
		t.Fatal(response.Body.String())
	}
	// Explicit requests select the work exercised by this retry test.
	for _, block := range blocks {
		if block.Image != "" {
			continue
		}
		response = request(t, s, "POST", "/api/documents/"+doc.ID+"/translations", strings.NewReader(`{"blockId":"`+block.ID+`"}`))
		if response.Code != 202 {
			t.Fatal(response.Body.String())
		}
	}
	p, _ = s.Store.processing(doc.ID)
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		batch := readTranslationInput(t, r).Batch
		var out strings.Builder
		for i, paragraph := range batch.Paragraphs {
			line := translationParagraphLine(paragraph)
			if i == 0 && calls == 1 {
				continue // Skipped by the first request.
			}
			if i == 0 && calls == 2 { // The automatic follow-up answers a stale source.
				line = strings.Replace(line, paragraph.SourceHash, "stale", 1)
			}
			out.WriteString(line)
		}
		writeAPIReply(w, out.String())
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	if err := s.processTranslation(context.Background(), &p); err == nil {
		t.Fatal("missing paragraph accepted")
	}
	items, err := s.translations(doc.ID, blocks)
	if err != nil || items[0].Status != "failed" || items[1].Status != "complete" {
		t.Fatalf("lost partial translations: %+v %v", items, err)
	}
	p.Status = "failed"
	_ = s.Store.saveProcessing(p)
	response = request(t, s, "POST", "/api/documents/"+doc.ID+"/translations", strings.NewReader(`{"blockId":"`+blocks[0].ID+`"}`))
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	p, _ = s.Store.processing(doc.ID)
	if err := s.processTranslation(context.Background(), &p); err != nil || p.Status != "complete" {
		t.Fatalf("retry: %+v %v", p, err)
	}
	source, err := s.readTranslationSource(doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	items, err = s.translations(doc.ID, source)
	if err != nil || items[0].Status != "complete" || calls != 3 {
		t.Fatalf("retry regenerated completed text: %d %v", calls, err)
	}
	// A source change invalidates only the affected paragraph's cached translation.
	blocks[0].Text = "Changed."
	changed, _ := s.translations(doc.ID, blocks)
	if changed[0].Status != "idle" || changed[1].Status != "complete" {
		t.Fatal("source versions mixed")
	}
	// Existing imports get an idle job without deleting translations.
	_, _ = s.Store.DB.Exec("DELETE FROM document_processing WHERE document_id=?", doc.ID)
	if err = s.Store.prepareManualEPUBProcessing(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.processing(doc.ID); err != nil {
		t.Fatal("legacy EPUB did not wait for a chapter request")
	}
}

func TestEPUBImagesPreserveReadingOrderAndStayLocal(t *testing.T) {
	blocks, err := epubTextBlocks("EPUB/chapters/a.xhtml", `<html><body><img src="../images/cover.jpg" alt="Cover"/><p>Text.</p><svg><image href="../images/chart.svg"/></svg><img src="https://example.com/remote.png"/><img src="../../../escape.png"/></body></html>`)
	if err != nil || len(blocks) != 3 {
		t.Fatalf("%+v %v", blocks, err)
	}
	if blocks[0].Image != "EPUB/images/cover.jpg" || blocks[0].Text != "Cover" || blocks[1].Text != "Text." || blocks[2].Image != "EPUB/images/chart.svg" {
		t.Fatalf("%+v", blocks)
	}
	if len(blocks.translationParagraphs()) != 1 {
		t.Fatal("images sent to text translator")
	}
}

func TestEPUBTranslatedAnnotationsPreserveSeparateRanges(t *testing.T) {
	s := testServer(t)
	var doc Document
	_ = json.Unmarshal(upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub")).Body.Bytes(), &doc)
	blocks, _ := s.readEPUBBlocks(doc.ID)
	path := "/api/documents/" + doc.ID + "/annotations"
	for i := 0; i < 2; i++ {
		var loc map[string]any
		raw, _ := json.Marshal(blocks[0].Location)
		_ = json.Unmarshal(raw, &loc)
		loc["translation"] = map[string]any{"blockId": blocks[0].ID, "sourceHash": "hash", "sentenceIndexes": []int{0}, "start": i, "end": i + 1, "ranges": []any{map[string]any{"blockId": blocks[0].ID, "sourceHash": "hash", "sentenceIndexes": []int{0}, "start": i, "end": i + 1, "quote": "译", "location": blocks[0].Location}}}
		body, _ := json.Marshal(map[string]any{"kind": "underline", "quote": "译", "note": "keep note", "location": loc, "color": "#e6b94c"})
		res := request(t, s, "POST", path, strings.NewReader(string(body)))
		if res.Code != 201 {
			t.Fatal(res.Body.String())
		}
	}
	// Opening the database also repairs old overlaps; translated selections must
	// remain distinct even when their original paragraph is identical.
	root := s.Store.Root
	s.Store.DB.Close()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s.Store = store
	res := request(t, s, "GET", path, nil)
	var notes []Annotation
	_ = json.Unmarshal(res.Body.Bytes(), &notes)
	if len(notes) != 2 {
		t.Fatalf("translated offsets merged: %s", res.Body.String())
	}
	for _, note := range notes {
		if note.Note != "keep note" || !strings.Contains(string(note.Location), `"translation"`) {
			t.Fatal("translation or note lost")
		}
	}
	if validLocation(json.RawMessage(`{"type":"epub","href":"a.xhtml","translation":{"ranges":[{"location":{"type":"epub","href":"../outside"}}]}}`), "epub") {
		t.Fatal("invalid nested source accepted")
	}
	if res = request(t, s, "DELETE", path+"/"+notes[0].ID, nil); res.Code != 204 {
		t.Fatal(res.Body.String())
	}
}

func epubRangeFixture(t *testing.T, toc []epubReaderLink) (*Server, epubTranslationDocument) {
	t.Helper()
	s, _ := processingFixture(t)
	s.Token = "test-secret"
	if _, err := s.Store.DB.Exec("UPDATE documents SET type='epub',library='books' WHERE id='doc'"); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.Store.Root, "cache", "doc")
	manifest := epubReaderManifest{ReadingOrder: []epubReaderLink{{Href: "a.xhtml", Type: "application/xhtml+xml"}, {Href: "b.xhtml", Type: "application/xhtml+xml"}}, TOC: toc}
	data, _ := json.Marshal(manifest)
	for name, text := range map[string]string{
		"manifest.json": string(data),
		"a.xhtml":       `<html><body><h1 id="one">Chapter one</h1><p>First paragraph.</p><a id="two"></a><h1>Chapter two</h1><p>Second paragraph.</p></body></html>`,
		"b.xhtml":       `<html><body><h1 id="three">Chapter three</h1><p>Third paragraph.</p></body></html>`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Store.saveProcessing(initialDocumentProcessing(Document{ID: "doc", Type: "epub"}, "")); err != nil {
		t.Fatal(err)
	}
	blocks, err := s.readEPUBBlocks("doc")
	if err != nil {
		t.Fatal(err)
	}
	return s, blocks
}

func TestEPUBChapterBoundaries(t *testing.T) {
	toc := []epubReaderLink{{Href: "a.xhtml#one"}, {Href: "a.xhtml#two"}, {Href: "b.xhtml#three"}}
	for _, test := range []struct {
		name     string
		toc      []epubReaderLink
		href     string
		block    int
		from, to int
	}{
		{"fragment", toc, "a.xhtml#two", -1, 2, 4},
		{"scrolled block", toc, "a.xhtml", 3, 2, 4},
		{"first", toc, "a.xhtml", 1, 0, 2},
		{"single parent", []epubReaderLink{{Href: "a.xhtml", Children: toc}}, "a.xhtml", 3, 2, 4},
		{"no toc", nil, "a.xhtml", 3, 0, 4},
		{"second spine", nil, "b.xhtml", 4, 4, 6},
		{"chapter spans files", []epubReaderLink{{Href: "a.xhtml#one"}}, "b.xhtml", 4, 0, 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, b := epubRangeFixture(t, test.toc)
			loc := EPUBReadingLocation{Type: "epub", Href: test.href}
			if test.block >= 0 {
				loc.BlockID = b[test.block].ID
			}
			from, to, err := s.epubTranslationChapter("doc", b, loc)
			if err != nil || from != test.from || to != test.to {
				t.Fatalf("%d:%d %v", from, to, err)
			}
		})
	}
}

func requestEPUBChapter(t *testing.T, s *Server, loc EPUBReadingLocation) (int, epubTranslationRange) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"location": loc})
	w := request(t, s, "POST", "/api/documents/doc/translations/chapter", strings.NewReader(string(data)))
	var result epubTranslationRange
	if w.Code == 202 {
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, result
}

func TestEPUBChapterRequestQueuesOnlyItsScopeAndReusesCompleted(t *testing.T) {
	s, blocks := epubRangeFixture(t, []epubReaderLink{{Href: "a.xhtml#one"}, {Href: "a.xhtml#two"}, {Href: "b.xhtml#three"}})
	items, _ := s.translations("doc", blocks)
	for _, item := range items {
		if item.Status != "idle" {
			t.Fatal("import authorized translation")
		}
	}
	completed := items[2]
	completed.Status = "complete"
	completed.Sentences = []TranslationSentence{{Source: blocks[2].Text, Target: "已完成"}}
	if err := s.saveTranslation("doc", completed); err != nil {
		t.Fatal(err)
	}
	code, res := requestEPUBChapter(t, s, EPUBReadingLocation{Type: "epub", Href: "a.xhtml#two"})
	if code != 202 || res.Queued != 1 {
		t.Fatalf("%d %+v", code, res)
	}
	items, _ = s.translations("doc", blocks)
	for i, item := range items {
		want := "idle"
		if i == 2 {
			want = "complete"
		}
		if i == 3 {
			want = "pending"
		}
		if item.Status != want {
			t.Fatalf("%d %+v", i, item)
		}
	}
	if items[2].Sentences[0].Target != "已完成" {
		t.Fatal("completed translation lost")
	}
	if r := request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(`{"action":"pause"}`)); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if r := request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(`{"action":"resume"}`)); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	p, _ := s.Store.processing("doc")
	if p.Phase != "translating" || !p.Enabled {
		t.Fatalf("EPUB entered PDF layout: %+v", p)
	}
	if r := request(t, s, "POST", "/api/documents/doc/translations", strings.NewReader(`{}`)); r.Code != 400 {
		t.Fatal("whole EPUB request allowed")
	}
	code, _ = requestEPUBChapter(t, s, EPUBReadingLocation{Type: "epub", Href: "a.xhtml", BlockID: blocks[4].ID})
	if code != 400 {
		t.Fatal("foreign chapter block accepted")
	}
	code, _ = requestEPUBChapter(t, s, EPUBReadingLocation{Type: "epub", Href: "../escape.xhtml"})
	if code != 400 {
		t.Fatal("outside path accepted")
	}
}

func TestEPUBManualMigrationPausesOldWorkOnce(t *testing.T) {
	s, blocks := epubRangeFixture(t, nil)
	items, _ := s.translations("doc", blocks)
	for i := 0; i < 3; i++ {
		items[i].Status = []string{"complete", "running", "failed"}[i]
		items[i].Sentences = []TranslationSentence{{Source: blocks[i].Text, Target: "保留"}}
		if err := s.saveTranslation("doc", items[i]); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := s.Store.processing("doc")
	p.EPUBManual = false
	p.Status = "queued"
	p.StartedAt = now()
	p.Translating = &ProcessingStage{Status: "queued"}
	_ = s.Store.saveProcessing(p)
	if err := s.Store.prepareManualEPUBProcessing(); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Store.processing("doc")
	if p.Enabled || p.Status != "paused" || !p.EPUBManual {
		t.Fatalf("old job resumed: %+v", p)
	}
	items, _ = s.translations("doc", blocks)
	if items[0].Status != "complete" || items[0].Sentences[0].Target != "保留" || items[1].Status != "idle" || items[2].Status != "idle" {
		t.Fatal(items)
	}
	code, _ := requestEPUBChapter(t, s, blocks[0].Location)
	if code != 202 {
		t.Fatal(code)
	}
	if err := s.Store.prepareManualEPUBProcessing(); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Store.processing("doc")
	if !p.Enabled || p.Status != "queued" {
		t.Fatal("migration repeated")
	}
	items, _ = s.translations("doc", blocks)
	if items[1].Status != "pending" || items[4].Status != "idle" {
		t.Fatal(items)
	}
}

func TestEPUBQueuesWholeLongChapterWithoutDuplicatingPendingWork(t *testing.T) {
	s, _ := epubRangeFixture(t, nil)
	source := `<html><body><p>` + strings.Repeat("字", 25000) + `</p><p>` + strings.Repeat("另", 25000) + `</p></body></html>`
	if err := os.WriteFile(filepath.Join(s.Store.Root, "cache", "doc", "a.xhtml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	code, first := requestEPUBChapter(t, s, EPUBReadingLocation{Type: "epub", Href: "a.xhtml"})
	if code != 202 || first.Queued != 2 || first.Characters != 50000 || first.HasMore {
		t.Fatalf("%d %+v", code, first)
	}
	code, second := requestEPUBChapter(t, s, EPUBReadingLocation{Type: "epub", Href: "a.xhtml"})
	if code != 202 || second.Queued != 0 || second.HasMore {
		t.Fatalf("%d %+v", code, second)
	}
}

func TestEPUBSourceIsStrictWhilePDFRepairsRemainAvailable(t *testing.T) {
	raw := sentencesJSON("The AGENT reads the paper. It writes a summary.")
	source := "The A GENT reads the paper. It writes a sum- mary."
	if _, err := parseTranslationSource(raw, source, false); err == nil {
		t.Fatal("EPUB source was rewritten")
	}
	if _, err := parseTranslationSource(raw, source, true); err != nil {
		t.Fatal(err)
	}
	prompt := translationPrompt(Document{Type: "epub"}, epubTranslationDocument{}, translationBatch{}, "")
	if strings.Contains(prompt, "原文由 PDF") || !strings.Contains(prompt, "禁止修正") {
		t.Fatal(prompt)
	}
}
