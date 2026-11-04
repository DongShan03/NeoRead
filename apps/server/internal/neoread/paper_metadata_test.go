package neoread

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"neoread.local/server/internal/scholar"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeIdentifiers(t *testing.T) {
	for in, want := range map[string]string{
		"10.1145/3290605.3300857":          "10.1145/3290605.3300857",
		"https://doi.org/10.1000/ABC.def":  "10.1000/ABC.def",
		"doi: 10.1000/xyz123.":             "10.1000/xyz123",
		"https://dx.doi.org/10.1000/a%2Fb": "10.1000/a/b",
	} {
		if got := NormalizeDOI(in); got != want {
			t.Errorf("DOI %q: %q", in, got)
		}
	}
	for in, want := range map[string]string{
		"2401.01234":                             "2401.01234",
		"arXiv:2401.01234v2":                     "2401.01234v2",
		"https://arxiv.org/abs/2401.01234":       "2401.01234",
		"https://arxiv.org/pdf/2401.01234v3.pdf": "2401.01234v3",
		"hep-th/9901001":                         "hep-th/9901001",
	} {
		if got := NormalizeArXiv(in); got != want {
			t.Errorf("arXiv %q: %q", in, got)
		}
	}
}
func TestManualMetadataValidation(t *testing.T) {
	var m PaperMetadata
	for _, patch := range []string{
		`{"doi":"11.1/x"}`, `{"arxiv":"abc"}`, `{"date":"May 2024"}`, `{"url":"javascript:alert(1)"}`,
		`{"itemType":"blog"}`, `{"venue":"line\nbreak"}`, `{"unknown":"x"}`, `{"creators":"Ada"}`, `{"doi":7}`,
	} {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal([]byte(patch), &raw)
		if err := m.ApplyManual(raw); err == nil {
			t.Errorf("accepted %s", patch)
		}
	}
	// A rejected patch leaves every field unchanged.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"venue":"NeurIPS","doi":"bad"}`), &raw)
	if m.ApplyManual(raw) == nil || m.Venue != "" {
		t.Fatalf("partial apply: %+v", m)
	}
	_ = json.Unmarshal([]byte(`{"doi":"https://doi.org/10.1000/X","abstract":"line one\nline two","remark":"组会讲\n第二部分","creators":[{"given":"Ada","family":"Lovelace"},{"given":"张三"},{"family":""}]}`), &raw)
	if err := m.ApplyManual(raw); err != nil {
		t.Fatal(err)
	}
	if m.DOI != "10.1000/X" || !strings.Contains(m.Abstract, "\n") || len(m.Creators) != 2 || m.Creators[1].Name != "张三" || m.Sources["doi"] != "manual" || m.Remark != "组会讲\n第二部分" {
		t.Fatalf("manual: %+v", m)
	}
	if CreatorNames(m.Creators) != "Ada Lovelace, 张三" {
		t.Fatal(CreatorNames(m.Creators))
	}
}
func TestMergeNeverReplacesManualValues(t *testing.T) {
	m := PaperMetadata{Venue: "My venue", Sources: map[string]string{"venue": "manual"}}
	found := PaperMetadata{Venue: "ICML", Date: "2024-07", DOI: "doi.org/10.5555/abc", Creators: []Creator{{Given: "A", Family: "B"}}}
	changed := m.Merge(found, "file")
	if m.Venue != "My venue" || m.Date != "2024-07" || m.DOI != "10.5555/abc" || len(m.Creators) != 1 || len(changed) != 3 {
		t.Fatalf("file merge: %+v %v", m, changed)
	}
	// Values from the file only fill gaps; a lookup refreshes automatic values.
	m.Merge(PaperMetadata{Date: "2023"}, "file")
	if m.Date != "2024-07" {
		t.Fatal("file overwrote a value")
	}
	m.Merge(PaperMetadata{Date: "2023", Venue: "Other"}, "lookup")
	if m.Date != "2023" || m.Sources["date"] != "lookup" || m.Venue != "My venue" {
		t.Fatalf("lookup merge: %+v", m)
	}
	m.Merge(PaperMetadata{DOI: "not a doi"}, "lookup")
	if m.DOI != "10.5555/abc" {
		t.Fatal("invalid lookup value accepted")
	}
}
func TestPatchPaperMetadataAndReadingStatus(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	if d.ReadingStatus != "unread" || d.Metadata.Sources != nil {
		t.Fatalf("new document: %+v", d)
	}
	if w := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"metadata":{"doi":"bad"}}`)); w.Code != 400 || !strings.Contains(w.Body.String(), "DOI") {
		t.Fatalf("bad DOI: %d %s", w.Code, w.Body.String())
	}
	w := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"title":"Attention","metadata":{"creators":[{"given":"Ashish","family":"Vaswani"}],"date":"2017","venue":"NeurIPS"}}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := s.Store.Document(d.ID)
	if got.Author != "Ashish Vaswani" || got.Metadata.Venue != "NeurIPS" || got.Metadata.Sources["title"] != "manual" || got.LastOpenedAt != d.LastOpenedAt {
		t.Fatalf("metadata: %+v", got)
	}
	if w = request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"readingStatus":"later"}`)); w.Code != 400 {
		t.Fatalf("bad status: %d", w.Code)
	}
	request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"percentage":0.2,"progress":{"type":"pdf","page":2}}`))
	if got, _ = s.Store.Document(d.ID); got.ReadingStatus != "reading" {
		t.Fatalf("progress did not start reading: %s", got.ReadingStatus)
	}
	request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"readingStatus":"done"}`))
	request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"percentage":0.3,"progress":{"type":"pdf","page":3}}`))
	if got, _ = s.Store.Document(d.ID); got.ReadingStatus != "done" {
		t.Fatalf("progress overrode a finished status: %s", got.ReadingStatus)
	}
}

type delayedMetadataBody struct {
	io.Reader
	started, resume chan struct{}
	once            sync.Once
}

func (b *delayedMetadataBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started); <-b.resume })
	return b.Reader.Read(p)
}

func TestConcurrentMetadataPatchesPreserveIndependentFields(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	body := &delayedMetadataBody{
		Reader:  strings.NewReader(`{"metadata":{"date":"2026"}}`),
		started: make(chan struct{}), resume: make(chan struct{}),
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(t, s, "PATCH", "/api/documents/"+d.ID, body) }()
	<-body.started
	other := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"metadata":{"remark":"manual note"}}`))
	close(body.resume)
	first := <-done
	if first.Code != 200 || other.Code != 200 {
		t.Fatalf("patches failed: %d %s; %d %s", first.Code, first.Body.String(), other.Code, other.Body.String())
	}
	saved, err := s.Store.Document(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Metadata.Date != "2026" || saved.Metadata.Remark != "manual note" || saved.Metadata.Sources["date"] != "manual" || saved.Metadata.Sources["remark"] != "manual" {
		t.Fatalf("a successful patch lost fields or their manual provenance: %+v", saved.Metadata)
	}
}

