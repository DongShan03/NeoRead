package neoread

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"golang.org/x/net/html"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEPUBReaderSanitizerAndBlockAlignment(t *testing.T) {
	source := `<body><div> Before <strong>😀</strong><p id="p" class="app" style="color:red" onclick="alert(1)">Same <em>words</em>.</p>After<p>Same words.</p></div><ul><li>List</li></ul><table><tr><td colspan="2">Cell</td></tr></table><ruby>字<rt>zi</rt></ruby><pre>code</pre><figure><img src="../images/p.png" onerror="alert(1)"><figcaption>Caption</figcaption></figure><script>evil()</script><style>@import 'https://evil';</style><form>secret<input></form><iframe srcdoc="bad">bad</iframe><object>bad</object><embed><button>bad</button><svg><script>bad</script><foreignObject>bad</foreignObject></svg><img src="https://evil/p.png"><img src="../../../escape"><img src="data:image/svg+xml,bad"><a href="javascript:alert(1)">bad link</a><a href="next.xhtml#note">note</a><a href="#p">here</a><a href="https://example.test/">external</a></body>`
	rendered, blocks, err := renderEPUBChapter("EPUB/text/a.xhtml", source, "/pub/token/book/")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"<script", "<style", "onclick", "onerror", "style=", "class=", "<form", "<input", "<button", "<iframe", "<object", "<embed", "javascript:", "data:image", "https://evil", "escape", "foreignobject"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("unsafe %s in %s", forbidden, rendered)
		}
	}
	for _, want := range []string{`src="/pub/token/book/EPUB/images/p.png"`, `href="EPUB/text/next.xhtml#note"`, `href="EPUB/text/a.xhtml#p"`, `href="https://example.test/"`, `<ruby>`, `<pre`, `<table>`} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %s in %s", want, rendered)
		}
	}
	again, err := epubTextBlocks("EPUB/text/a.xhtml", source)
	if err != nil || len(again) != len(blocks) {
		t.Fatal(err)
	}
	doc, _ := html.Parse(strings.NewReader(rendered))
	var ids []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for _, a := range n.Attr {
			if a.Key == "data-epub-block" {
				ids = append(ids, a.Val)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if len(ids) != len(blocks) {
		t.Fatalf("%d anchors for %d blocks: %s", len(ids), len(blocks), rendered)
	}
	for i, b := range blocks {
		if ids[i] != b.ID || again[i].ID != b.ID {
			t.Fatalf("block drift at %d", i)
		}
	}
}

func TestEPUBReaderKeepsBodyTOCAnchor(t *testing.T) {
	source := `<html><body id="chapter-start"><h1>Chapter</h1><p>First paragraph.</p></body></html>`
	rendered, blocks, err := renderEPUBChapter("chapter.xhtml", source, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `id="chapter-start"`) || len(blocks) == 0 || !strings.Contains(rendered, "First paragraph.") {
		t.Fatalf("body TOC anchor or prose lost: %s", rendered)
	}
}
func TestEPUBReaderURLs(t *testing.T) {
	for _, value := range []string{"../../../escape", "%2e%2e/%2e%2e/escape", "//evil.test/x", "/app", "https://evil.test/x", "javascript:alert(1)", "data:image/png,bad", "..\\escape", "java\nscript:bad"} {
		if got := epubURL("EPUB/a.xhtml", value, true); got != "" {
			t.Errorf("accepted %q: %s", value, got)
		}
	}
}
func TestEPUBBlockLocationValidation(t *testing.T) {
	for _, tail := range []string{`"blockId":"b","start":0,"end":2`, `"blockId":"b","start":2,"end":1,"endBlockId":"c"`} {
		if !validLocation([]byte(`{"type":"epub","href":"a.xhtml",`+tail+`}`), "epub") {
			t.Fatal(tail)
		}
	}
	for _, tail := range []string{`"start":0`, `"blockId":""`, `"blockId":"b","start":-1,"end":2`, `"blockId":"b","start":2,"end":1`, `"blockId":"b","start":0.5,"end":2`, `"blockId":"b","start":0,"end":null`} {
		if validLocation([]byte(`{"type":"epub","href":"a.xhtml",`+tail+`}`), "epub") {
			t.Fatal(tail)
		}
	}
}
func TestEPUBReaderEndpoints(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	var d Document
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &d) != nil {
		t.Fatal(response.Body.String())
	}
	response = request(t, s, "GET", "/api/documents/"+d.ID+"/epub-chapters", nil)
	var listing struct {
		Chapters []epubReaderChapter `json:"chapters"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &listing) != nil || len(listing.Chapters) == 0 {
		t.Fatal(response.Body.String())
	}
	for _, c := range listing.Chapters {
		response = request(t, s, "GET", "/api/documents/"+d.ID+"/epub-chapter?href="+url.QueryEscape(c.Href), nil)
		var chapter struct {
			HTML string `json:"html"`
		}
		json.Unmarshal(response.Body.Bytes(), &chapter)
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
		for _, b := range c.Blocks {
			if strings.Count(chapter.HTML, `data-epub-block="`+b.ID+`"`) != 1 {
				t.Fatal(b.ID)
			}
		}
	}
	response = request(t, s, "GET", "/api/documents/"+d.ID+"/epub-chapter?href=../../escape", nil)
	if response.Code != 404 {
		t.Fatal(response.Code)
	}
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/documents/"+d.ID+"/epub-chapters", nil))
	if recorder.Code != 401 && recorder.Code != 403 {
		t.Fatal("unauthenticated reader", recorder.Code)
	}
}

func TestEPUBReaderRejectsUnsupportedLayouts(t *testing.T) {
	for _, layout := range []string{`{"presentation":{"layout":"fixed"}}`, `{"readingProgression":"rtl"}`, `{"presentation":{"layout":"pre-paginated"}}`} {
		s := testServer(t)
		response := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
		var d Document
		json.Unmarshal(response.Body.Bytes(), &d)
		manifest, err := s.readerManifest(d.ID)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Metadata = json.RawMessage(layout)
		encoded, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(s.Store.Root, "cache", d.ID, "manifest.json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
		response = request(t, s, "GET", "/api/documents/"+d.ID+"/epub-chapters", nil)
		if response.Code != 422 || !strings.Contains(response.Body.String(), "暂不支持") {
			t.Fatal(response.Body.String())
		}
	}
}

func TestEPUBReaderReadsLayoutFieldsNotProse(t *testing.T) {
	for _, raw := range []string{`{"title":"Portland turtles","description":"fixed \"rtl\" layouts"}`, `{"presentation":{"layout":"reflowable"},"readingProgression":"ltr"}`, `{"subject":["fixed"]}`} {
		if unsupportedEPUBLayout(json.RawMessage(raw)) {
			t.Fatal("rejected readable metadata", raw)
		}
	}
	for _, raw := range []string{`{"readingProgression":"RTL"}`, `{"layout":"fixed"}`, `{"presentation":{"layout":"pre-paginated"}}`} {
		if !unsupportedEPUBLayout(json.RawMessage(raw)) {
			t.Fatal("accepted unsupported layout", raw)
		}
	}
}

func TestEPUBReaderPreservesNewAndLegacyAnnotations(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	blocks, err := s.readEPUBBlocks(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := json.Marshal(blocks[0].Location)
	legacy, _ := json.Marshal(map[string]any{"type": "epub", "href": blocks[0].Location.Href, "locator": blocks[0].Location.Locator, "quote": blocks[0].Text})
	for _, location := range []json.RawMessage{fresh, legacy} {
		encoded, _ := json.Marshal(Annotation{Kind: "note", Location: location, Quote: blocks[0].Text, Note: "保留原始位置", Color: "#e6b94c"})
		response = request(t, s, "POST", "/api/documents/"+d.ID+"/annotations", bytes.NewReader(encoded))
		if response.Code != 201 {
			t.Fatal(response.Body.String())
		}
	}
	// NeoRead endpoints are read-only, including legacy location handling.
	request(t, s, "GET", "/api/documents/"+d.ID+"/epub-chapters", nil)
	response = request(t, s, "GET", "/api/documents/"+d.ID+"/annotations", nil)
	var notes []Annotation
	json.Unmarshal(response.Body.Bytes(), &notes)
	if len(notes) != 2 {
		t.Fatal(response.Body.String())
	}
	for _, location := range []json.RawMessage{fresh, legacy} {
		found := false
		for _, note := range notes {
			if bytes.Equal(note.Location, location) {
				found = true
			}
		}
		if !found {
			t.Fatal("saved location was changed", string(location), response.Body.String())
		}
	}
}

func TestEPUBReaderLargePublication(t *testing.T) {
	filename := os.Getenv("NEOREAD_TEST_EPUB")
	if filename == "" {
		t.Skip("set NEOREAD_TEST_EPUB for the local large-book fixture")
	}
	cache := t.TempDir()
	manifest, _, err := prepareEPUB(context.Background(), filename, cache)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(manifest)
	var m epubReaderManifest
	json.Unmarshal(encoded, &m)
	started := time.Now()
	count, images, size := 0, 0, 0
	for _, link := range m.ReadingOrder {
		name, err := safeResource(link.Href)
		if err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(filepath.Join(cache, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		rendered, blocks, err := renderEPUBChapter(link.Href, string(source), "/pub/test/book/")
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			if strings.Count(rendered, `data-epub-block="`+b.ID+`"`) != 1 {
				t.Fatal("missing or duplicate block", link.Href, b.ID)
			}
			if b.Image != "" {
				images++
			}
		}
		count += len(blocks)
		size += len(rendered)
	}
	t.Logf("chapters=%d blocks=%d images=%d sanitized_bytes=%d elapsed=%s", len(m.ReadingOrder), count, images, size, time.Since(started))
}

func TestEPUBReaderLayoutDeclarations(t *testing.T) {
	for _, source := range []string{`<html dir="RTL"><p>text</p>`, `<body style="writing-mode: vertical-rl"><p>text</p></body>`, `<style>body { -epub-writing-mode: tb-rl; }</style>`, `<style>body{ direction: rtl !important }</style>`, `<style>@media screen { html, p { writing-mode: vertical-rl } }</style>`, `<style>:root{direction:rtl}</style>`} {
		if !unsupportedEPUBStyle(source, true) {
			t.Fatal("accepted unsupported declaration", source)
		}
	}
	// Element-level vertical or RTL text still reads in a horizontal book.
	for _, source := range []string{`<p>writing-mode: vertical-rl is a CSS declaration.</p>`, `<style>/* body{writing-mode:vertical-rl} */ p { color: red }</style>`, `<p style="writing-mode: vertical-rl">text</p>`, `<p dir="rtl">שלום</p>`, `<style>.tategaki { writing-mode: vertical-rl } .bodytext { direction: rtl }</style>`} {
		if unsupportedEPUBStyle(source, true) {
			t.Fatal("rejected prose or comment", source)
		}
	}
}

func TestEPUBReaderLayoutDeclarationsSelectorSubjects(t *testing.T) {
	for _, selector := range []string{
		"html > body", "html.book", "body#reader", ":root.book", "p, body.book",
		`body[data-title="a > b, :before"]`, `html:not(.aside, .quote) > body`,
		`body:has(.quote > span, .note)`, `body[data-title="a\" > b"]`,
	} {
		t.Run(selector, func(t *testing.T) {
			if !unsupportedEPUBStyle(selector+" { direction: rtl }", false) {
				t.Fatal("accepted root declaration", selector)
			}
		})
	}
	for _, selector := range []string{
		"body .quote", "html .vertical", ":root > h1", "body + aside", "body ~ p",
		"body::before", "body:after", "html::first-letter", "body::part(title)",
		`[data-label="body"]`, `.quote:not(body, :root)`, `.quote:has(body > p)`,
		`body [data-label="a,b"]`, `body [data-label="body > :root"]`,
		`body[data-title="a,b"] .quote`, `body\.quote`, `body-custom`,
		"body .quote, :root > h1", `p[data-label="x, body::before"]`,
	} {
		t.Run(selector, func(t *testing.T) {
			if unsupportedEPUBStyle(selector+" { writing-mode: vertical-rl }", false) {
				t.Fatal("rejected descendant or pseudo-element declaration", selector)
			}
		})
	}
}

func TestEPUBReaderSummaryDoesNotReadChapterBodies(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	manifest, err := s.readerManifest(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A missing distant chapter must not prevent opening the chapter list.
	name, _ := safeResource(manifest.ReadingOrder[len(manifest.ReadingOrder)-1].Href)
	if err := os.Remove(filepath.Join(s.Store.Root, "cache", d.ID, filepath.FromSlash(name))); err != nil {
		t.Fatal(err)
	}
	response = request(t, s, "GET", "/api/documents/"+d.ID+"/epub-chapters?summary=1", nil)
	var listing struct {
		Chapters []epubReaderChapter `json:"chapters"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &listing) != nil {
		t.Fatal(response.Body.String())
	}
	if len(listing.Chapters) != len(manifest.ReadingOrder) {
		t.Fatal("incomplete spine")
	}
	for _, c := range listing.Chapters {
		if len(c.Blocks) != 0 {
			t.Fatal("summary contains body", c.Href)
		}
	}
	first := listing.Chapters[0]
	response = request(t, s, "GET", "/api/documents/"+d.ID+"/epub-chapter?href="+url.QueryEscape(first.Href), nil)
	var chapter struct {
		HTML   string             `json:"html"`
		Blocks []EPUBReadingBlock `json:"blocks"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &chapter) != nil || len(chapter.Blocks) == 0 {
		t.Fatal(response.Body.String())
	}
	for _, b := range chapter.Blocks {
		if !strings.Contains(chapter.HTML, `data-epub-block="`+b.ID+`"`) {
			t.Fatal(b.ID)
		}
	}
}

func TestEPUBChapterSubscriptionDoesNotReadDistantChaptersOrTranslations(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	var d Document
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &d) != nil {
		t.Fatal(response.Body.String())
	}
	manifest, err := s.readerManifest(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	href := manifest.ReadingOrder[0].Href
	_, blocks, err := s.readerChapter(d.ID, href)
	if err != nil || len(blocks.translationItems()) == 0 {
		t.Fatal(err)
	}
	name, _ := safeResource(manifest.ReadingOrder[len(manifest.ReadingOrder)-1].Href)
	if err := os.Remove(filepath.Join(s.Store.Root, "cache", d.ID, filepath.FromSlash(name))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("INSERT INTO translations(document_id,block_id,source_hash,status,body) VALUES(?,?,?,?,?)", d.ID, "unrelated", "hash", "complete", "invalid JSON"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/documents/"+d.ID+"/translations/stream?href="+url.QueryEscape(href), nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	next := func() string {
		t.Helper()
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data: ") {
				return strings.TrimPrefix(scanner.Text(), "data: ")
			}
		}
		t.Fatalf("stream ended: %v", scanner.Err())
		return ""
	}
	var snapshot []TranslationBlock
	if err := json.Unmarshal([]byte(next()), &snapshot); err != nil || len(snapshot) != len(blocks.translationItems()) {
		t.Fatalf("invalid chapter snapshot: %v", err)
	}
	current := snapshot[0]
	current.Status = "complete"
	s.translationMu.Lock()
	s.publishTranslation(d.ID, TranslationBlock{BlockID: "unrelated", Status: "complete"})
	s.publishTranslation(d.ID, current)
	s.translationMu.Unlock()
	var live TranslationBlock
	if err := json.Unmarshal([]byte(next()), &live); err != nil || live.BlockID != current.BlockID {
		t.Fatalf("unscoped live event: %+v %v", live, err)
	}
	cancel()
	res.Body.Close()
	invalid := request(t, s, "GET", "/api/documents/"+d.ID+"/translations/stream?href=../escape", nil)
	if invalid.Code != 404 {
		t.Fatal(invalid.Code)
	}
}

func TestEPUBSourceNoteRecoveryDoesNotReadChapterBodies(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	var d Document
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &d) != nil {
		t.Fatal(response.Body.String())
	}
	manifest, err := s.readerManifest(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.saveProcessing(Processing{DocumentID: d.ID, Status: "complete"}); err != nil {
		t.Fatal(err)
	}
	location, _ := json.Marshal(map[string]string{"type": "epub", "href": manifest.ReadingOrder[0].Href})
	note := Annotation{ID: "source-note", DocumentID: d.ID, Kind: "note", Location: location, Note: "keep"}
	if _, err := s.Store.DB.Exec("INSERT INTO annotations(id,document_id,body) VALUES(?,?,?)", note.ID, d.ID, jsonText(note)); err != nil {
		t.Fatal(err)
	}
	for _, link := range manifest.ReadingOrder {
		name, _ := safeResource(link.Href)
		if err := os.Remove(filepath.Join(s.Store.Root, "cache", d.ID, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	response = request(t, s, "POST", "/api/documents/"+d.ID+"/annotations/recover", strings.NewReader(`{}`))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"checked":true`) {
		t.Fatal(response.Body.String())
	}
	var body string
	if err := s.Store.DB.QueryRow("SELECT body FROM annotations WHERE id=?", note.ID).Scan(&body); err != nil || body != jsonText(note) {
		t.Fatal("source note changed", err)
	}
}

