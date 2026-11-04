package neoread

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"neoread.local/server/internal/diagnostics"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNextBookBatchFollowsChaptersFromTheReadingPosition(t *testing.T) {
	parsed := &layoutManifest{Pages: 80, ParsedPages: []int{12, 13, 14, 15}}
	for _, c := range []struct {
		name     string
		m        *layoutManifest
		total    int
		starts   []int
		reading  int
		from, to int
		ok       bool
	}{
		{"no outline uses the page bound", nil, 80, nil, 1, 1, 30, true},
		{"chapter end bounds the batch", nil, 80, []int{1, 12, 50}, 1, 1, 11, true},
		{"short chapters share a batch", nil, 80, []int{1, 3, 5, 40}, 1, 1, 4, true},
		{"the rest of the book is one batch", nil, 53, []int{2, 45}, 45, 45, 53, true},
		{"long chapter is split", nil, 80, []int{1, 12, 50}, 20, 12, 41, true},
		{"starts at the chapter being read", nil, 80, []int{1, 12, 50}, 60, 50, 79, true},
		{"skips parsed pages and stops before them", parsed, 80, []int{1, 12, 50}, 12, 16, 45, true},
		{"wraps to unparsed pages before the reader", &layoutManifest{Pages: 10, ParsedPages: []int{6, 7, 8, 9, 10}}, 10, nil, 8, 1, 5, true},
		{"unknown page count parses a first batch", nil, 0, nil, 1, 1, 30, true},
		{"complete book", &layoutManifest{Pages: 10}, 10, nil, 1, 0, 0, false},
	} {
		from, to, ok := nextBookBatch(c.m, c.total, c.starts, c.reading)
		if from != c.from || to != c.to || ok != c.ok {
			t.Errorf("%s: got %d-%d %v, want %d-%d %v", c.name, from, to, ok, c.from, c.to, c.ok)
		}
	}
}

// fakeOutline stands in for the corpus extractor's --outline mode.
func fakeOutline(t *testing.T, outline string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "corpus.sh")
	body := `while [ $# -gt 0 ]; do case "$1" in --output) out="$2"; shift;; esac; shift; done
printf '{"totalPages":80,"pages":[],"outline":%s}' '` + outline + `' > "$out"
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_CORPUS_PROCESSOR", script)
}

func uploadBook(t *testing.T, s *Server) Document {
	t.Helper()
	w := uploadTo(t, s, "books", "book.pdf", testPDF(80))
	var d Document
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &d) != nil {
		t.Fatal(w.Body.String())
	}
	if err := s.Store.queueBookLayout(); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBookImportParsesChaptersInTheBackgroundWithoutTranslating(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	log := fakeLayout(t)
	fakeOutline(t, `[{"title":"Book","page":1,"depth":0},{"title":"One","page":1,"depth":1},{"title":"Two","page":12,"depth":1},{"title":"Three","page":50,"depth":1},{"title":"3.1","page":55,"depth":2}]`)
	d := uploadBook(t, s)
	before, _ := s.Store.processing(d.ID)
	for range 10 {
		s.runBookLayoutBatch(context.Background(), d.ID)
		jobs, err := s.Store.bookLayoutJobs()
		if err != nil {
			t.Fatal(err)
		}
		if jobs[d.ID].Status == "complete" {
			break
		}
	}
	calls, _ := os.ReadFile(log)
	if string(calls) != "1-11\n12-41\n42-49\n50-79\n80-80\n" {
		t.Fatalf("layout ran for %q", calls)
	}
	m, err := s.readLayout(d.ID)
	if err != nil || len(m.ParsedPages) != 80 || len(m.Blocks) != 80 {
		t.Fatalf("book not fully parsed: %v %v", m.ParsedPages, err)
	}
	var status string
	if err = s.Store.DB.QueryRow("SELECT status FROM book_layout_jobs WHERE document_id=?", d.ID).Scan(&status); err != nil || status != "complete" {
		t.Fatalf("job status %q %v", status, err)
	}
	after, _ := s.Store.processing(d.ID)
	if after.Phase != before.Phase || after.Status != before.Status || after.Enabled != before.Enabled || after.PagesDone != 0 {
		t.Fatalf("background parsing changed translation processing: %+v -> %+v", before, after)
	}
	for id, status := range translationStatuses(t, s, d.ID, m) {
		if status != "idle" {
			t.Fatalf("background parsing queued %s as %s", id, status)
		}
	}
}

func TestBookChaptersSkipAnUnresolvedTitleBookmark(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	fakeLayout(t)
	fakeOutline(t, `[{"title":"Book","page":2,"depth":1},{"title":"One","page":2,"depth":2},{"title":"1.1","page":3,"depth":3},{"title":"Two","page":40,"depth":2}]`)
	d := uploadBook(t, s)
	if starts := s.bookChapterStarts(context.Background(), d); fmt.Sprint(starts) != "[2 40]" {
		t.Fatalf("chapter starts %v", starts)
	}
}

func TestBookLayoutProgressAndChapterRequestShareParsedPages(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	log := fakeLayout(t)
	fakeOutline(t, `[]`)
	d := uploadBook(t, s)
	s.runBookLayoutBatch(context.Background(), d.ID)
	w := request(t, s, "GET", "/api/processing", nil)
	var jobs []struct {
		DocumentID string              `json:"documentId"`
		BookLayout *bookLayoutProgress `json:"bookLayout"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil || len(jobs) != 1 || jobs[0].BookLayout == nil ||
		jobs[0].BookLayout.PagesDone != 30 || jobs[0].BookLayout.PagesTotal != 80 || jobs[0].BookLayout.Status != "queued" {
		t.Fatalf("processing list: %s", w.Body.String())
	}
	if code, result := requestRangeFor(t, s, d.ID, 1, 65); code != 202 || !result.Parsing {
		t.Fatalf("chapter request: %d %+v", code, result)
	}
	p, _ := s.Store.processing(d.ID)
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if string(calls) != "1-30\n31-60\n61-65\n" {
		t.Fatalf("chapter reparsed background pages: %q", calls)
	}
	s.runBookLayoutBatch(context.Background(), d.ID)
	calls, _ = os.ReadFile(log)
	if !strings.HasSuffix(string(calls), "66-80\n") {
		t.Fatalf("background parsing did not continue after the chapter: %q", calls)
	}
}

// An open reader's translation stream learns the idle state of new blocks.
func TestBackgroundParsingPublishesIdleBlocksToOpenReaders(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	fakeLayout(t)
	fakeOutline(t, `[]`)
	d := uploadBook(t, s)
	ch := make(chan TranslationBlock, 64)
	s.translationMu.Lock()
	s.translationSubscribers = map[string]map[chan TranslationBlock]struct{}{d.ID: {ch: {}}}
	s.translationMu.Unlock()
	s.runBookLayoutBatch(context.Background(), d.ID)
	if len(ch) != 30 {
		t.Fatalf("published %d blocks, want the 30 new ones", len(ch))
	}
	for range 30 {
		if item := <-ch; item.Status != "idle" {
			t.Fatalf("new block published as %s", item.Status)
		}
	}
	var saved int
	if err := s.Store.DB.QueryRow("SELECT COUNT(*) FROM translations WHERE document_id=?", d.ID).Scan(&saved); err != nil || saved != 0 {
		t.Fatalf("publishing saved %d translation records: %v", saved, err)
	}
}