func TestDocumentAnnotationCounts(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	for i, kind := range []string{"note", "note", "highlight", "underline", "bookmark"} {
		if _, err := s.Store.DB.Exec(`INSERT INTO annotations VALUES(?, ?, ?)`, kind+string(rune('a'+i)), d.ID, `{"kind":"`+kind+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Store.Document(d.ID)
	if got.NoteCount != 2 || got.HighlightCount != 2 {
		t.Fatalf("counts: %+v", got)
	}
}
func TestReadingStatusMigration(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "neoread.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE documents(id TEXT PRIMARY KEY,type TEXT NOT NULL,title TEXT NOT NULL,author TEXT NOT NULL,size INTEGER NOT NULL,created_at TEXT NOT NULL,last_opened_at TEXT NOT NULL,favorite INTEGER NOT NULL DEFAULT 0,progress TEXT,percentage REAL NOT NULL DEFAULT 0);
 INSERT INTO documents VALUES('a','pdf','A','',1,'t','t',0,NULL,0),('b','pdf','B','',1,'t','t',0,NULL,0.4),('c','pdf','C','',1,'t','t',0,NULL,1);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	for id, want := range map[string]string{"a": "unread", "b": "reading", "c": "done"} {
		if d, _ := store.Document(id); d.ReadingStatus != want {
			t.Fatalf("%s: %s", id, d.ReadingStatus)
		}
	}
}

const lookupAtom = `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>http://arxiv.org/abs/1706.03762v7</id>
<published>2017-06-12T00:00:00Z</published><title>Attention Is All You Need</title><summary>Abstract text.</summary>
<author><name>Ashish Vaswani</name></author></entry></feed>`

// scholarFixture serves an arXiv entry and its PDF; it counts requests.
func scholarFixture(t *testing.T, s *Server, pageCounts ...int) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	pages := 3
	if len(pageCounts) > 0 {
		pages = pageCounts[0]
	}
	pdf := testPDF(pages)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api"):
			w.Write([]byte(lookupAtom))
		case strings.HasPrefix(r.URL.Path, "/pdf/"):
			w.Write(pdf)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c := scholar.New(maxImportBytes)
	c.AllowPrivateHosts = true
	c.ArXivAPI, c.ArXivPDF = server.URL+"/api", server.URL+"/pdf/"
	c.Crossref, c.SemanticScholar = server.URL+"/none/", server.URL+"/none/"
	s.scholar = c
	return &hits
}
func TestResolveImportsPaperWithMetadata(t *testing.T) {
	s := testServer(t)
	scholarFixture(t, s)
	w := request(t, s, "POST", "/api/documents/resolve", strings.NewReader(`{"ref":"arXiv:1706.03762"}`))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var d Document
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Library != "papers" || d.Title != "Attention Is All You Need" || d.Author != "Ashish Vaswani" || d.Metadata.ArXiv != "1706.03762" || d.Metadata.Lookup != "done" || d.Metadata.Sources["title"] != "lookup" {
		t.Fatalf("resolved: %+v", d)
	}
	// Resolving again returns the same document without duplicating it.
	if w = request(t, s, "POST", "/api/documents/resolve", strings.NewReader(`{"ref":"1706.03762"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request(t, s, "POST", "/api/documents/resolve", strings.NewReader(`{"ref":"x"}`)); w.Code != 400 {
		t.Fatalf("bad reference: %d %s", w.Code, w.Body.String())
	}
}
func TestLookupKeepsManualEdits(t *testing.T) {
	s := testServer(t)
	scholarFixture(t, s)
	w := uploadTo(t, s, "papers", "paper.pdf", testPDF(2))
	var d Document
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Metadata.Lookup != "pending" {
		t.Fatalf("file import not queued for lookup: %+v", d.Metadata)
	}
	request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"title":"My title","metadata":{"arxiv":"1706.03762","date":"2016"}}`))
	if w = request(t, s, "POST", "/api/documents/"+d.ID+"/metadata/lookup", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := s.Store.Document(d.ID)
	if got.Title != "My title" || got.Metadata.Date != "2016" || got.Metadata.Abstract != "Abstract text." || got.Author != "Ashish Vaswani" || got.Metadata.Lookup != "done" {
		t.Fatalf("lookup: %+v", got)
	}
}
func TestLookupDetectsIdentifierOnFirstPage(t *testing.T) {
	s := testServer(t)
	hits := scholarFixture(t, s)
	w := uploadTo(t, s, "papers", "paper.pdf", testPDF(2))
	var d Document
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if _, err := s.lookupDocument(context.Background(), d.ID, false); err != errNoIdentifier || hits.Load() != 0 {
		t.Fatalf("lookup without identifiers: %v hits=%d", err, hits.Load())
	}
	dir := s.analysisDir(d.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"pages":2,"metadata":{"Title":"Microsoft Word - draft.docx"},"blocks":[{"id":"p1-b1","page":1,"label":"text","bounds":{"x":0.1,"y":0.1,"width":0.5,"height":0.1},"text":"arXiv:1706.03762v7 [cs.CL] 2 Aug 2023"}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := s.lookupDocument(context.Background(), d.ID, false)
	if err != nil || got.Title != "Attention Is All You Need" || got.Metadata.ArXiv != "1706.03762v7" {
		t.Fatalf("detected lookup: %+v %v", got, err)
	}
}
func TestFileTitleOnlyFillsAPlainTitle(t *testing.T) {
	for title, want := range map[string]string{
		"Microsoft Word - paper.docx": "",
		"untitled document":           "",
		"main":                        "",
		"  Learning   Transferable Visual Models": "Learning Transferable Visual Models",
	} {
		if got := fileTitle(layoutManifest{Metadata: map[string]any{"Title": title}}); got != want {
			t.Errorf("%q: %q", title, got)
		}
	}
}
func TestAutoLookupPreference(t *testing.T) {
	s := testServer(t)
	if !s.autoLookupEnabled() {
		t.Fatal("automatic lookup should default on")
	}
	request(t, s, "PUT", "/api/preferences/library", strings.NewReader(`{"papers":{"autoLookup":false}}`))
	if s.autoLookupEnabled() {
		t.Fatal("preference ignored")
	}
}

func TestResolveLargePaperConfirmation(t *testing.T) {
	lowerLargePaperBytes(t)
	for _, library := range []string{"papers", "books"} {
		t.Run(library, func(t *testing.T) {
			s := testServer(t)
			scholarFixture(t, s, 160)
			w := request(t, s, "POST", "/api/documents/resolve", strings.NewReader(`{"ref":"1706.03762"}`))
			if w.Code != 409 || !strings.Contains(w.Body.String(), "large-paper") {
				t.Fatalf("missing confirmation: %d %s", w.Code, w.Body.String())
			}
			w = request(t, s, "POST", "/api/documents/resolve", strings.NewReader(`{"ref":"1706.03762","allowLarge":true,"library":"`+library+`"}`))
			var d Document
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &d) != nil || d.Library != library {
				t.Fatalf("confirmed destination: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCrossrefEmailPreferences(t *testing.T) {
	s := testServer(t)
	initial := s.scholarClient()
	for _, email := range []string{" reader+papers@example.com ", ""} {
		body, _ := json.Marshal(map[string]any{"crossrefEmail": email, "papers": map[string]bool{"autoLookup": false}})
		w := request(t, s, "PUT", "/api/preferences/library", strings.NewReader(string(body)))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		if got := s.scholarClient().CrossrefEmail; got != strings.TrimSpace(email) {
			t.Fatalf("email = %q", got)
		}
		w = request(t, s, "GET", "/api/preferences/library", nil)
		if !strings.Contains(w.Body.String(), `"autoLookup":false`) {
			t.Fatal("other preferences lost")
		}
	}
	if initial.CrossrefEmail != "" {
		t.Fatal("mutated an in-flight client")
	}
	for _, value := range []string{`"bad"`, `"Name <a@example.com>"`, `123`, `null`, `"a@example.com\r\nBcc:b@example.com"`} {
		w := request(t, s, "PUT", "/api/preferences/library", strings.NewReader(`{"crossrefEmail":`+value+`}`))
		if w.Code != 400 {
			t.Fatalf("accepted %s: %d", value, w.Code)
		}
	}
}

func TestRetryPaperLookup(t *testing.T) {
	t.Run("stops on success", func(t *testing.T) {
		calls := 0
		result, attempts, err := retryPaperLookup(context.Background(), time.Millisecond, func(context.Context) (scholar.Metadata, error) {
			calls++
			if calls < 3 {
				return scholar.Metadata{}, scholar.ErrNotFound
			}
			return scholar.Metadata{Title: "Found"}, nil
		})
		if err != nil || attempts != 3 || calls != 3 || result.Title != "Found" {
			t.Fatalf("result=%+v attempts=%d calls=%d err=%v", result, attempts, calls, err)
		}
	})
	t.Run("exhaustion", func(t *testing.T) {
		calls := 0
		_, attempts, err := retryPaperLookup(context.Background(), time.Millisecond, func(context.Context) (scholar.Metadata, error) {
			calls++
			return scholar.Metadata{}, scholar.ErrNotFound
		})
		if !errors.Is(err, scholar.ErrNotFound) || attempts != 10 || calls != 10 {
			t.Fatalf("attempts=%d calls=%d err=%v", attempts, calls, err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		_, attempts, err := retryPaperLookup(ctx, time.Second, func(context.Context) (scholar.Metadata, error) {
			calls++
			cancel()
			return scholar.Metadata{}, scholar.ErrNotFound
		})
		if !errors.Is(err, context.Canceled) || attempts != 1 || calls != 1 {
			t.Fatalf("attempts=%d calls=%d err=%v", attempts, calls, err)
		}
	})
}

func TestLookupByTitleOverridesIdentifiers(t *testing.T) {
	s := testServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "A Reliable Paper Title" {
			t.Errorf("expected title query, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"title":"A Reliable Paper Title","venue":"Example Journal"}]}`))
	}))
	defer upstream.Close()
	c := scholar.New(maxImportBytes)
	c.AllowPrivateHosts = true
	c.SemanticScholar = upstream.URL + "/"
	s.scholar = c
	w := uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"title":"A Reliable Paper Title","metadata":{"arxiv":"1706.03762"}}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "POST", "/api/documents/"+d.ID+"/metadata/lookup?by=title", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := s.Store.Document(d.ID)
	if got.Metadata.Venue != "Example Journal" {
		t.Fatalf("metadata: %+v", got.Metadata)
	}
}

func TestAutoPaperTitleFallback(t *testing.T) {
	s := testServer(t)
	scholarFixture(t, s)
	upstream, calls := fakeAPI(t, func(_ int, _ apiCall) string { return `{"title":"A Reliable Paper Title"}` })
	config := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, TranslationModels: map[string]string{"api": "deepseek-flash"}}
	passModels(&config, map[string]bool{"deepseek-flash": false})
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	node := filepath.Join(temp, "node")
	script := "#!/bin/sh\nprintf '%s' '{\"metadata\":{},\"pages\":[\"A Reliable Paper Title\\nAuthors and abstract\"]}' > \"$5\"\n"
	if err := os.WriteFile(node, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_NODE", node)
	t.Setenv("NEOREAD_PROCESSOR", filepath.Join(temp, "main.mjs"))
	w := uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	revision := s.connectorRevision.Load()
	got, err := s.autoPaperLookup(context.Background(), d.ID)
	if err != nil || got.Title != "random" || got.Metadata.SuggestedTitle != "A Reliable Paper Title" || got.Metadata.Lookup != "notFound" {
		t.Fatalf("got title %q state %q err %v", got.Title, got.Metadata.Lookup, err)
	}
	if s.connectorRevision.Load() <= revision {
		t.Fatal("title update did not notify library")
	}
	if len(calls()) != 1 || calls()[0].Model != "deepseek-flash" {
		t.Fatalf("unexpected calls: %+v", calls())
	}
	request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"title":"Manual title"}`))
	got, err = s.autoPaperLookup(context.Background(), d.ID)
	if err != nil || got.Title != "Manual title" || got.Metadata.SuggestedTitle != "A Reliable Paper Title" {
		t.Fatalf("manual title overwritten: %q %v", got.Title, err)
	}
}

func TestAutoPaperTitleSkipsUnconfiguredAI(t *testing.T) {
	s := testServer(t)
	temp := t.TempDir()
	node := filepath.Join(temp, "node")
	script := "#!/bin/sh\nprintf '%s' '{\"metadata\":{},\"pages\":[\"Paper text\"]}' > \"$5\"\n"
	if err := os.WriteFile(node, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_NODE", node)
	t.Setenv("NEOREAD_PROCESSOR", filepath.Join(temp, "main.mjs"))
	w := uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	got, err := s.autoPaperLookup(context.Background(), d.ID)
	if err != errNoIdentifier || got.Title != "random" {
		t.Fatalf("got %q, %v", got.Title, err)
	}
}

func TestExtractedTitleLooksUpPaperMetadata(t *testing.T) {
	s := testServer(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "A Reliable Paper Title" {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"data":[{"title":"A Reliable Paper Title","venue":"Example Journal","authors":[{"name":"Alex Smith"}]}]}`))
	}))
	defer server.Close()
	client := scholar.New(maxImportBytes)
	client.AllowPrivateHosts = true
	client.SemanticScholar = server.URL + "/"
	s.scholar = client
	w := uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	found, err := s.lookupScholarCandidates(context.Background(), d.ID, []scholar.Identifiers{{Title: "A Reliable Paper Title"}}, true)
	if err != nil || found.Venue != "Example Journal" {
		t.Fatalf("metadata lookup: %+v %v", found, err)
	}
}

func TestPaperTitleSuggestionApplyDismissAndEqual(t *testing.T) {
	s := testServer(t)
	w := uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	got, err := s.applyFound(d.ID, PaperMetadata{}, "random", "ai", "notFound")
	if err != nil || got.Metadata.SuggestedTitle != "" {
		t.Fatalf("equal title suggested: %+v %v", got, err)
	}
	got, err = s.applyFound(d.ID, PaperMetadata{}, "Suggested Title", "ai", "notFound")
	if err != nil || got.Title != "random" || got.Metadata.SuggestedTitle != "Suggested Title" {
		t.Fatalf("suggestion: %+v %v", got, err)
	}
	w = request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"metadata":{"suggestedTitle":""}}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ = s.Store.Document(d.ID)
	if got.Title != "random" || got.Metadata.SuggestedTitle != "" {
		t.Fatalf("dismiss: %+v", got)
	}
	_, _ = s.applyFound(d.ID, PaperMetadata{}, "Suggested Title", "ai", "notFound")
	w = request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"title":"Suggested Title","metadata":{"suggestedTitle":""}}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ = s.Store.Document(d.ID)
	if got.Title != "Suggested Title" || got.Metadata.SuggestedTitle != "" || got.Metadata.Sources["title"] != "manual" {
		t.Fatalf("apply: %+v", got)
	}
}