func TestEPUBSelfClosingScriptPreservesBody(t *testing.T) {
	for _, script := range []string{
		`<script src="../js/kobo.js"/>`,
		`<script src="../js/kobo.js"></script>`,
		`<script src="../js/kobo.js" />`,
		`<SCRIPT SRC="../js/kobo.js"/>`,
		`<style/>`,
		`<title/>`,
		`<iframe/>`,
		`<script>const text = '<script/>LEAK';</script>`,
		`<!-- <script/> --><script>LEAK</script>`,
	} {
		source := `<html xmlns="http://www.w3.org/1999/xhtml"><head>` + script + `</head><body><h1 id="chapter">Chapter</h1><p>Original paragraph</p></body></html>`
		cleaned, _, err := sanitizeContent([]byte(source))
		if err != nil {
			t.Fatal(err)
		}
		rendered, blocks, err := renderEPUBChapter("chapter.xhtml", string(cleaned), "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rendered, `id="chapter"`) || !strings.Contains(rendered, "Original paragraph") || len(blocks) == 0 {
			t.Fatalf("lost body or TOC anchor: html=%s blocks=%d", rendered, len(blocks))
		}
		if strings.Contains(string(cleaned), "<script") || strings.Contains(string(cleaned), "kobo.js") || strings.Contains(string(cleaned), "LEAK") {
			t.Fatal("active script survived")
		}
	}
}