// A short book can finish between two polls; the list must still change.
func TestFinishedBookLayoutStaysVisibleWithItsPageCount(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	fakeLayout(t)
	fakeOutline(t, `[{"title":"One","page":1,"depth":0},{"title":"Two","page":31,"depth":0},{"title":"Three","page":61,"depth":0}]`)
	d := uploadBook(t, s)
	for range 5 {
		s.runBookLayoutBatch(context.Background(), d.ID)
	}
	w := request(t, s, "GET", "/api/processing", nil)
	var jobs []struct {
		DocumentID string              `json:"documentId"`
		BookLayout *bookLayoutProgress `json:"bookLayout"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil || len(jobs) != 1 || jobs[0].BookLayout == nil ||
		*jobs[0].BookLayout != (bookLayoutProgress{Status: "complete", PagesDone: 80, PagesTotal: 80}) {
		t.Fatalf("processing list: %s", w.Body.String())
	}
}

func TestBookOutlineOutcomeIsDiagnosed(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	directory := filepath.Join(s.Store.Root, "logs")
	logs, err := diagnostics.New(directory, "outline-test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetDiagnostics(logs)
	fakeLayout(t)
	failing := filepath.Join(t.TempDir(), "corpus.sh")
	if err := os.WriteFile(failing, []byte("echo PRIVATE >&2; exit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_CORPUS_PROCESSOR", failing)
	d := uploadBook(t, s)
	if starts := s.bookChapterStarts(context.Background(), d); len(starts) != 0 {
		t.Fatalf("failed outline produced chapters %v", starts)
	}
	fakeOutline(t, `[]`)
	other := uploadTo(t, s, "books", "other.pdf", testPDF(12))
	var second Document
	if other.Code != 201 || json.Unmarshal(other.Body.Bytes(), &second) != nil {
		t.Fatal(other.Body.String())
	}
	s.bookChapterStarts(context.Background(), second)
	files, _ := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	var all strings.Builder
	for _, file := range files {
		b, _ := os.ReadFile(file)
		all.Write(b)
	}
	output := all.String()
	for _, want := range []string{`"event":"book.layout.outline"`, `"outcome":"failed"`, `"reason":"exit_failed"`, `"outcome":"complete"`, `"reason":"no_outline"`, `"durationMs"`} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %s in %s", want, output)
		}
	}
	if strings.Contains(output, "PRIVATE") {
		t.Fatal("outline stderr leaked into diagnostics")
	}
}

func TestBookLayoutFailureNamesTheChapter(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	directory := filepath.Join(s.Store.Root, "logs")
	logs, err := diagnostics.New(directory, "layout-test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetDiagnostics(logs)
	fakeLayout(t)
	fakeOutline(t, `[{"title":"PRIVATE One","page":1,"depth":0},{"title":"PRIVATE Two","page":31,"depth":0},{"title":"Three","page":61,"depth":0}]`)
	t.Setenv("FAKE_LAYOUT_FAIL_FROM", "31")
	d := uploadBook(t, s)
	s.runBookLayoutBatch(context.Background(), d.ID)
	s.runBookLayoutBatch(context.Background(), d.ID)
	files, _ := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	var all strings.Builder
	for _, file := range files {
		b, _ := os.ReadFile(file)
		all.Write(b)
	}
	var failed map[string]any
	for line := range strings.SplitSeq(all.String(), "\n") {
		if strings.Contains(line, `"event":"book.layout.batch"`) && strings.Contains(line, `"outcome":"failed"`) {
			if err := json.Unmarshal([]byte(line), &failed); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := map[string]any{"documentId": d.ID, "level": "error", "jobStatus": "failed", "phase": "layout",
		"reasonCode": "worker_failed", "errorCode": "process", "fromPage": 31.0, "toPage": 60.0,
		"chapter": 2.0, "chapterStartPage": 31.0, "chapters": 3.0}
	for key, value := range want {
		if failed[key] != value {
			t.Errorf("%s = %v, want %v in %v", key, failed[key], value, failed)
		}
	}
	if !strings.Contains(all.String(), `"outcome":"complete"`) {
		t.Error("successful batch left no terminal event")
	}
	if strings.Contains(all.String(), "PRIVATE") {
		t.Fatal("chapter titles leaked into diagnostics")
	}
}

func TestBookLayoutFailureWaitsForTheReader(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	fakeLayout(t)
	fakeOutline(t, `[]`)
	t.Setenv("FAKE_LAYOUT_FAIL_FROM", "1")
	d := uploadBook(t, s)
	s.runBookLayoutBatch(context.Background(), d.ID)
	var status string
	_ = s.Store.DB.QueryRow("SELECT status FROM book_layout_jobs WHERE document_id=?", d.ID).Scan(&status)
	if status != "failed" {
		t.Fatalf("failed parse left job %q", status)
	}
	request(t, s, "GET", "/api/documents/"+d.ID+"/blocks", nil)
	_ = s.Store.DB.QueryRow("SELECT status FROM book_layout_jobs WHERE document_id=?", d.ID).Scan(&status)
	if status != "queued" {
		t.Fatalf("opening the book did not retry parsing: %q", status)
	}
}

func requestRangeFor(t *testing.T, s *Server, id string, from, to int) (int, translationRange) {
	t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"fromPage":%d,"toPage":%d}`, from, to))
	w := request(t, s, "POST", "/api/documents/"+id+"/translations/range", body)
	var result translationRange
	if w.Code == 202 {
		_ = json.Unmarshal(w.Body.Bytes(), &result)
	}
	return w.Code, result
}

func epubWithTOCAnchor(t *testing.T) []byte {
	t.Helper()
	source := sample(t, "the-art-of-reading.epub")
	z, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if f.Name == "EPUB/nav.xhtml" {
			body = bytes.Replace(body, []byte(`href="chapter1.xhtml"`), []byte(`href="chapter1.xhtml#toc-anchor"`), 1)
		}
		if f.Name == "EPUB/chapter1.xhtml" {
			body = bytes.Replace(body, []byte("<h1"), []byte(`<h1 id="toc-anchor"`), 1)
		}
		entry, err := w.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestReparseEPUBRestoresOldCacheAnchorWithoutReplacingDocument(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "book.epub", epubWithTOCAnchor(t))
	if response.Code != 201 {
		t.Fatalf("import: %d %s", response.Code, response.Body.String())
	}
	var d Document
	if err := json.Unmarshal(response.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("INSERT INTO annotations(id,document_id,body) VALUES(?,?,?)", "saved-note", d.ID, `{"id":"saved-note"}`); err != nil {
		t.Fatal(err)
	}
	chapter := filepath.Join(s.Store.Root, "cache", d.ID, "EPUB", "chapter1.xhtml")
	clean, err := os.ReadFile(chapter)
	if err != nil || !bytes.Contains(clean, []byte(`id="toc-anchor"`)) {
		t.Fatal("fixture does not contain the TOC anchor", err)
	}
	stale := bytes.Replace(clean, []byte(`id="toc-anchor"`), []byte(`id="old-cache"`), 1)
	if err = os.WriteFile(chapter, stale, 0600); err != nil {
		t.Fatal(err)
	}
	html, _, err := s.readerChapter(d.ID, "EPUB/chapter1.xhtml")
	if err != nil || strings.Contains(html, `id="toc-anchor"`) {
		t.Fatal("old cache did not reproduce the missing anchor", err)
	}
	response = request(t, s, "POST", "/api/documents/"+d.ID+"/reparse", nil)
	if response.Code != 200 {
		t.Fatalf("reparse: %d %s", response.Code, response.Body.String())
	}
	html, _, err = s.readerChapter(d.ID, "EPUB/chapter1.xhtml")
	if err != nil || !strings.Contains(html, `id="toc-anchor"`) {
		t.Fatal("reparse did not restore the TOC anchor", err)
	}
	current, err := s.Store.Document(d.ID)
	if err != nil || current.ID != d.ID || current.Title != d.Title {
		t.Fatal("reparse replaced the document", err)
	}
	var saved int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM annotations WHERE document_id=? AND id='saved-note'", d.ID).Scan(&saved); err != nil || saved != 1 {
		t.Fatal("reparse removed an annotation", err)
	}
}