func TestPaperRefreshEligibility(t *testing.T) {
	base := Document{Library: "papers", Type: "pdf"}
	for _, tc := range []struct {
		name string
		doc  Document
		want bool
	}{
		{"eligible", base, true},
		{"venue present", Document{Library: "papers", Type: "pdf", Metadata: PaperMetadata{Venue: "Journal"}}, false},
		{"manual title", Document{Library: "papers", Type: "pdf", Metadata: PaperMetadata{Sources: map[string]string{"title": "manual"}}}, true},
		{"trashed", Document{Library: "papers", Type: "pdf", DeletedAt: "now"}, false},
		{"book", Document{Library: "books", Type: "pdf"}, false},
		{"epub", Document{Library: "papers", Type: "epub"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := paperRefreshEligible(tc.doc); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestPaperRefreshRechecksAndStopsDuplicates(t *testing.T) {
	s := testServer(t)
	w := uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "POST", "/api/diagnostics/paper-refresh", nil)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Body.String())
	}
	var p paperRefreshProgress
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Total != 1 || p.Status != "running" {
		t.Fatalf("progress %+v err %v", p, err)
	}
	w = request(t, s, "POST", "/api/diagnostics/paper-refresh", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate status %d", w.Code)
	}
	w = request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"metadata":{"venue":"Journal"}}`))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	s.runPaperRefresh(context.Background())
	w = request(t, s, "GET", "/api/diagnostics/paper-refresh", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Status != "done" || p.Processed != 1 || p.Skipped != 1 {
		t.Fatalf("progress %+v err %v", p, err)
	}
	got, err := s.Store.Document(d.ID)
	if err != nil || got.Metadata.Venue != "Journal" {
		t.Fatalf("document %+v err %v", got, err)
	}
}

func TestPaperRefreshInterruption(t *testing.T) {
	s := testServer(t)
	uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	request(t, s, "POST", "/api/diagnostics/paper-refresh", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.runPaperRefresh(ctx)
	w := request(t, s, "GET", "/api/diagnostics/paper-refresh", nil)
	var p paperRefreshProgress
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Status != "interrupted" || p.Processed != 0 {
		t.Fatalf("progress %+v err %v", p, err)
	}
}

func TestPaperRefreshManualTitleUsesItForLookup(t *testing.T) {
	s := testServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "A Manual Paper Title" {
			t.Errorf("unexpected lookup query")
		}
		_, _ = w.Write([]byte(`{"data":[{"title":"A Manual Paper Title","venue":"Example Journal"}]}`))
	}))
	defer upstream.Close()
	client := scholar.New(maxImportBytes)
	client.AllowPrivateHosts = true
	client.SemanticScholar = upstream.URL + "/"
	s.scholar = client
	w := uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"title":"A Manual Paper Title"}`))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "POST", "/api/diagnostics/paper-refresh", nil)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Body.String())
	}
	s.runPaperRefresh(context.Background())
	got, err := s.Store.Document(d.ID)
	if err != nil || got.Title != "A Manual Paper Title" || got.Metadata.Venue != "Example Journal" || got.Metadata.Sources["title"] != "manual" {
		t.Fatalf("document %+v err %v", got, err)
	}
}