func TestEPUBEmptyIndexAnchorsDoNotEncloseProse(t *testing.T) {
	source := `<html xmlns="http://www.w3.org/1999/xhtml"><head><script src="kobo.js"/></head><body><p><a id="index-a"/><a id="index-b"/><span>First paragraph.</span></p><p>Second paragraph.</p><p><a href="#reference">1</a></p><p id="reference">Reference.</p></body></html>`
	cleaned, _, err := sanitizeContent([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	rendered, blocks, err := renderEPUBChapter("chapter.xhtml", string(cleaned), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{string(cleaned), rendered} {
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		var visit func(*html.Node)
		visit = func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "a" {
				for _, attr := range n.Attr {
					if attr.Key == "id" && strings.HasPrefix(attr.Val, "index-") {
						seen[attr.Val] = true
						if n.FirstChild != nil {
							t.Errorf("empty index anchor %s encloses prose", attr.Val)
						}
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				visit(c)
			}
		}
		visit(doc)
		if !seen["index-a"] || !seen["index-b"] {
			t.Fatal("index targets lost")
		}
	}
	if len(blocks) != 4 || !strings.Contains(rendered, `href="chapter.xhtml#reference"`) {
		t.Fatalf("paragraphs or real link lost: %s", rendered)
	}
}

func TestEPUBLocatorValidation(t *testing.T) {
	for _, locator := range []string{
		`null`, `[]`, `not JSON`,
		`{"href":"https://example.test/chapter.xhtml","type":"text/html"}`,
		`{"href":"other.xhtml","type":"text/html"}`,
		`{"href":"chapter.xhtml","type":"text/html","locations":{"progression":2}}`,
		`{"href":"chapter.xhtml","type":"text/html","locations":{"domRange":{"start":{"cssSelector":"p","textNodeIndex":-1}}}}`,
		`{"href":"chapter.xhtml","type":"text/html","locations":{"domRange":{"end":{"cssSelector":"p"}}}}`,
		`{"href":"chapter.xhtml","type":"text/html","locations":{"textRange":{"start":5,"end":2}}}`,
	} {
		location, _ := json.Marshal(map[string]any{"type": "epub", "href": "chapter.xhtml", "locator": locator})
		if validLocation(location, "epub") {
			t.Errorf("accepted %s", locator)
		}
	}
	for _, locator := range []string{"", "{}", `{"href":"chapter.xhtml","type":"text/html","locations":{"progression":0.8}}`} {
		location, _ := json.Marshal(map[string]any{"type": "epub", "href": "chapter.xhtml", "locator": locator})
		if !validLocation(location, "epub") {
			t.Errorf("rejected compatible progress %s", locator)
		}
	}
}
func TestEPUBExactAnnotationSurvivesReopen(t *testing.T) {
	s := testServer(t)
	w := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var document Document
	if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	locator := `{"href":"EPUB/chapter1.xhtml","type":"application/xhtml+xml","locations":{"domRange":{"start":{"cssSelector":"html > body > p:nth-of-type(2)","textNodeIndex":0,"charOffset":2},"end":{"cssSelector":"html > body > p:nth-of-type(2)","textNodeIndex":0,"charOffset":6}},"textRange":{"start":12,"end":16}},"text":{"highlight":"阅读😀","before":"前文","after":"后文"}}`
	location, _ := json.Marshal(map[string]any{"type": "epub", "href": "EPUB/chapter1.xhtml", "locator": locator, "quote": "阅读😀"})
	body, _ := json.Marshal(Annotation{Kind: "note", Location: location, Quote: "阅读😀", Note: "Keep this note", Color: "#e6b94c"})
	w = request(t, s, "POST", "/api/documents/"+document.ID+"/annotations", bytes.NewReader(body))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var created Annotation
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	reopened := NewServer(store, "test-secret", "")
	w = request(t, reopened, "GET", "/api/documents/"+document.ID+"/annotations", nil)
	var annotations []Annotation
	if err := json.Unmarshal(w.Body.Bytes(), &annotations); err != nil || len(annotations) != 1 {
		t.Fatalf("%s: %v", w.Body.String(), err)
	}
	var saved struct {
		Locator string `json:"locator"`
	}
	json.Unmarshal(annotations[0].Location, &saved)
	if saved.Locator != locator || annotations[0].Note != "Keep this note" {
		t.Fatal("annotation changed during persistence")
	}
	w = request(t, reopened, "DELETE", "/api/documents/"+document.ID+"/annotations/"+created.ID, nil)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	w = request(t, reopened, "GET", "/api/documents/"+document.ID+"/annotations", nil)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("deleted annotation remains", w.Body.String())
	}
}

func TestEPUBImageBlockIsServedAndAttachedAsPNG(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	response := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	var doc Document
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &doc) != nil {
		t.Fatal(response.Body.String())
	}
	cache := filepath.Join(s.Store.Root, "cache", doc.ID)
	if err := os.MkdirAll(filepath.Join(cache, "check"), 0700); err != nil {
		t.Fatal(err)
	}
	picture := image.NewRGBA(image.Rect(0, 0, 4, 3))
	picture.Set(1, 1, color.RGBA{200, 10, 10, 255})
	var data bytes.Buffer
	if err := jpeg.Encode(&data, picture, nil); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"check/pic.jpg": data.String(),
		"check/c.xhtml": `<html><body><p>Before.</p><img src="pic.jpg" alt="A chart"/></body></html>`,
		"manifest.json": `{"readingOrder":[{"href":"check/c.xhtml","type":"application/xhtml+xml"}]}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cache, filepath.FromSlash(name)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	blocks, err := s.readEPUBBlocks(doc.ID)
	if err != nil || len(blocks) != 2 || blocks[1].Image != "check/pic.jpg" {
		t.Fatalf("blocks %+v %v", blocks, err)
	}
	w := request(t, s, "GET", "/pub/test-secret/"+doc.ID+"/assets/"+blocks[1].ID+".png", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image route: %d %s", w.Code, w.Body.String())
	}
	if config, err := png.DecodeConfig(w.Body); err != nil || config.Width != 4 || config.Height != 3 {
		t.Fatalf("not a PNG of the picture: %+v %v", config, err)
	}
	block, _, err := s.readBlockImage(doc.ID, blocks[1].ID)
	if err != nil || block.Page != 0 || block.Caption != "A chart" {
		t.Fatalf("attachment %+v %v", block, err)
	}
	for _, id := range []string{blocks[0].ID, "p1-b1", "../c"} {
		if _, _, err := s.readBlockImage(doc.ID, id); err == nil {
			t.Fatalf("%s resolved to an image", id)
		}
	}
	// The publication's own files are still served as they are.
	if w = request(t, s, "GET", "/pub/test-secret/"+doc.ID+"/check/pic.jpg", nil); w.Code != 200 {
		t.Fatalf("publication resource: %d", w.Code)
	}
}

// bookEPUB adds publication details and a cover image to the sample EPUB.
func bookEPUB(t *testing.T) []byte {
	t.Helper()
	original := sample(t, "the-art-of-reading.epub")
	source, err := zip.NewReader(bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	var cover bytes.Buffer
	if err = png.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 4, 6))); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for _, file := range source.File {
		r, _ := file.Open()
		body, _ := io.ReadAll(r)
		r.Close()
		if file.Name == "EPUB/package.opf" {
			body = bytes.Replace(body, []byte("<dc:language>"), []byte(`<dc:publisher>示例出版社</dc:publisher><dc:date>2021-03-04T00:00:00Z</dc:date><dc:identifier>urn:isbn:978-7-111-12345-6</dc:identifier><dc:description>&lt;p&gt;第一段&lt;/p&gt;&lt;p&gt;第二  段&lt;/p&gt;</dc:description><dc:language>`), 1)
			body = bytes.Replace(body, []byte("</manifest>"), []byte(`<item id="cover" href="images/cover.png" media-type="image/png" properties="cover-image"/></manifest>`), 1)
		}
		w, _ := writer.Create(file.Name)
		w.Write(body)
	}
	w, _ := writer.Create("EPUB/images/cover.png")
	w.Write(cover.Bytes())
	writer.Close()
	return out.Bytes()
}

func TestEPUBImportReadsBookDetailsAndCover(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "book.epub", bookEPUB(t))
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	m := d.Metadata
	if m.Publisher != "示例出版社" || m.Date != "2021-03-04" || m.ISBN != "9787111123456" || m.Language != "zh-CN" || m.Abstract != "第一段\n第二 段" {
		t.Fatalf("book details: %+v", m)
	}
	if m.Sources["publisher"] != "file" || m.Lookup != "" {
		t.Fatalf("sources: %+v", m)
	}
	response = request(t, s, "GET", "/pub/test-secret/"+d.ID+"/"+coverResource, nil)
	if response.Code != 200 || response.Header().Get("Content-Type") != "image/png" {
		t.Fatal(response.Code, response.Header())
	}
	if _, err := png.DecodeConfig(response.Body); err != nil {
		t.Fatal(err)
	}
	response = request(t, s, "PUT", "/api/documents/"+d.ID+"/cover", strings.NewReader("x"))
	if response.Code != 400 {
		t.Fatal("EPUB covers come from the book:", response.Code)
	}
}

func TestEPUBWithoutCoverHasNoCover(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "sample.epub", sample(t, "the-art-of-reading.epub"))
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	if response = request(t, s, "GET", "/pub/test-secret/"+d.ID+"/"+coverResource, nil); response.Code != 404 {
		t.Fatal(response.Code)
	}
}

func TestPDFCoverUpload(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "notes.pdf", sample(t, "reading-notes.pdf"))
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	cover := "/pub/test-secret/" + d.ID + "/" + coverResource
	if response = request(t, s, "GET", cover, nil); response.Code != 404 {
		t.Fatal("no cover before upload:", response.Code)
	}
	var png bytes.Buffer
	_ = pngEncode(&png)
	if response = request(t, s, "PUT", "/api/documents/"+d.ID+"/cover", &png); response.Code != 400 {
		t.Fatal("only JPEG is accepted:", response.Code)
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 30, 40)), nil); err != nil {
		t.Fatal(err)
	}
	data := jpg.Bytes()
	if response = request(t, s, "PUT", "/api/documents/"+d.ID+"/cover", bytes.NewReader(data)); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(t, s, "GET", cover, nil)
	if response.Code != 200 || response.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(response.Body.Bytes(), data) {
		t.Fatal(response.Code, response.Header())
	}
	if response = request(t, s, "PUT", "/api/documents/"+d.ID+"/cover", bytes.NewReader(make([]byte, maxCoverBytes+1))); response.Code != 413 {
		t.Fatal("oversized cover:", response.Code)
	}
}

func pngEncode(w io.Writer) error { return png.Encode(w, image.NewRGBA(image.Rect(0, 0, 2, 2))) }

func TestRatingValidation(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "sample.epub", sample(t, "the-art-of-reading.epub"))
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	path := "/api/documents/" + d.ID
	for _, bad := range []string{`6`, `-1`, `3.5`, `"4"`} {
		if w := request(t, s, "PATCH", path, strings.NewReader(`{"metadata":{"rating":`+bad+`}}`)); w.Code != 400 {
			t.Fatal(bad, w.Code)
		}
	}
	if w := request(t, s, "PATCH", path, strings.NewReader(`{"metadata":{"rating":4}}`)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got, _ := s.Store.Document(d.ID); got.Metadata.Rating != 4 {
		t.Fatal(got.Metadata.Rating)
	}
	request(t, s, "PATCH", path, strings.NewReader(`{"metadata":{"rating":0}}`))
	if got, _ := s.Store.Document(d.ID); got.Metadata.Rating != 0 {
		t.Fatal("rating not cleared:", got.Metadata.Rating)
	}
}

func TestFillEPUBBookMetadataKeepsManualFields(t *testing.T) {
	s := testServer(t)
	response := upload(t, s, "book.epub", bookEPUB(t))
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	// Simulate a book imported before details were read, with one manual edit.
	if _, err := s.Store.DB.Exec(`UPDATE documents SET metadata='{"publisher":"手填出版社","sources":{"publisher":"manual"}}' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.fillEPUBBookMetadata(); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Store.Document(d.ID)
	if got.Metadata.Publisher != "手填出版社" || got.Metadata.ISBN != "9787111123456" {
		t.Fatalf("%+v", got.Metadata)
	}
	// The migration runs once; later edits are not refilled.
	s.Store.DB.Exec(`UPDATE documents SET metadata='{}' WHERE id=?`, d.ID)
	if err := s.fillEPUBBookMetadata(); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Store.Document(d.ID); got.Metadata.ISBN != "" {
		t.Fatal("migration repeated")
	}
}

func TestBookISBN(t *testing.T) {
	for in, want := range map[string]string{"urn:isbn:0-306-40615-2": "0306406152", "ISBN:978 7 111 12345 6": "9787111123456", "urn:uuid:1234": "", "123456789X": "123456789X", "12345X7890": ""} {
		if got := bookISBN(in); got != want {
			t.Errorf("%s: %q", in, got)
		}
	}
}