func TestFailedReparsePreservesOldEPUBCache(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "book.epub", sample(t, "the-art-of-reading.epub"))
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	manifest := filepath.Join(s.Store.Root, "cache", d.ID, "manifest.json")
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.Store.File(d), []byte("invalid EPUB"), 0600); err != nil {
		t.Fatal(err)
	}
	response = request(t, s, "POST", "/api/documents/"+d.ID+"/reparse", nil)
	if response.Code != 500 {
		t.Fatalf("expected failure: %d %s", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed reparse changed existing cache", err)
	}
}

func TestReparseRebuildsMissingEPUBCache(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "book.epub", sample(t, "the-art-of-reading.epub"))
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	if err := os.RemoveAll(filepath.Join(s.Store.Root, "cache", d.ID)); err != nil {
		t.Fatal(err)
	}
	response = request(t, s, "POST", "/api/documents/"+d.ID+"/reparse", nil)
	if response.Code != 200 {
		t.Fatalf("reparse: %d %s", response.Code, response.Body.String())
	}
	if _, err := s.readerManifest(d.ID); err != nil {
		t.Fatal("cache was not rebuilt", err)
	}
}

func TestReparseRejectsBusyDocumentWithoutStoppingItsTask(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "book.epub", sample(t, "the-art-of-reading.epub"))
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	work, finish, err := s.beginDocumentTask(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	response = request(t, s, "POST", "/api/documents/"+d.ID+"/reparse", nil)
	if response.Code != 409 || work.Err() != nil {
		t.Fatalf("active task was disrupted: %d %v", response.Code, work.Err())
	}
}

func TestReparsePDFPublishesNewLayoutAndKeepsTranslations(t *testing.T) {
	for _, library := range []string{"papers", "books"} {
		t.Run(library, func(t *testing.T) {
			s, _, old := completedTranslationFixture(t)
			prepareReparseDocument(t, s)
			if library == "books" {
				if _, err := s.Store.DB.Exec("UPDATE documents SET library='books' WHERE id='doc'"); err != nil {
					t.Fatal(err)
				}
			}
			fresh := old
			fresh.Blocks = append([]PDFBlock(nil), old.Blocks...)
			fresh.Blocks[0].Text = "Newly parsed text."
			calls := reparseFixture(t, fresh)
			response := request(t, s, "POST", "/api/documents/doc/reparse", nil)
			if response.Code != 200 {
				t.Fatalf("reparse: %d %s", response.Code, response.Body.String())
			}
			parsed, err := s.readLayout("doc")
			if err != nil || parsed.Blocks[0].Text != "Newly parsed text." {
				t.Fatal("new layout not published", err)
			}
			if data, err := os.ReadFile(calls); err != nil || string(data) != "parse\n" {
				t.Fatal("expected one full parse", err, string(data))
			}
			var count int
			if err = s.Store.DB.QueryRow("SELECT count(*) FROM translations WHERE document_id='doc'").Scan(&count); err != nil || count != 2 {
				t.Fatal("translations were deleted", count, err)
			}
			if library == "books" {
				var status string
				if err = s.Store.DB.QueryRow("SELECT status FROM book_layout_jobs WHERE document_id='doc'").Scan(&status); err != nil || status != "complete" {
					t.Fatal("book layout job not complete", status, err)
				}
			}
		})
	}
}

func TestFailedPDFReparseKeepsPublishedLayout(t *testing.T) {
	s, _, old := completedTranslationFixture(t)
	prepareReparseDocument(t, s)
	reparseFixture(t, old)
	t.Setenv("NEOREAD_REPARSE_TEST_CHILD", "fail")
	response := request(t, s, "POST", "/api/documents/doc/reparse", nil)
	if response.Code != 500 {
		t.Fatalf("expected parse failure: %d %s", response.Code, response.Body.String())
	}
	parsed, err := s.readLayout("doc")
	if err != nil || parsed.Blocks[0].Text != old.Blocks[0].Text {
		t.Fatal("failed parse replaced old layout", err)
	}
}

func TestReparseEPUBRefreshesCoverFromChangedOriginal(t *testing.T) {
	for _, hadCover := range []bool{false, true} {
		t.Run(fmt.Sprint(hadCover), func(t *testing.T) {
			s := testServer(t)
			original := sample(t, "the-art-of-reading.epub")
			if hadCover {
				original = bookEPUB(t)
			}
			response := upload(t, s, "book.epub", original)
			if response.Code != 201 {
				t.Fatal(response.Code, response.Body.String())
			}
			var d Document
			if err := json.Unmarshal(response.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			coverURL := "/pub/test-secret/" + d.ID + "/" + coverResource
			before := request(t, s, "GET", coverURL, nil)
			if hadCover && before.Code != 200 || !hadCover && before.Code != 404 {
				t.Fatal("unexpected initial cover", before.Code)
			}
			var pngData bytes.Buffer
			if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 12, 18))); err != nil {
				t.Fatal(err)
			}
			source := bookEPUB(t)
			z, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
			if err != nil {
				t.Fatal(err)
			}
			var updated bytes.Buffer
			w := zip.NewWriter(&updated)
			for _, f := range z.File {
				r, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				name := f.Name
				if name == "EPUB/images/cover.png" {
					name = "EPUB/images/new-cover.png"
					body = pngData.Bytes()
				}
				if name == "EPUB/package.opf" {
					body = bytes.ReplaceAll(body, []byte("images/cover.png"), []byte("images/new-cover.png"))
				}
				entry, err := w.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = entry.Write(body); err != nil {
					t.Fatal(err)
				}
			}
			if err = w.Close(); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(s.Store.File(d), updated.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			stale := request(t, s, "GET", coverURL, nil)
			if stale.Code != before.Code || !bytes.Equal(stale.Body.Bytes(), before.Body.Bytes()) {
				t.Fatal("fixture did not retain the old cover cache")
			}
			response = request(t, s, "POST", "/api/documents/"+d.ID+"/reparse", nil)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			after := request(t, s, "GET", coverURL, nil)
			if after.Code != 200 || !bytes.Equal(after.Body.Bytes(), pngData.Bytes()) {
				t.Fatal("reparse did not serve the new cover", after.Code)
			}
		})
	}
}