func TestPaperRefreshUsesTitleSuggestion(t *testing.T) {
	s := testServer(t)
	upstream, calls := fakeAPI(t, func(_ int, _ apiCall) string { return `{"title":"A Reliable Paper Title"}` })
	config := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, TranslationModels: map[string]string{"api": "deepseek-flash"}}
	passModels(&config, map[string]bool{"deepseek-flash": false})
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	node := filepath.Join(temp, "node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\nprintf '%s' '{\"metadata\":{},\"pages\":[\"Paper title and text\"]}' > \"$5\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_NODE", node)
	t.Setenv("NEOREAD_PROCESSOR", filepath.Join(temp, "main.mjs"))
	w := uploadTo(t, s, "papers", "random.pdf", testPDF(1))
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	request(t, s, "POST", "/api/diagnostics/paper-refresh", nil)
	s.runPaperRefresh(context.Background())
	got, err := s.Store.Document(d.ID)
	if err != nil || got.Title != "random" || got.Metadata.SuggestedTitle != "A Reliable Paper Title" || len(calls()) != 1 {
		t.Fatalf("document %+v err %v calls %d", got, err, len(calls()))
	}
}

func TestJournalRankParsesCurrentMultiSubjectResponse(t *testing.T) {
	// A small subset of an actual 2026-10-12 easyScholar response for JMLR.
	raw := []byte(`{"code":200,"msg":"SUCCESS","data":{"officialRank":{"all":{"sciif":"6.8","sci":"Q1","sciUp":"计算机科学4区","sciUpSmall":"自动化与控制系统4区/计算机：人工智能4区。","sciif5":"10.5","jci":"0.94"}},"customRank":{"rankInfo":[],"rank":[]}}}`)
	result, err := parseJournalRank(raw)
	if err != nil || result.Status != "found" || len(result.Badges) != 7 {
		t.Fatalf("parse result: %+v, %v", result, err)
	}
	var small []string
	for _, badge := range result.Badges {
		if badge.Key == "sciUpSmall" {
			small = append(small, badge.Value)
		}
		if badge.Year != "" {
			t.Fatalf("invented data year: %+v", badge)
		}
	}
	if len(small) != 2 || small[0] != "自动化与控制系统4区" || small[1] != "计算机：人工智能4区" {
		t.Fatalf("subjects: %q", small)
	}
}

func TestJournalRankParsesCustomDatasetWithoutOfficialRanks(t *testing.T) {
	raw := []byte(`{"code":200,"msg":"SUCCESS","data":{"officialRank":{"all":null},"customRank":{"rankInfo":[{"uuid":"1614919989423271936","abbName":"CCF","oneRankText":"A","twoRankText":"B"}],"rank":["1614919989423271936&&&1"]}}}`)
	result, err := parseJournalRank(raw)
	if err != nil || result.Status != "found" || len(result.Badges) != 1 {
		t.Fatalf("custom result: %+v, %v", result, err)
	}
	badge := result.Badges[0]
	if badge.Key != "custom:1614919989423271936" || badge.Name != "CCF" || badge.Value != "A" {
		t.Fatalf("custom badge: %+v", badge)
	}
}

func TestJournalRankConfigDefaultsAndNeverReturnsKey(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s := NewServer(store, "test-secret", "")
	w := request(t, s, "GET", "/api/journal-ranks/config", nil)
	var config journalRankConfig
	if json.Unmarshal(w.Body.Bytes(), &config) != nil || !config.Enabled || config.HasKey || config.FilterMode != "include" || len(config.Selected) == 0 {
		t.Fatalf("bad default: %s", w.Body.String())
	}
	w = request(t, s, "PUT", "/api/journal-ranks/config", strings.NewReader(`{"enabled":true,"filterMode":"exclude","selected":["sciif","sciUpSmall"],"key":"private-test-key"}`))
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-test-key") {
		t.Fatalf("secret exposed: %d %s", w.Code, w.Body.String())
	}
	w = request(t, s, "PUT", "/api/journal-ranks/config", strings.NewReader(`{"enabled":false,"selected":["sciif"]}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	config, key, err := s.journalConfig()
	if err != nil || config.Enabled || !config.HasKey || key != "private-test-key" || config.FilterMode != "exclude" || len(config.Selected) != 1 {
		t.Fatalf("key-only settings lost: %+v %v", config, err)
	}
	w = request(t, s, "PUT", "/api/journal-ranks/config", strings.NewReader(`{"enabled":true,"filterMode":"invalid","selected":[]}`))
	if w.Code != 400 {
		t.Fatalf("invalid filter mode accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestJournalRankRequestsRespectGlobalRateLimit(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		if r.URL.Query().Get("secretKey") != "test-key" {
			t.Error("key missing")
		}
		if r.URL.Query().Get("publicationName") == "" {
			t.Error("name missing")
		}
		_, _ = w.Write([]byte(`{"code":200,"msg":"SUCCESS","data":{"officialRank":{"all":{"sci":"Q2"}}}}`))
	}))
	defer upstream.Close()
	previous := journalRankURL
	journalRankURL = upstream.URL
	defer func() { journalRankURL = previous }()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s := NewServer(store, "test-secret", "")
	var wg sync.WaitGroup
	for _, name := range []string{"Nature", "Applied Economics"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.fetchJournalRank(context.Background(), name, "test-key", 20*time.Second); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 || starts[1].Sub(starts[0]) < 500*time.Millisecond {
		t.Fatalf("request starts too close: %v", starts)
	}
}

func TestJournalRankRequestTimeoutStartsAfterRateWait(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"msg":"SUCCESS","data":{"officialRank":{"all":{"sci":"Q1"}}}}`))
	}))
	defer upstream.Close()
	previous := journalRankURL
	journalRankURL = upstream.URL
	defer func() { journalRankURL = previous }()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s := NewServer(store, "test-secret", "")
	s.journalRankLast = time.Now().Add(200 * time.Millisecond)
	result, err := s.fetchJournalRank(context.Background(), "Nature", "test-key", 500*time.Millisecond)
	if err != nil || result.Status != "found" {
		t.Fatalf("queued request spent upstream timeout before sending: %+v, %v", result, err)
	}
}