func TestReparsePDFQueuesNewSymbolTextAndFormulaWithoutDeletingOldTranslations(t *testing.T) {
	s, _, old := completedTranslationFixture(t)
	prepareReparseDocument(t, s)
	formula := old.Blocks[0]
	formula.ID, formula.Label, formula.Text, formula.Image = "p1-b3", "display_formula", "T(X)", "assets/p1-b3.png"
	old.Blocks = append(old.Blocks, formula)
	if err := atomicFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), []byte(jsonText(old))); err != nil {
		t.Fatal(err)
	}
	saved := newTranslation(formula)
	saved.Status, saved.FormulaMarkdown = "complete", "$$T(X)$$"
	if err := s.saveTranslation("doc", saved); err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.Blocks = append([]PDFBlock(nil), old.Blocks...)
	fresh.Blocks[0].Text = "We define 𝒯 as a set of transformations."
	fresh.Blocks[2].Text = "𝒯(X)"
	calls := reparseFixture(t, fresh)
	response := request(t, s, "POST", "/api/documents/doc/reparse", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	p, err := s.Store.processing("doc")
	if err != nil || p.Phase != "translating" || p.Status != "queued" || p.Translating == nil || p.Translating.Status != "queued" || p.TranslationsDone != 1 || p.TranslationsTotal != 3 || !validLayoutRun(p.LayoutRevision) {
		t.Fatalf("changed sources were not queued: %+v, %v", p, err)
	}
	parsed, err := s.readLayout("doc")
	if err != nil || parsed.RetranslationRun != p.LayoutRevision {
		t.Fatal("layout revision not published", parsed.RetranslationRun, err)
	}
	items, err := s.translations("doc", parsed)
	if err != nil || items[0].Status != "pending" || items[1].Status != "complete" || items[2].Status != "pending" {
		t.Fatal("unchanged prose was not reused", items, err)
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM translations WHERE document_id='doc' AND status='complete'").Scan(&count); err != nil || count != 3 {
		t.Fatal("historical translations were lost", count, err)
	}
	// An old app could publish the layout without updating processing. Opening the
	// translation stream in the fixed app must repair that existing state too.
	p.LayoutRevision = ""
	p.Translating = &ProcessingStage{Status: "complete"}
	p.TranslationsDone, p.TranslationsTotal = 3, 3
	aggregateProcessing(&p)
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reparsedPDFLayout(Document{ID: "doc", Type: "pdf", Library: "papers"}); err != nil {
		t.Fatal(err)
	}
	recovered, _ := s.Store.processing("doc")
	if recovered.Status != "queued" || recovered.TranslationsDone != 1 || recovered.LayoutRevision != parsed.RetranslationRun {
		t.Fatal("legacy reparse did not recover", recovered)
	}
	if _, err := s.reparsedPDFLayout(Document{ID: "doc", Type: "pdf", Library: "papers"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(calls); err != nil || string(data) != "parse\n" {
		t.Fatal("recovery reparsed the PDF", string(data), err)
	}
}

func TestReparseDoesNotEnablePausedManualOrBookTranslation(t *testing.T) {
	for _, variant := range []string{"paused", "manual", "limited", "book"} {
		t.Run(variant, func(t *testing.T) {
			s, p, old := completedTranslationFixture(t)
			prepareReparseDocument(t, s)
			switch variant {
			case "paused":
				p.Enabled = false
			case "manual":
				p.ManualTranslation = true
			case "limited":
				limit := 0
				p.AutoTranslateMaxPages = &limit
			case "book":
				if _, err := s.Store.DB.Exec("UPDATE documents SET library='books' WHERE id='doc'"); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Store.saveProcessing(p); err != nil {
				t.Fatal(err)
			}
			fresh := old
			fresh.Blocks = append([]PDFBlock(nil), old.Blocks...)
			fresh.Blocks[0].Text = "We define 𝒯 as a set of transformations."
			reparseFixture(t, fresh)
			response := request(t, s, "POST", "/api/documents/doc/reparse", nil)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			current, err := s.Store.processing("doc")
			if err != nil || current.Enabled != p.Enabled || current.ManualTranslation != p.ManualTranslation || current.Status == "queued" || current.Translating == nil || current.Translating.Status != "complete" {
				t.Fatalf("reparse changed translation consent: %+v %v", current, err)
			}
		})
	}
}

func TestServerCloseCancelsAndJoinsExclusiveReparse(t *testing.T) {
	s, _ := processingFixture(t)
	d, err := s.Store.Document("doc")
	if err != nil {
		t.Fatal(err)
	}
	work, finish, err := s.beginDocumentReparse(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			finish()
		}
	}()
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-work.Done():
	case <-time.After(time.Second):
		t.Fatal("reparse was not cancelled")
	}
	select {
	case <-closed:
		t.Fatal("server closed before reparse cleanup finished")
	default:
	}
	if _, end, err := s.beginDocumentTask(context.Background(), d.ID); err == nil {
		end()
		t.Fatal("work started during server close")
	}
	if _, end, err := s.beginDocumentReparse(context.Background(), d); err == nil {
		end()
		t.Fatal("reparse started during server close")
	}
	finish()
	finished = true
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("server did not finish closing after reparse cleanup")
	}
	if len(s.reparsingDocuments) != 0 || len(s.documentTasks) != 0 {
		t.Fatal("reparse lifecycle was retained")
	}
}

func TestPaperParsingCannotBePaused(t *testing.T) {
	for _, status := range []string{"queued", "running", "failed"} {
		t.Run(status, func(t *testing.T) {
			s, p := processingFixture(t)
			s.Token = "test-secret"
			p.Status = status
			if err := s.Store.saveProcessing(p); err != nil {
				t.Fatal(err)
			}
			w := request(t, s, "POST", "/api/documents/doc/assistance", strings.NewReader(`{"action":"pause"}`))
			if w.Code != 409 {
				t.Fatalf("mandatory parsing accepted pause: %d %s", w.Code, w.Body.String())
			}
			got, err := s.Store.processing("doc")
			if err != nil || !got.Enabled || got.Status != status || got.Phase != "learning" {
				t.Fatalf("pause changed parsing: %+v %v", got, err)
			}
		})
	}
}

func TestPaperParsingRetryPreservesManualTranslation(t *testing.T) {
	for _, action := range []string{"start", "resume"} {
		t.Run(action, func(t *testing.T) {
			s, _ := processingFixture(t)
			p := importedProcessing("doc", "papers", `{"autoTranslateMaxPages":0}`)
			p.Status = "failed"
			if err := s.Store.saveProcessing(p); err != nil {
				t.Fatal(err)
			}
			p = assistanceRequest(t, s, action)
			if !p.Enabled || p.Phase != "learning" || p.Status != "queued" || p.AutoTranslateMaxPages == nil || *p.AutoTranslateMaxPages != 0 {
				t.Fatalf("retry changed translation preference: %+v", p)
			}
			if err := s.learnPDF(context.Background(), &p); err != nil {
				t.Fatal(err)
			}
			if p.PagesDone != 2 || !p.ManualTranslation || p.Enabled {
				t.Fatalf("retry must finish parsing without enabling translation: %+v", p)
			}
		})
	}
}

func TestPaperParsingRetryReturnsSavedProcessing(t *testing.T) {
	s, p := processingFixture(t)
	p.Status = "failed"
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	// A retry may share the previous clock tick on Windows. Seed an older record
	// so this checks that the response contains the newly persisted timestamp.
	if _, err := s.Store.DB.Exec("UPDATE document_processing SET body=json_set(body,'$.updatedAt',?) WHERE document_id=?", "2000-01-01T00:00:00Z", "doc"); err != nil {
		t.Fatal(err)
	}
	failed, err := s.Store.processing("doc")
	if err != nil {
		t.Fatal(err)
	}

	returned := assistanceRequest(t, s, "resume")
	saved, err := s.Store.processing("doc")
	if err != nil {
		t.Fatal(err)
	}
	if returned.Status != "queued" || saved.Status != "queued" || returned.UpdatedAt != saved.UpdatedAt || returned.UpdatedAt == failed.UpdatedAt {
		t.Fatalf("retry returned stale state: failed=%+v returned=%+v saved=%+v", failed, returned, saved)
	}
}