func TestJournalRankEndpointCachesByVenueAndStopsWhenDisabled(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":200,"msg":"SUCCESS","data":{"officialRank":{"all":{"sci":"Q1","sciif":"6.8"}}}}`))
	}))
	defer upstream.Close()
	previous := journalRankURL
	journalRankURL = upstream.URL
	defer func() { journalRankURL = previous }()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s := NewServer(store, "test-secret", "")
	for _, id := range []string{"a", "b"} {
		if _, err := store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,library,metadata) VALUES(?,?,?,?,?,?,?,?,?)", id, "pdf", id, "", 1, now(), now(), "papers", `{"itemType":"journal","venue":"Nature"}`); err != nil {
			t.Fatal(err)
		}
	}
	if w := request(t, s, "PUT", "/api/journal-ranks/config", strings.NewReader(`{"enabled":true,"selected":["sci","sciif"],"key":"test-key"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, id := range []string{"a", "b"} {
		w := request(t, s, "GET", "/api/documents/"+id+"/journal-rank", nil)
		var result journalRankResult
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Status != "found" || len(result.Badges) != 2 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate venue queried %d times", calls.Load())
	}
	if w := request(t, s, "PUT", "/api/journal-ranks/config", strings.NewReader(`{"enabled":false,"selected":["sci"]}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := request(t, s, "GET", "/api/documents/a/journal-rank?refresh=1", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"disabled"`) || calls.Load() != 1 {
		t.Fatalf("disabled feature queried provider: %d %s", w.Code, w.Body.String())
	}
}

func TestJournalRankLive(t *testing.T) {
	key := os.Getenv("EASYSCHOLAR_SECRET_KEY")
	if key == "" {
		t.Skip("set EASYSCHOLAR_SECRET_KEY for a small live contract check")
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s := NewServer(store, "test-secret", "")
	for _, name := range []string{"Nature", "Journal of Machine Learning Research"} {
		result, err := s.fetchJournalRank(context.Background(), name, key, 20*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.Status != "found" || len(result.Badges) < 4 {
			t.Fatalf("%s: incomplete result: %+v", name, result)
		}
	}
}

func TestOrganizationMigrationNormalizesLegacyZoteroJSON(t *testing.T) {
	s := testServer(t)
	ids := uploadPapers(t, s, 5)
	metadata := `{ "doi": "10.1000/example", "sources": { "doi": "manual" } }`
	tags := `[ "研究", "ML" ]`
	folders := `[ "AI/Security" ]`
	before := make([]Document, len(ids))
	for i, id := range ids {
		// Include already-correct TEXT, mixed storage types, and a trashed row.
		var metaValue, tagsValue, foldersValue any = metadata, tags, folders
		if i == 1 || i == 4 {
			metaValue = []byte(metadata)
		}
		if i == 2 || i == 4 {
			tagsValue = []byte(tags)
		}
		if i == 3 || i == 4 {
			foldersValue = []byte(folders)
		}
		deletedAt := ""
		if i == 4 {
			deletedAt = now()
		}
		if _, err := s.Store.DB.Exec(`UPDATE documents SET metadata=?,tags=?,folders=?,deleted_at=?,favorite=1,reading_status='reading',percentage=0.5,progress='{"type":"pdf","page":1}' WHERE id=?`, metaValue, tagsValue, foldersValue, deletedAt, id); err != nil {
			t.Fatal(err)
		}
		var err error
		before[i], err = s.Store.Document(id)
		if err != nil {
			t.Fatal(err)
		}
	}
	root := s.Store.Root
	s.Close()
	if err := s.Store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening twice checks that the repair is idempotent.
	for range 2 {
		store, err := OpenStore(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.DB.Close() })
		for i, id := range ids {
			var metaType, tagsType, foldersType, gotMeta, gotTags, gotFolders string
			if err := store.DB.QueryRow(`SELECT typeof(metadata),typeof(tags),typeof(folders),metadata,tags,folders FROM documents WHERE id=?`, id).Scan(&metaType, &tagsType, &foldersType, &gotMeta, &gotTags, &gotFolders); err != nil {
				t.Fatal(err)
			}
			if metaType != "text" || tagsType != "text" || foldersType != "text" {
				t.Fatalf("document %d storage types: %s/%s/%s", i, metaType, tagsType, foldersType)
			}
			if gotMeta != metadata || gotTags != tags || gotFolders != folders {
				t.Fatalf("document %d JSON changed during migration", i)
			}
			after, err := store.Document(id)
			if err != nil || !reflect.DeepEqual(before[i], after) {
				t.Fatalf("document %d changed during migration: %+v %v", i, after, err)
			}
		}
		if err := store.DB.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func organizationDoc(t *testing.T, s *Server) Document {
	t.Helper()
	w := upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf"))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}
func TestOrganizationMigrationPreservesLibrary(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "neoread.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE documents(id TEXT PRIMARY KEY,type TEXT NOT NULL,title TEXT NOT NULL,author TEXT NOT NULL,size INTEGER NOT NULL,created_at TEXT NOT NULL,last_opened_at TEXT NOT NULL,favorite INTEGER NOT NULL DEFAULT 0,progress TEXT,percentage REAL NOT NULL DEFAULT 0);
 INSERT INTO documents VALUES('old','epub','Existing','Author',123,'before','before',1,'{"type":"epub","href":"chapter.xhtml"}',0.5);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.Document("old")
	if err != nil {
		t.Fatal(err)
	}
	if d.Category != "book" || d.ClassificationStatus != "pending" || len(d.Tags) != 0 || !d.Favorite || d.Percentage != .5 || !strings.Contains(string(d.Progress), "chapter.xhtml") {
		t.Fatalf("migration lost data: %+v", d)
	}
	_, err = store.DB.Exec("UPDATE documents SET category='paper',category_source='manual',tags='[\"Security\"]' WHERE id='old'")
	if err != nil {
		t.Fatal(err)
	}
	store.DB.Close()
	store, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	d, err = store.Document("old")
	if err != nil || d.Category != "paper" || len(d.Tags) != 1 {
		t.Fatalf("reopen: %+v %v", d, err)
	}
}
func TestOrganizationPatchValidationAndIsolation(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	for _, body := range []string{`{"category":null}`, `{"category":"pdf"}`, `{"category":""}`, `{"tags":null}`, `{"tags":[1]}`, `{"tags":[""]}`, `{"tags":["line\nbreak"]}`} {
		w := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(body))
		if w.Code != 400 {
			t.Fatalf("accepted %s: %d", body, w.Code)
		}
	}
	w := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"category":"paper","tags":[" Security ","security","论文笔记"]}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := s.Store.Document(d.ID)
	if got.Category != "paper" || got.CategorySource != "manual" || got.ClassificationStatus != "done" || len(got.Tags) != 2 || got.Tags[0] != "Security" || got.LastOpenedAt != d.LastOpenedAt {
		t.Fatalf("patch: %+v", got)
	}
	request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"percentage":0.3,"progress":{"type":"pdf","page":2}}`))
	w = request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"tags":[]}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ = s.Store.Document(d.ID)
	if got.CategorySource != "manual" || got.Category != "paper" || len(got.Tags) != 0 || got.Percentage != .3 {
		t.Fatalf("unrelated metadata lost: %+v", got)
	}
	w = request(t, s, "POST", "/api/documents/"+d.ID+"/classification", nil)
	if w.Code != 409 {
		t.Fatal("manual classification was requeued")
	}
}
func TestClassificationCompletionAndManualRace(t *testing.T) {
	for _, scenario := range []string{"success", "invalid", "incomplete", "manual", "tags", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			s := testServer(t)
			d := organizationDoc(t, s)
			dir := s.analysisDir(d.ID)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			block := PDFBlock{ID: "p1-b1", Page: 1, Text: "Abstract: We evaluate a new security analysis method."}
			block.Bounds.Width = 1
			block.Bounds.Height = 1
			manifest, _ := json.Marshal(layoutManifest{Pages: 1, Blocks: []PDFBlock{block}})
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := &GenerationService{primary: "codex", connections: map[string]generationConnection{"codex": {VerifiedText: true, Adapter: adapterFunc(func(_ context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
				if !strings.Contains(req.Input.Prompt, "Abstract: We evaluate") || !strings.Contains(req.Input.Prompt, "untrusted document data") {
					t.Fatal("missing document boundary")
				}
				if scenario == "manual" {
					w := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"category":"book","tags":["mine"]}`))
					if w.Code != 200 {
						t.Fatal(w.Body.String())
					}
				}
				if scenario == "tags" {
					request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"tags":["mine"]}`))
				}
				text := `{"category":"paper"}`
				if scenario == "invalid" {
					text = `{"category":"pdf"}`
				}
				if err := emit(ProviderEvent{Text: text}); err != nil {
					return GenerateResult{}, err
				}
				if scenario == "cancel" {
					cancel()
					return GenerateResult{}, context.Canceled
				}
				if scenario == "incomplete" {
					return GenerateResult{}, errors.New("missing terminal")
				}
				return GenerateResult{Text: text, FinishReason: "stop"}, nil
			})}}}
			if _, err := s.Store.DB.Exec("UPDATE documents SET classification_status='pending' WHERE id=?", d.ID); err != nil {
				t.Fatal(err)
			}
			s.classifyDocument(ctx, d.ID, service)
			got, err := s.Store.Document(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "success", "tags":
				if got.Category != "paper" || got.CategorySource != "ai" || got.ClassificationStatus != "done" {
					t.Fatalf("result: %+v", got)
				}
				if scenario == "tags" && len(got.Tags) != 1 {
					t.Fatal("lost tags")
				}
			case "manual":
				if got.Category != "book" || got.CategorySource != "manual" || len(got.Tags) != 1 {
					t.Fatalf("overwrote manual: %+v", got)
				}
			case "cancel":
				if got.ClassificationStatus != "pending" || got.CategorySource != "default" {
					t.Fatalf("cancellation: %+v", got)
				}
			default:
				if got.ClassificationStatus != "failed" || got.CategorySource != "default" {
					t.Fatalf("saved invalid result: %+v", got)
				}
				w := request(t, s, "POST", "/api/documents/"+d.ID+"/classification", nil)
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				got, _ = s.Store.Document(d.ID)
				if got.ClassificationStatus != "pending" {
					t.Fatal("retry did not queue")
				}
			}
		})
	}
}