func TestStartupRecoversPausedPaperParsingOnly(t *testing.T) {
	for _, tc := range []struct {
		name, library, phase   string
		retranslating, recover bool
	}{
		{"paper parsing", "papers", "learning", false, true},
		{"paper translation", "papers", "translating", false, false},
		{"book parsing", "books", "learning", false, false},
		{"retranslation", "papers", "learning", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := processingFixture(t)
			if _, err := s.Store.DB.Exec("UPDATE documents SET library=? WHERE id='doc'", tc.library); err != nil {
				t.Fatal(err)
			}
			p.Enabled, p.Status, p.Phase, p.Retranslating = false, "paused", tc.phase, tc.retranslating
			if err := s.Store.saveProcessing(p); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.StartProcessing(ctx)()
			got, err := s.Store.processing("doc")
			if err != nil || got.Enabled != tc.recover {
				t.Fatalf("wrong recovery: %+v %v", got, err)
			}
			if tc.recover {
				if got.Status != "queued" || got.AutoTranslateMaxPages == nil || *got.AutoTranslateMaxPages != 0 {
					t.Fatalf("recovery must queue parsing without translation: %+v", got)
				}
				if err := s.learnPDF(context.Background(), &got); err != nil {
					t.Fatal(err)
				}
				if got.PagesDone != 2 || !got.ManualTranslation || got.Enabled {
					t.Fatalf("recovery started translation: %+v", got)
				}
			} else if got.Status != "paused" {
				t.Fatalf("unrelated pause changed: %+v", got)
			}
		})
	}
}

func processingFixture(t *testing.T) (*Server, Processing) {
	t.Helper()
	store, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.DB.Close() })
	_, e = store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,library) VALUES('doc','pdf','test','',1,?,?,'papers')", now(), now())
	if e != nil {
		t.Fatal(e)
	}
	s := NewServer(store, "test", "")
	d, _ := store.Document("doc")
	if e = store.enqueueDocument(d); e != nil {
		t.Fatal(e)
	}
	p, _ := store.processing("doc")
	p.Enabled = true
	p.Status = "queued"
	if e = store.saveProcessing(p); e != nil {
		t.Fatal(e)
	}
	dir := s.analysisDir("doc")
	if e = os.MkdirAll(filepath.Join(dir, "assets"), 0700); e != nil {
		t.Fatal(e)
	}
	b := PDFBlock{ID: "p1-b1", Page: 1, Label: "chart", Image: "assets/p1-b1.png"}
	b.Bounds.X = .1
	b.Bounds.Y = .1
	b.Bounds.Width = .3
	b.Bounds.Height = .4
	data, _ := json.Marshal(layoutManifest{Pages: 2, Blocks: []PDFBlock{b}})
	if e = os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	return s, p
}
func TestLearningWithoutTextCompletesWithoutAttachmentWork(t *testing.T) {
	s, p := processingFixture(t)
	if e := s.learnPDF(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	if p.Phase != "ready" || p.Status != "complete" || p.PagesDone != 2 || p.CompletedAt == "" {
		t.Fatalf("wrong learning result: %+v", p)
	}
	m, e := s.readLayout("doc")
	if e != nil || len(m.Blocks) != 1 {
		t.Fatal("blocks unavailable")
	}
	if _, e = os.Stat(filepath.Join(s.analysisDir("doc"), "transcripts")); !os.IsNotExist(e) {
		t.Fatal("created an attachment transcript directory")
	}
}

// Image consolidation was removed: attachments never reach the vision provider,
// and transcripts saved by earlier versions stay untouched on disk.
func TestProcessingTranslatesWithoutConsolidatingAttachments(t *testing.T) {
	s, p := processingFixture(t)
	m, _ := s.readLayout("doc")
	m.Blocks[0].Caption = "Figure one."
	dir := s.analysisDir("doc")
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.Blocks[0].Image), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "transcripts", "p1-b1.md")
	_ = os.MkdirAll(filepath.Dir(legacy), 0700)
	if err := os.WriteFile(legacy, []byte("user-corrected transcript"), 0600); err != nil {
		t.Fatal(err)
	}
	var imageCalls atomic.Int32
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageCalls.Add(1)
		http.Error(w, "unexpected image call", http.StatusTeapot)
	}))
	defer images.Close()
	text := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, paragraph := range readTranslationInput(t, r).Batch.Paragraphs {
			sendTranslationDelta(w, translationParagraphLine(paragraph))
		}
		finishTranslationStream(w)
	}))
	defer text.Close()
	configureFormulaTest(t, s, text.URL, images.URL)
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Phase != "translating" || p.Status != "queued" {
		t.Fatalf("translation not queued after learning: %+v", p)
	}
	if err := s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != "complete" || p.Phase != "ready" || p.TranslationsDone != 1 {
		t.Fatalf("%+v", p)
	}
	if imageCalls.Load() != 0 {
		t.Fatal("attachment sent to the vision provider")
	}
	if data, _ := os.ReadFile(legacy); string(data) != "user-corrected transcript" {
		t.Fatal("legacy transcript changed")
	}
}
func TestLayoutRejectsEscapingAssetPath(t *testing.T) {
	s, _ := processingFixture(t)
	path := filepath.Join(s.analysisDir("doc"), "manifest.json")
	data, _ := os.ReadFile(path)
	var m layoutManifest
	_ = json.Unmarshal(data, &m)
	m.Blocks[0].Image = "../../outside.png"
	data, _ = json.Marshal(m)
	_ = os.WriteFile(path, data, 0600)
	if _, e := s.readLayout("doc"); e == nil {
		t.Fatal("escaping path accepted")
	}
}
func TestProcessingInterruptedStateIsQueuedOnRestart(t *testing.T) {
	s, p := processingFixture(t)
	p.Status = "running"
	_ = s.Store.saveProcessing(p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop := s.StartProcessing(ctx)
	stop()
	saved, _ := s.Store.processing("doc")
	if saved.Status != "queued" {
		t.Fatalf("lost interrupted task: %+v", saved)
	}
}
func TestLibraryLockIsExclusive(t *testing.T) {
	root := t.TempDir()
	release, e := LockLibrary(root)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := LockLibrary(root); e == nil {
		second()
		t.Fatal("second library writer admitted")
	}
	release()
	next, e := LockLibrary(root)
	if e != nil {
		t.Fatal(e)
	}
	next()
}
func TestPagesWithoutTextCompleteWithoutOCRWarnings(t *testing.T) {
	s, p := processingFixture(t)
	m := layoutManifest{Pages: 2, Blocks: []PDFBlock{}, Warnings: []string{"第 1 页无可提取文字；尚未接入 OCR，正文不完整。"}, IncompletePages: []int{1}}
	data, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), data, 0600)
	if e := s.learnPDF(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	if p.Incomplete || p.Status != "complete" || p.Warning != "" || strings.Contains(p.Detail, "OCR") {
		t.Fatalf("empty page reported as incomplete: %+v", p)
	}
}