func TestClassificationEvidenceComesFromEachFormat(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	if _, err := s.classificationInput(d); err == nil {
		t.Fatal("classified a PDF without extracted text")
	}
	w := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	input, err := s.classificationInput(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input.Prompt, `"excerpts":[`) || strings.Contains(input.Prompt, `"excerpts":null`) {
		t.Fatal("missing EPUB excerpts")
	}
}

func TestTagBoardsPersistAndDeleteIndependently(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(store, "test-secret", "")
	// test request helper uses test-secret.
	w := request(t, s, "POST", "/api/tag-boards", strings.NewReader(`{"name":" Security ","tags":[" Web ","web","Go"],"match":"all"}`))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var board TagBoard
	json.Unmarshal(w.Body.Bytes(), &board)
	if board.Name != "Security" || len(board.Tags) != 2 {
		t.Fatalf("normalization: %+v", board)
	}
	store.DB.Close()
	store, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s = NewServer(store, "test-secret", "")
	w = request(t, s, "GET", "/api/tag-boards", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), board.ID) {
		t.Fatal("lost board on reopen", w.Body.String())
	}
	w = request(t, s, "PUT", "/api/tag-boards/"+board.ID, strings.NewReader(`{"name":"Either","tags":["Go","Web"],"match":"any"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	d := organizationDoc(t, s)
	if w := request(t, s, "DELETE", "/api/tag-boards/"+board.ID, nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if _, err := store.Document(d.ID); err != nil {
		t.Fatal("deleting board deleted document", err)
	}
	if w := request(t, s, "GET", "/api/tag-boards", nil); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "PUT", "/api/tag-boards/"+board.ID, strings.NewReader(`{"name":"Stale","tags":["Go"],"match":"all"}`))
	if w.Code != 404 {
		t.Fatal("update resurrected deleted board", w.Code)
	}
}
func TestTagBoardValidation(t *testing.T) {
	s := testServer(t)
	for _, body := range []string{`{}`, `{"name":"","tags":["A"],"match":"all"}`, `{"name":"x","tags":[],"match":"all"}`, `{"name":"x","tags":["A"],"match":"invalid"}`, `{"name":"x","tags":["a\nb"],"match":"all"}`} {
		if w := request(t, s, "POST", "/api/tag-boards", strings.NewReader(body)); w.Code != 400 {
			t.Fatal("accepted", body, w.Code)
		}
	}
}

func TestRelatedDocumentsLinkBothWays(t *testing.T) {
	s := testServer(t)
	ids := uploadPapers(t, s, 3)
	w := request(t, s, "PUT", "/api/documents/"+ids[1]+"/related/"+ids[0], nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var d Document
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if strings.Join(d.Related, ",") != ids[0] {
		t.Fatalf("related: %v", d.Related)
	}
	// Repeating the link is harmless and it shows on the other side too.
	request(t, s, "PUT", "/api/documents/"+ids[0]+"/related/"+ids[1], nil)
	other, _ := s.Store.Document(ids[0])
	if strings.Join(other.Related, ",") != ids[1] {
		t.Fatalf("reverse: %v", other.Related)
	}
	if w = request(t, s, "PUT", "/api/documents/"+ids[0]+"/related/"+ids[0], nil); w.Code != 400 {
		t.Fatalf("self: %d", w.Code)
	}
	if w = request(t, s, "PUT", "/api/documents/"+ids[0]+"/related/missing", nil); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}
	// Merging moves the duplicate's links to the version kept.
	request(t, s, "PUT", "/api/documents/"+ids[2]+"/related/"+ids[1], nil)
	if w = request(t, s, "POST", "/api/documents/"+ids[0]+"/merge", strings.NewReader(`{"from":["`+ids[2]+`"]}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if kept, _ := s.Store.Document(ids[0]); strings.Join(kept.Related, ",") != ids[1] {
		t.Fatalf("after merge: %v", kept.Related)
	}
	if w = request(t, s, "DELETE", "/api/documents/"+ids[1]+"/related/"+ids[0], nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if d, _ = s.Store.Document(ids[0]); len(d.Related) != 0 {
		t.Fatalf("unlink: %v", d.Related)
	}
	// Purging removes the links with the document.
	request(t, s, "PUT", "/api/documents/"+ids[0]+"/related/"+ids[1], nil)
	request(t, s, "POST", "/api/documents/"+ids[1]+"/trash", nil)
	if w = request(t, s, "DELETE", "/api/documents/"+ids[1], nil); w.Code != 200 && w.Code != 204 {
		t.Fatalf("purge: %d %s", w.Code, w.Body.String())
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM document_relations").Scan(&count)
	if count != 0 {
		t.Fatalf("relations left after purge: %d", count)
	}
}

func attachmentPaper(t *testing.T, s *Server, pages int) Document {
	t.Helper()
	w := uploadTo(t, s, "papers", "paper.pdf", testPDF(pages))
	if w.Code != 201 {
		t.Fatalf("paper: %d %s", w.Code, w.Body.String())
	}
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPaperAttachmentDeleteDoesNotHoldArchiveLockWhileWaitingForImport(t *testing.T) {
	s := testServer(t)
	d := attachmentPaper(t, s, 1)
	a := addedAttachment(t, s, d.ID, "supplement.txt", []byte("supplement"))
	s.importMu.Lock()
	locked := true
	done := make(chan struct{})
	var code int
	defer func() {
		if locked {
			s.importMu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("attachment deletion did not finish")
		}
	}()
	go func() {
		code = request(t, s, "DELETE", "/api/documents/"+d.ID+"/attachments/"+a.ID, nil).Code
		close(done)
	}()
	// Wait for the real handler to block on importMu before testing writer access.
	blocked := false
	deadline := time.Now().Add(2 * time.Second)
	buffer := make([]byte, 1<<20)
	for !blocked && time.Now().Before(deadline) {
		for _, stack := range strings.Split(string(buffer[:runtime.Stack(buffer, true)]), "\n\n") {
			if strings.Contains(stack, "(*Server).deletePaperAttachment") && strings.Contains(stack, "sync.runtime_SemacquireMutex") {
				blocked = true
				break
			}
		}
		if !blocked {
			runtime.Gosched()
		}
	}
	if !blocked {
		t.Fatal("deletion did not reach the import lock")
	}
	if s.libraryArchiveMu.TryLock() {
		s.libraryArchiveMu.Unlock()
	} else {
		t.Error("waiting attachment deletion blocks archive writers and can deadlock document cleanup")
	}
	s.importMu.Unlock()
	locked = false
	select {
	case <-done:
		if code != 204 {
			t.Fatalf("delete: %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deletion stalled after releasing import lock")
	}
}
func attachmentBody(t *testing.T, names []string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	for _, name := range names {
		f, err := m.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(data)
	}
	_ = m.Close()
	return &body, m.FormDataContentType()
}
func sendAttachment(t *testing.T, s *Server, doc string, names []string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := attachmentBody(t, names, data)
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/documents/"+doc+"/attachments", body)
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func addedAttachment(t *testing.T, s *Server, doc, name string, data []byte) PaperAttachment {
	t.Helper()
	w := sendAttachment(t, s, doc, []string{name}, data)
	if w.Code != 201 {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	var a PaperAttachment
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	return a
}
func attachmentList(t *testing.T, s *Server, doc string) []PaperAttachment {
	t.Helper()
	w := request(t, s, "GET", "/api/documents/"+doc+"/attachments", nil)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var items []PaperAttachment
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	return items
}
func attachmentJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestPaperAttachmentsStoreDedupeAndDownload(t *testing.T) {
	s := testServer(t)
	d := attachmentPaper(t, s, 1)
	data := []byte("<script>active content stays a download</script>")
	a := addedAttachment(t, s, d.ID, "补充材料.html", data)
	if a.Kind != "file" || a.Title != "补充材料.html" || a.Size != int64(len(data)) {
		t.Fatalf("metadata: %+v", a)
	}
	w := request(t, s, "PATCH", "/api/documents/"+d.ID+"/attachments/"+a.ID, strings.NewReader(`{"title":"实验补充"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = sendAttachment(t, s, d.ID, []string{"different.html"}, data)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "实验补充") || !strings.Contains(w.Body.String(), a.ID) {
		t.Fatalf("duplicate renamed: %d %s", w.Code, w.Body.String())
	}
	w = request(t, s, "GET", "/api/documents/"+d.ID+"/attachments/"+a.ID+"/file", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unsafe download: %d %v", w.Code, w.Header())
	}
	saved, _ := s.Store.Document(d.ID)
	if saved.AttachmentCount != 1 || saved.LastOpenedAt != d.LastOpenedAt || saved.Percentage != 0 {
		t.Fatalf("changed paper state: %+v", saved)
	}
	files, err := os.ReadDir(filepath.Join(s.Store.Root, "attachments"))
	if err != nil || len(files) != 1 || len(files[0].Name()) != 64 {
		t.Fatalf("storage: %v %v", files, err)
	}
	// A reopened store keeps labels and file ownership.
	reopened, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	items, err := reopened.paperAttachments(d.ID)
	if err != nil || len(items) != 1 || items[0].Title != "实验补充" {
		t.Fatalf("reopen: %+v %v", items, err)
	}
}
func TestPaperAttachmentLinksAndOwnershipBoundaries(t *testing.T) {
	s := testServer(t)
	d := attachmentPaper(t, s, 1)
	other := attachmentPaper(t, s, 2)
	path := "/api/documents/" + d.ID + "/attachments"
	for _, raw := range []string{`{"url":"javascript:alert(1)"}`, `{"url":"file:///tmp/a"}`, `{"url":"https://u:p@example.org"}`, `{"url":"https://example.org","title":"bad\nname"}`} {
		if w := attachmentJSON(t, s, path, raw); w.Code != 400 {
			t.Fatalf("unsafe link: %s %d", raw, w.Code)
		}
	}
	w := attachmentJSON(t, s, path, `{"url":"https://example.org/supplement"}`)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var a PaperAttachment
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	if a.Title != "example.org" || a.Kind != "link" {
		t.Fatalf("link: %+v", a)
	}
	if w := attachmentJSON(t, s, path, `{"title":"other","url":"https://example.org/supplement"}`); w.Code != 200 {
		t.Fatalf("link duplicate %d", w.Code)
	}
	for _, method := range []string{"PATCH", "DELETE", "GET"} {
		suffix := ""
		if method == "GET" {
			suffix = "/file"
		}
		w := request(t, s, method, "/api/documents/"+other.ID+"/attachments/"+a.ID+suffix, strings.NewReader(`{"title":"stolen"}`))
		if w.Code != 404 {
			t.Fatalf("cross parent %s: %d", method, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:17840"+path, nil)
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, r)
	if out.Code != 401 {
		t.Fatalf("unauthed list: %d", out.Code)
	}
	if len(attachmentList(t, s, d.ID)) != 1 {
		t.Fatal("attachment changed")
	}
}
func TestPaperAttachmentsFollowTrashMergeAndPurge(t *testing.T) {
	s := testServer(t)
	master := attachmentPaper(t, s, 1)
	other := attachmentPaper(t, s, 2)
	data := []byte("shared supplementary data")
	a := addedAttachment(t, s, other.ID, "data.csv", data)
	addedAttachment(t, s, master.ID, "different.csv", data)
	if w := request(t, s, "POST", "/api/documents/"+master.ID+"/merge", strings.NewReader(`{"from":["`+other.ID+`"]}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(attachmentList(t, s, master.ID)) != 1 {
		t.Fatal("merge did not deduplicate")
	}
	if w := request(t, s, "GET", "/api/documents/"+other.ID+"/attachments", nil); w.Code != 404 {
		t.Fatal("trashed attachments exposed")
	}
	if w := request(t, s, "POST", "/api/documents/"+other.ID+"/restore", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if got := attachmentList(t, s, other.ID); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("restore lost attachment: %+v", got)
	}
	if w := request(t, s, "DELETE", "/api/documents/"+other.ID, nil); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	keep := attachmentList(t, s, master.ID)[0]
	if w := request(t, s, "GET", "/api/documents/"+master.ID+"/attachments/"+keep.ID+"/file", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("purge removed shared bytes")
	}
	if w := request(t, s, "PATCH", "/api/documents/"+master.ID, strings.NewReader(`{"library":"books"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(attachmentList(t, s, master.ID)) != 1 {
		t.Fatal("library move lost attachments")
	}
	if w := request(t, s, "DELETE", "/api/documents/"+master.ID+"/attachments/"+keep.ID, nil); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	files, _ := os.ReadDir(filepath.Join(s.Store.Root, "attachments"))
	if len(files) != 0 {
		t.Fatal("last owner did not clean file")
	}
	// Unique attachments are copied as well; original owner remains recoverable.
	another := attachmentPaper(t, s, 3)
	target := attachmentPaper(t, s, 4)
	addedAttachment(t, s, another.ID, "unique.zip", []byte("unique"))
	if w := request(t, s, "POST", "/api/documents/"+target.ID+"/merge", strings.NewReader(`{"from":["`+another.ID+`"]}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(attachmentList(t, s, target.ID)) != 1 {
		t.Fatal("merge lost unique attachment")
	}
	if w := request(t, s, "DELETE", "/api/documents/"+target.ID, nil); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if w := request(t, s, "POST", "/api/documents/"+another.ID+"/restore", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	restored := attachmentList(t, s, another.ID)[0]
	if w := request(t, s, "GET", "/api/documents/"+another.ID+"/attachments/"+restored.ID+"/file", nil); w.Code != 200 {
		t.Fatal("original owner lost bytes")
	}
}
func TestPaperAttachmentUploadFailureLeavesNoRecordOrFile(t *testing.T) {
	s := testServer(t)
	d := attachmentPaper(t, s, 1)
	if w := sendAttachment(t, s, d.ID, []string{"one.txt", "two.txt"}, []byte("bad")); w.Code != 400 {
		t.Fatalf("multiple accepted: %d", w.Code)
	}
	body, ct := attachmentBody(t, []string{"partial.txt"}, []byte("partial"))
	data := body.Bytes()
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/documents/"+d.ID+"/attachments", bytes.NewReader(data[:len(data)-50]))
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("partial accepted: %d", w.Code)
	}
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_attachment BEFORE INSERT ON paper_attachments BEGIN SELECT RAISE(ABORT,'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if w := sendAttachment(t, s, d.ID, []string{"file.txt"}, []byte("complete")); w.Code != 500 {
		t.Fatalf("failed insert: %d", w.Code)
	}
	if len(attachmentList(t, s, d.ID)) != 0 {
		t.Fatal("failed upload saved record")
	}
	files, _ := os.ReadDir(filepath.Join(s.Store.Root, "attachments"))
	if len(files) != 0 {
		t.Fatal("failed upload left files")
	}
}

type pausedAttachmentReader struct {
	prefix, rest io.Reader
	waiting      chan struct{}
	resume       chan struct{}
	paused       bool
}

func (r *pausedAttachmentReader) Read(p []byte) (int, error) {
	if r.prefix != nil {
		n, err := r.prefix.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			r.prefix = nil
		} else {
			return n, err
		}
	}
	if !r.paused {
		r.paused = true
		close(r.waiting)
		<-r.resume
	}
	return r.rest.Read(p)
}
func TestPaperAttachmentUploadRechecksParentAfterTransfer(t *testing.T) {
	s := testServer(t)
	d := attachmentPaper(t, s, 1)
	body, ct := attachmentBody(t, []string{"late.csv"}, bytes.Repeat([]byte("x"), 2000))
	data := body.Bytes()
	paused := &pausedAttachmentReader{prefix: bytes.NewReader(data[:300]), rest: bytes.NewReader(data[300:]), waiting: make(chan struct{}), resume: make(chan struct{})}
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/documents/"+d.ID+"/attachments", paused)
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", ct)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); s.Handler().ServeHTTP(w, r); done <- w }()
	select {
	case <-paused.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	w := request(t, s, "POST", "/api/documents/"+d.ID+"/trash", nil)
	close(paused.resume)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	select {
	case result := <-done:
		if result.Code != 404 {
			t.Fatalf("late upload committed: %d %s", result.Code, result.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
	files, _ := os.ReadDir(filepath.Join(s.Store.Root, "attachments"))
	if len(files) != 0 {
		t.Fatal("late upload left files")
	}
}

func TestPaperAttachmentFailedDeletionKeepsOwnedBytes(t *testing.T) {
	s := testServer(t)
	d := attachmentPaper(t, s, 1)
	other := attachmentPaper(t, s, 2)
	a := addedAttachment(t, s, d.ID, "supplement.pdf", testPDF(2))
	if w := request(t, s, "GET", "/api/documents/"+other.ID+"/attachments/"+a.ID+"/file", nil); w.Code != 404 {
		t.Fatal("cross-paper file exposed")
	}
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER keep_document BEFORE DELETE ON documents BEGIN SELECT RAISE(ABORT,'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if w := request(t, s, "DELETE", "/api/documents/"+d.ID, nil); w.Code != 500 {
		t.Fatalf("delete should fail: %d", w.Code)
	}
	if len(attachmentList(t, s, d.ID)) != 1 {
		t.Fatal("failed deletion lost attachment ownership")
	}
	if w := request(t, s, "GET", "/api/documents/"+d.ID+"/attachments/"+a.ID+"/file", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), testPDF(2)) {
		t.Fatal("failed deletion lost bytes")
	}
}

// A stalled response keeps the real download handle open while another request
// removes ownership. Permission denial below models Windows' sharing violation.
type blockedAttachmentResponse struct {
	*httptest.ResponseRecorder
	started, release chan struct{}
	once             sync.Once
	writeErr         error
}

func (w *blockedAttachmentResponse) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(p)
}
func startBlockedAttachmentDownload(t *testing.T, s *Server, docID, attachmentID string, writeErr error) (*blockedAttachmentResponse, <-chan struct{}, func()) {
	t.Helper()
	w := &blockedAttachmentResponse{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{}), writeErr: writeErr}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(w.release) }) }
	done := make(chan struct{})
	r := httptest.NewRequest("GET", "http://127.0.0.1:17840/api/documents/"+docID+"/attachments/"+attachmentID+"/file", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	go func() { s.Handler().ServeHTTP(w, r); close(done) }()
	t.Cleanup(func() {
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("download did not stop")
		}
	})
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	return w, done, release
}
func TestPaperAttachmentDownloadRetriesFailedCleanup(t *testing.T) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("requires directory permissions or Windows sharing violations")
	}
	for _, test := range []struct {
		name     string
		purge    bool
		writeErr error
	}{{name: "complete"}, {name: "interrupted", writeErr: io.ErrClosedPipe}, {name: "purge", purge: true}} {
		t.Run(test.name, func(t *testing.T) {
			s := testServer(t)
			d := attachmentPaper(t, s, 1)
			data := testPDF(2)
			a := addedAttachment(t, s, d.ID, "supplement.pdf", data)
			owned, err := s.Store.paperAttachments(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(s.Store.Root, "attachments")
			path := filepath.Join(directory, owned[0].Hash)
			w, done, release := startBlockedAttachmentDownload(t, s, d.ID, a.ID, test.writeErr)
			if runtime.GOOS != "windows" {
				if err := os.Chmod(directory, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
			}
			endpoint := "/api/documents/" + d.ID + "/attachments/" + a.ID
			if test.purge {
				endpoint = "/api/documents/" + d.ID
			}
			deleted := make(chan *httptest.ResponseRecorder, 1)
			go func() { deleted <- request(t, s, "DELETE", endpoint, nil) }()
			select {
			case response := <-deleted:
				if response.Code != 204 {
					t.Fatalf("delete: %d %s", response.Code, response.Body.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("download blocked unrelated library mutations")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("expected an in-flight file awaiting cleanup: %v", err)
			}
			if runtime.GOOS != "windows" {
				if err := os.Chmod(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("download did not complete")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("deleted attachment bytes survived download completion: %v", err)
			}
			if test.writeErr == nil && !bytes.Equal(w.Body.Bytes(), data) {
				t.Fatal("in-flight download was truncated")
			}
		})
	}
}
func TestPaperAttachmentDownloadCompletionKeepsReattachedFile(t *testing.T) {
	s := testServer(t)
	d := attachmentPaper(t, s, 1)
	data := testPDF(2)
	a := addedAttachment(t, s, d.ID, "supplement.pdf", data)
	_, done, release := startBlockedAttachmentDownload(t, s, d.ID, a.ID, nil)
	if w := request(t, s, "DELETE", "/api/documents/"+d.ID+"/attachments/"+a.ID, nil); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	replacement := addedAttachment(t, s, d.ID, "reimported.pdf", data)
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not complete")
	}
	if w := request(t, s, "GET", "/api/documents/"+d.ID+"/attachments/"+replacement.ID+"/file", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("download completion deleted reattached content")
	}
}

func TestPaperAttachmentCountUsesOwnershipIndexAfterUpgrade(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB.Exec(`INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,percentage) VALUES('paper','pdf','Paper','',10,'t','t',0.5);
 INSERT INTO paper_attachments(id,document_id,kind,title,url,created_at) VALUES('link','paper','link','Supplement','https://example.org/supplement','t');
 DROP INDEX IF EXISTS paper_attachment_owner;`)
	if err != nil {
		store.DB.Close()
		t.Fatal(err)
	}
	if err := store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.DB.Close()
	d, err := upgraded.Document("paper")
	if err != nil || d.AttachmentCount != 1 || d.Percentage != 0.5 {
		t.Fatalf("upgrade changed data: %+v %v", d, err)
	}
	rows, err := upgraded.DB.Query(`EXPLAIN QUERY PLAN SELECT id,(SELECT count(*) FROM paper_attachments a WHERE a.document_id=documents.id) FROM documents`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
		if strings.Contains(detail, "SEARCH a ") && strings.Contains(detail, "document_id=?") {
			indexed = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("attachment count still scans all attachments for each paper")
	}
}

func TestPaperAttachmentFileRangesRequireAuthentication(t *testing.T) {
	s := testServer(t)
	d := attachmentPaper(t, s, 1)
	other := attachmentPaper(t, s, 2)
	data := testPDF(2)
	a := addedAttachment(t, s, d.ID, "supplement.pdf", data)
	endpoint := "/api/documents/" + d.ID + "/attachments/" + a.ID + "/file"
	serve := func(method, path, rangeHeader, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:17840"+path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if rangeHeader != "" {
			r.Header.Set("Range", rangeHeader)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	w := serve("GET", endpoint, "bytes=0-63", "test-secret")
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), data[:64]) || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Content-Range") != fmt.Sprintf("bytes 0-63/%d", len(data)) || w.Header().Get("Content-Length") != "64" {
		t.Fatalf("range response: %d %v", w.Code, w.Header())
	}
	w = serve("HEAD", endpoint, "", "test-secret")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != fmt.Sprint(len(data)) {
		t.Fatalf("head response: %d %v", w.Code, w.Header())
	}
	if w := serve("GET", endpoint, "bytes=0-63", ""); w.Code != 401 {
		t.Fatalf("unauthenticated range: %d", w.Code)
	}
	if w := serve("GET", "/api/documents/"+other.ID+"/attachments/"+a.ID+"/file", "bytes=0-63", "test-secret"); w.Code != 404 {
		t.Fatalf("cross-parent range: %d", w.Code)
	}
	if w := serve("GET", endpoint, "bytes=999999-", "test-secret"); w.Code != 416 {
		t.Fatalf("unsatisfiable range: %d", w.Code)
	}
}