func TestLegacyNoTextStateIsNormalizedWithoutRestartingWork(t *testing.T) {
	s, p := processingFixture(t)
	s.Token = "test-secret"
	for _, state := range []string{"complete", "paused", "failed"} {
		p.Status = state
		p.Enabled = state != "paused"
		p.Incomplete = true
		p.Detail = "部分就绪：有页面缺少可提取文字，尚未接入 OCR"
		if state != "complete" {
			p.Detail = "保留任务状态"
		}
		p.Warning = "第 1 页无可提取文字；尚未接入 OCR，正文不完整。 第 2 页有未归入版面块的文字，已保留。"
		if _, err := s.Store.DB.Exec("UPDATE document_processing SET status=?,body=? WHERE document_id='doc'", state, jsonText(p)); err != nil {
			t.Fatal(err)
		}
		got, err := s.Store.processing("doc")
		if err != nil || got.Incomplete || got.Status != state || got.Enabled != p.Enabled || strings.Contains(got.Warning, "OCR") || !strings.Contains(got.Warning, "已保留") {
			t.Fatalf("legacy state: %+v %v", got, err)
		}
		if state != "complete" && got.Detail != p.Detail {
			t.Fatal("changed failure or pause detail")
		}
		response := request(t, s, "GET", "/api/processing", nil)
		if response.Code != 200 || strings.Contains(response.Body.String(), "OCR") || strings.Contains(response.Body.String(), `"incomplete":true`) {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

func TestProcessingWakeDoesNotResetFailedTranslation(t *testing.T) {
	s, p := processingFixture(t)
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	p.Translating = &ProcessingStage{Status: "failed", Detail: "text failed"}
	aggregateProcessing(&p)
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	s.wakeProcessing()
	state, _ := s.Store.processing("doc")
	if state.Translating.Status != "failed" || state.Status != "failed" {
		t.Fatalf("wake altered failed translation: %+v", state)
	}
}
func TestProcessingRetryKeepsCompletedTranslation(t *testing.T) {
	s, p := processingFixture(t)
	s.Token = "test-secret"
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	p.Translating = &ProcessingStage{Status: "complete"}
	p.TranslationsDone = 5
	aggregateProcessing(&p)
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "POST", "/api/documents/doc/processing", nil)
	if w.Code != 200 {
		t.Fatalf("%s", w.Body.String())
	}
	state, _ := s.Store.processing("doc")
	if state.Translating.Status != "complete" || state.TranslationsDone != 5 {
		t.Fatalf("retry altered completed translation: %+v", state)
	}
}

// Rows saved while image consolidation existed must not stay blocked on it.
func TestStartupDropsLegacyConsolidationState(t *testing.T) {
	cases := []struct {
		name, phase, status, body string
		wantPhase, wantStatus     string
	}{
		{"waiting consolidation, translated", "settling", "waiting",
			`{"enabled":true,"documentId":"doc","phase":"settling","status":"waiting","settling":{"status":"waiting","detail":"请选择主 Agent"},"translating":{"status":"complete","detail":"已完成"},"assetsDone":0,"assetsTotal":3}`,
			"ready", "complete"},
		{"failed consolidation, translation running", "settling", "failed",
			`{"enabled":true,"documentId":"doc","phase":"settling","status":"failed","settling":{"status":"failed","detail":"image failed"},"translating":{"status":"running","detail":"翻译中"}}`,
			"translating", "queued"},
		{"queued before translation lane existed", "settling", "queued",
			`{"enabled":true,"documentId":"doc","phase":"settling","status":"queued","detail":"等待解析图表"}`,
			"translating", "queued"},
		{"completed before translation lane existed", "ready", "complete",
			`{"enabled":true,"documentId":"doc","phase":"ready","status":"complete","completedAt":"2026-01-01T00:00:00Z","settling":{"status":"complete"}}`,
			"ready", "complete"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := processingFixture(t)
			if _, err := s.Store.DB.Exec("UPDATE document_processing SET phase=?, status=?, body=? WHERE document_id='doc'", c.phase, c.status, c.body); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.StartProcessing(ctx)()
			var phase, status, body string
			if err := s.Store.DB.QueryRow("SELECT phase,status,body FROM document_processing WHERE document_id='doc'").Scan(&phase, &status, &body); err != nil {
				t.Fatal(err)
			}
			if phase != c.wantPhase || status != c.wantStatus || strings.Contains(body, "settling") || strings.Contains(body, "assets") {
				t.Fatalf("phase=%s status=%s body=%s", phase, status, body)
			}
			var p Processing
			if err := json.Unmarshal([]byte(body), &p); err != nil || p.Translating == nil {
				t.Fatalf("translation stage missing: %s", body)
			}
			if c.name == "completed before translation lane existed" && p.CompletedAt != "2026-01-01T00:00:00Z" {
				t.Fatalf("completion time rewritten: %s", p.CompletedAt)
			}
		})
	}
}

func TestReaderLayoutSettingsSurviveReopen(t *testing.T) {
	s := testServer(t)
	settings := `{"zoom":1.4,"readerLayout":{"navigationWidth":21,"assistantWidth":32,"navigationOpen":false,"assistantOpen":true,"sourceWidth":63,"swapped":true}}`
	w := request(t, s, "PUT", "/api/settings", strings.NewReader(settings))
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	s.Close()
	if err := s.Store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	reopened := NewServer(store, "test-secret", "")
	t.Cleanup(reopened.Close)
	w = request(t, reopened, "GET", "/api/settings", nil)
	if w.Code != 200 {
		t.Fatalf("reload: %d %s", w.Code, w.Body.String())
	}
	var got, want map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(settings), &want); err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(got)
	expected, _ := json.Marshal(want)
	if string(actual) != string(expected) {
		t.Fatalf("settings changed after reopening: %s", actual)
	}
}

func TestPageDecorationTranslation(t *testing.T) {
	for _, tc := range []struct {
		label, text string
		y, width    float64
		excluded    bool
	}{
		{"header", "Journal", .05, .8, true},
		{"footer_image", "Publisher", .93, .4, true},
		{"number", "12", .8, .04, true},
		{"text", "– 12 –", .95, .04, true},
		{"text", "iv", .95, .04, true},
		{"text", "2026", .4, .04, false},
		{"footnote", "1 Important footnote.", .95, .7, false},
		{"figure_title", "Figure 1. Results", .95, .7, false},
	} {
		t.Run(tc.label+tc.text, func(t *testing.T) {
			b := PDFBlock{Label: tc.label, Text: tc.text}
			b.Bounds.Y, b.Bounds.Width, b.Bounds.Height = tc.y, tc.width, .02
			if got := translationSource(b); (got == "") != tc.excluded {
				t.Fatalf("translationSource=%q, excluded=%v", got, tc.excluded)
			}
		})
	}
}

func TestArxivMarginIsExcludedButBodyReferencesAreRetained(t *testing.T) {
	for _, tc := range []struct {
		text             string
		x, width, height float64
		excluded         bool
	}{
		{"arXiv:2605.18915v1 [cs.CR]", .025, .02, .3, true},
		{"arXiv:2605.18915v1 [cs.CR] 12 May 2026", .95, .02, .3, true},
		{"arXiv:2605.18915v1 [cs.CR]", .15, .6, .02, false},
		{"See arXiv:2605.18915v1 [cs.CR] for details.", .025, .02, .3, false},
	} {
		b := PDFBlock{Label: "text", Text: tc.text}
		b.Bounds.X, b.Bounds.Y, b.Bounds.Width, b.Bounds.Height = tc.x, .2, tc.width, tc.height
		if got := isPDFPageDecoration(b); got != tc.excluded {
			t.Fatalf("margin=%v want=%v: %+v", got, tc.excluded, b)
		}
	}
}

// Run the actual subprocess boundary without a platform-specific shell or DLL.
func init() {
	if os.Getenv("NEOREAD_LAYOUT_TEST_CHILD") == "telemetry" {
		if os.Getenv("ORT_DISABLE_TELEMETRY") != "1" {
			os.Exit(24)
		}
		os.Exit(0)
	}
	if os.Getenv("NEOREAD_LAYOUT_TEST_CHILD") == "1" {
		fmt.Println(`{"event":"diagnostic","stage":"onnx_load","status":"start"}`)
		fmt.Println(`{"event":"diagnostic","stage":"onnx_load","status":"failed","errorCode":"ERR_DLOPEN_FAILED","errorType":"Error","errorFingerprint":"0123456789abcdef"}`)
		fmt.Println(`{"event":"diagnostic","stage":"PRIVATE_PDF_TEXT","status":"failed"}`)
		fmt.Fprintln(os.Stderr, "[crashpad_client_win.cc:868] not connected")
		fmt.Fprintln(os.Stderr, "C:\\Users\\PRIVATE\\secret.pdf PRIVATE_PDF_TEXT")
		os.Exit(23)
	}
}

func TestLayoutDisablesTelemetry(t *testing.T) {
	s := testServer(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_NODE", executable)
	t.Setenv("NEOREAD_PROCESSOR", "unused-worker")
	t.Setenv("NEOREAD_LAYOUT_TEST_CHILD", "telemetry")
	// Override even an inherited setting that enables telemetry.
	t.Setenv("ORT_DISABLE_TELEMETRY", "0")
	if err := s.runLayout(context.Background(), Document{ID: "test-document"}, filepath.Join(s.Store.Root, "work"), 0, 0, &Processing{}); err != nil {
		t.Fatalf("PDF child did not receive disabled telemetry: %v", err)
	}
}

func TestLayoutFailureDiagnostics(t *testing.T) {
	s := testServer(t)
	directory := filepath.Join(s.Store.Root, "logs")
	logs, err := diagnostics.New(directory, "layout-test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetDiagnostics(logs)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_NODE", executable)
	t.Setenv("NEOREAD_PROCESSOR", "unused-worker")
	t.Setenv("NEOREAD_LAYOUT_TEST_CHILD", "1")
	err = s.runLayout(context.Background(), Document{ID: "test-document"}, filepath.Join(s.Store.Root, "work"), 0, 0, &Processing{})
	if err == nil {
		t.Fatal("child failure was lost")
	}
	files, _ := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	var all strings.Builder
	for _, file := range files {
		b, _ := os.ReadFile(file)
		all.Write(b)
	}
	output := all.String()
	for _, want := range []string{"pdf.parse.start", "pdf.parse.spawned", "pdf.parse.stage", "pdf.parse.finish", "onnx_load", "ERR_DLOPEN_FAILED", `"exitCode":23`, "stderrFingerprint", "nativeDiagnostics", "crashpad_not_connected", `"exitCodeHex":"0x00000017"`, "runId", "test-document"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %s in %s", want, output)
		}
	}
	if strings.Contains(output, "PRIVATE") || strings.Contains(output, "secret.pdf") {
		t.Fatal("private child output leaked")
	}
}

func TestLayoutStderrBounded(t *testing.T) {
	w := &layoutStderr{}
	for i := 0; i < 100; i++ {
		_, _ = w.Write([]byte(strings.Repeat("private", 1000)))
	}
	if len(w.tail) > 16<<10 || w.total != 700000 || len(w.fingerprint()) != 16 {
		t.Fatal("stderr capture is unbounded or incomplete")
	}
}

func TestLayoutNativeDiagnosticsAcrossWrites(t *testing.T) {
	w := &layoutStderr{}
	// Deliberately split the Crashpad message across pipe reads.
	for _, part := range []string{"C:\\Users\\PRIVATE\\secret.pdf\n[crashpad_client_win.cc:868] not con", "nected\n", "Initializing session PRIVATE\n", "Session successfully initialized\n", strings.Repeat("PRIVATE", 20000), "\nFATAL ERROR: JavaScript heap out of memory"} {
		_, _ = w.Write([]byte(part))
	}
	codes := w.nativeCodes()
	for _, code := range []string{"crashpad_not_connected", "ort_session_initialize", "ort_session_ready", "node_fatal_error", "javascript_heap_oom"} {
		if !slices.Contains(codes, code) {
			t.Errorf("missing %s: %v", code, codes)
		}
	}
	if strings.Contains(strings.Join(codes, " "), "PRIVATE") || len(w.pending) > 4096 || len(w.tail) > 16<<10 {
		t.Fatal("unbounded or private diagnostic")
	}
	if !slices.Equal(codes, w.nativeCodes()) {
		t.Fatal("duplicate classification")
	}
	if layoutExitHex(4294930435) != "0xFFFF7003" || layoutExitHex(-1) != "" {
		t.Fatal("incorrect Windows exit code")
	}
}

func TestLayoutRuntimeFieldsRejectPrivateText(t *testing.T) {
	e := layoutDiagnostic{Stage: "session_create", Status: "start", NodeVersion: "24.21.0", ElectronVersion: "44.5.1", OnnxVersion: "1.30.0", RSSBytes: 123}
	if !e.valid() {
		t.Fatal("valid runtime fields rejected")
	}
	e.OnnxVersion = "PRIVATE_PATH"
	if e.valid() {
		t.Fatal("arbitrary runtime text accepted")
	}
	e.OnnxVersion = "1.30.0"
	e.RSSBytes = -1
	if e.valid() {
		t.Fatal("invalid memory accepted")
	}
}

func TestLayoutNativeSourceTrace(t *testing.T) {
	w := &layoutStderr{}
	for i := 0; i < 50; i++ {
		fmt.Fprintf(w, "[I:onnxruntime:, inference_session.cc:%d Initialize] PRIVATE C:\\Users\\private.pdf\n", i)
	}
	fmt.Fprintln(w, "[I:onnxruntime:, PRIVATE.cc:123 secret] PRIVATE")
	w.nativeCodes()
	trace := w.nativeTrace()
	if len(trace) != 32 || trace[31] != "inference_session.cc:49" || strings.Contains(strings.Join(trace, " "), "PRIVATE") {
		t.Fatalf("unsafe/unbounded trace: %v", trace)
	}
}

func TestNewLibraryExample(t *testing.T) {
	root := t.TempDir()
	s, err := OpenLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	var paper bundledPaper
	raw, _ := examplePaper.ReadFile("example-paper/paper.json")
	if err = json.Unmarshal(raw, &paper); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Document(paper.ID)
	if err != nil || doc.Title != "Attention Is All You Need" {
		t.Fatalf("document: %v %v", doc, err)
	}
	server := NewServer(s, "test", "")
	layout, err := server.readLayout(paper.ID)
	if err != nil || layout.Pages != 15 {
		t.Fatalf("layout: %v", err)
	}
	translations, err := server.translations(paper.ID, layout)
	if err != nil || len(translations) != 122 {
		t.Fatalf("translations: %d %v", len(translations), err)
	}
	for _, tr := range translations {
		if tr.Status != "complete" {
			t.Fatalf("unfinished translation: %s", tr.BlockID)
		}
	}
	for _, block := range layout.Blocks {
		if block.Image != "" {
			if _, err = os.Stat(filepath.Join(server.analysisDir(paper.ID), block.Image)); err != nil {
				t.Fatal(err)
			}
		}
	}
	p, err := s.processing(paper.ID)
	if err != nil || p.Phase != "ready" || p.Status != "complete" {
		t.Fatalf("processing: %+v %v", p, err)
	}
	var prefs string
	if err = s.DB.QueryRow("SELECT value FROM settings WHERE key='preferences:guide'").Scan(&prefs); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(prefs), []byte(paper.ID)) {
		t.Fatal("missing onboarding example")
	}
	// Restart does not duplicate or restore an example the reader deleted.
	s.DB.Close()
	s, err = OpenLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	s.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count)
	if count != 1 {
		t.Fatalf("duplicate documents: %d", count)
	}
	if _, err = s.DB.Exec("DELETE FROM documents"); err != nil {
		t.Fatal(err)
	}
	s.DB.Close()
	s, err = OpenLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	s.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count)
	if count != 0 {
		t.Fatal("deleted example was restored in an existing user's library")
	}
}

func TestExampleAfterReaderMigration(t *testing.T) {
	for _, external := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			name := fmt.Sprintf("external=%t/empty=%t", external, empty)
			t.Run(name, func(t *testing.T) {
				old, current, lib := legacyFixture(t, external)
				if empty {
					original, err := OpenStore(lib)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = original.DB.Exec("DELETE FROM documents"); err != nil {
						t.Fatal(err)
					}
					original.DB.Close()
					if err = os.Rename(filepath.Join(lib, "neoread.sqlite"), filepath.Join(lib, "reader.sqlite")); err != nil {
						t.Fatal(err)
					}
				}
				migrated, err := MigrateReaderData(old, current)
				if err != nil {
					t.Fatal(err)
				}
				s, err := OpenLibrary(migrated.Library)
				if err != nil {
					t.Fatal(err)
				}
				defer s.DB.Close()
				var title string
				if err = s.DB.QueryRow("SELECT title FROM documents").Scan(&title); err != nil {
					t.Fatal(err)
				}
				want := "Paper"
				if empty {
					want = "Attention Is All You Need"
				}
				if title != want {
					t.Fatalf("got %s, want %s", title, want)
				}
				var count int
				s.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count)
				if count != 1 {
					t.Fatalf("documents: %d", count)
				}
				var onboarded bool
				if err = s.DB.QueryRow("SELECT json_extract(value,'$.onboarded') FROM settings WHERE key='preferences:guide'").Scan(&onboarded); err != nil {
					t.Fatal(err)
				}
				if onboarded == empty {
					t.Fatalf("onboarded=%t for empty=%t", onboarded, empty)
				}
			})
		}
	}
}

func TestExampleInitializationRecovery(t *testing.T) {
	root := t.TempDir()
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	s.DB.Close()
	// Reuse a PDF published by an interrupted initialization, before DB commit.
	raw, _ := examplePaper.ReadFile("example-paper/paper.json")
	var paper bundledPaper
	json.Unmarshal(raw, &paper)
	pdf, _ := examplePaper.ReadFile("example-paper/paper.pdf")
	if err = os.WriteFile(filepath.Join(root, "papers", paper.ID+".pdf"), pdf, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = OpenLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	s.DB.Close()
	s, err = OpenLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	var count int
	s.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count)
	if count != 1 {
		t.Fatal("recovery duplicated example")
	}
	var onboarded bool
	if err = s.DB.QueryRow("SELECT json_extract(value,'$.onboarded') FROM settings WHERE key='preferences:guide'").Scan(&onboarded); err != nil {
		t.Fatal(err)
	}
	if !onboarded {
		t.Fatal("existing example triggered onboarding again")
	}
}

func TestExamplePreservesOrphanedLibraryFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "papers", "user.pdf")
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte("user content"), 0600)
	s, err := OpenLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	var count int
	s.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count)
	if count != 1 {
		t.Fatal("empty database did not receive example")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "user content" {
		t.Fatal("modified user content")
	}
}

func TestExampleRecoveryPreservesConflictingFile(t *testing.T) {
	root := t.TempDir()
	var paper bundledPaper
	raw, _ := examplePaper.ReadFile("example-paper/paper.json")
	json.Unmarshal(raw, &paper)
	path := filepath.Join(root, "papers", paper.ID+".pdf")
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte("preserve this file"), 0600)
	if s, err := OpenLibrary(root); err == nil {
		s.DB.Close()
		t.Fatal("overwrote conflicting file")
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != "preserve this file" {
		t.Fatal("user file changed")
	}
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	var count int
	s.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count)
	if count != 0 {
		t.Fatal("partial example published")
	}
}

func TestExplicitEmptyLibraryDoesNotStartFirstRunOnCreationOrRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-library")
	store, err := OpenEmptyLibrary(root, "0.10.0-experiment.6")
	if err != nil {
		t.Fatal(err)
	}
	store.DB.Close()
	for i := 0; i < 2; i++ {
		opened, err := OpenLibrary(root)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		var raw string
		if err := opened.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count); err != nil || count != 0 {
			t.Fatal("created library received demo documents", count, err)
		}
		if err := opened.DB.QueryRow("SELECT value FROM settings WHERE key='preferences:guide'").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var guide struct {
			Onboarded   bool   `json:"onboarded"`
			SeenVersion string `json:"seenVersion"`
			Example     string `json:"exampleDocumentId"`
		}
		if err := json.Unmarshal([]byte(raw), &guide); err != nil || !guide.Onboarded || guide.SeenVersion != "0.10.0-experiment.6" || guide.Example != "" {
			t.Fatal("new library would replay onboarding", guide, err)
		}
		opened.DB.Close()
	}
}

func TestExplicitEmptyLibraryCannotReplaceAnExistingDatabase(t *testing.T) {
	root := filepath.Join(t.TempDir(), "existing")
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	if _, err := store.DB.Exec("INSERT INTO settings(key,value) VALUES('preferences:guide','{\"onboarded\":false}')"); err != nil {
		t.Fatal(err)
	}
	if replacement, err := OpenEmptyLibrary(root, "new"); err == nil {
		replacement.DB.Close()
		t.Fatal("existing library was reinitialized")
	}
	var onboarded bool
	if err := store.DB.QueryRow("SELECT json_extract(value,'$.onboarded') FROM settings WHERE key='preferences:guide'").Scan(&onboarded); err != nil || onboarded {
		t.Fatal("existing preferences changed", onboarded, err)
	}
	legacy := t.TempDir()
	legacyFile := filepath.Join(legacy, "reader.sqlite")
	if err := os.WriteFile(legacyFile, []byte("legacy database"), 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenEmptyLibrary(legacy, "new"); err == nil {
		opened.DB.Close()
		t.Fatal("legacy library was reinitialized")
	}
	if body, err := os.ReadFile(legacyFile); err != nil || string(body) != "legacy database" {
		t.Fatal("legacy database changed", err)
	}
}
